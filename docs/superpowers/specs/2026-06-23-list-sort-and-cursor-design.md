# ListEmails / ListSMS 排序稳定性与 cursor 分页设计

- 日期：2026-06-23
- 范围：修排序不稳定 + 可扩展排序 + offset/cursor 双模式分页
- 状态：已对齐，待 plan

## 背景

`ListEmails` / `ListSMS` 现状两个问题：

1. **排序不稳定**。DAL 只用 `ORDER BY created_at DESC`，无第二排序键。`created_at` 在批量发送、并发请求或同事务多记录场景下出现并列值，PG 对并列行的返回顺序未定义——不同 `OFFSET` 的查询可能以不同物理顺序返回，导致分页时**记录被永久跳过或重复返回**，无法拉完整。
2. **仅 offset 分页**。大表深翻页（如 `OFFSET 10000`）PG 需扫描并丢弃前 10000 行，性能差；高并发写入下 offset 还会受新数据影响偏移。

外加业务需求：列表查询要支持 ASC/DESC 方向切换（按时间正序拉取历史是高频场景）。

## 设计决策

| # | 决策点 | 选择 | 理由 |
|---|--------|------|------|
| 1 | 排序键 | `(sort_field, id)` 双键，id 为 tiebreaker | 雪花 id 单调递增，天然稳定；现有 `created_at` 单列索引即可双向扫描，无需新建复合索引 |
| 2 | 排序字段表达 | proto enum `SortField` | 项目强制 enum 风格；预留扩展位（未来 SENT_AT 等） |
| 3 | 排序方向表达 | proto enum `SortDirection`（UNSPECIFIED/ASC/DESC） | 同上；UNSPECIFIED 在 service/DAL 降级为 DESC，保持现状 |
| 4 | cursor 与 offset 关系 | 两个独立 RPC | 两种模式互斥，分开各自简洁；共用一个 RPC 会让字段语义混乱（page vs page_token 谁优先） |
| 5 | 是否直接用 `dbx.OffsetPaginate[T]` | 不用，typed chain 上手写 | `OffsetPaginate` 签名收 `*gorm.DB`，gorm gen typed chain `gorm.G[T](tx)` 不兼容；storage-service 也是手写 |
| 6 | cursor 包归属 | 本次内嵌 `internal/pagination/cursor.go`，参考 storage-service | 跨项目推 go-common 超出本次范围；后续 user/storage/message 三处统一时再上推 |
| 7 | cursor 模式是否默认返回 total | 不默认，`include_total` 字段控制 | 大表上 `COUNT(*)` 昂贵；offset 模式已带 total，cursor 用户多数不需要 |
| 8 | cursor token 格式 | `v1.<base64url(json)>`，json 内含 `{id, created_at}` | 与 storage-service 一致，便于未来合并；legacy bare-numeric 兼容可选（本服务首发无需） |
| 9 | response 扩展 | offset 响应加 `total_pages` + `has_more` | 对齐 `dbx.PageResult` 语义，UI 直接拿 |
| 10 | 兼容性 | 旧调用方零感知 | 不传新字段时行为 = DESC（现状）+ tiebreaker 修复；response 仅追加字段 |

## proto 改动

`api/proto/message/v1/message.proto`：

### 新增 enum

```proto
enum SortField {
  SORT_FIELD_UNSPECIFIED = 0;
  SORT_FIELD_CREATED_AT = 1;
}

enum SortDirection {
  SORT_DIRECTION_UNSPECIFIED = 0;
  SORT_DIRECTION_ASC = 1;
  SORT_DIRECTION_DESC = 2;
}
```

### ListEmails / ListSMS（offset 模式）

request 加排序字段：

```proto
message ListEmailsRequest {
  // ... 1-9 不变
  SortField sort_field = 10;
  SortDirection sort_direction = 11;
}
```

response 加 total 系列字段：

```proto
message ListEmailsResponse {
  repeated EmailRecord records = 1;
  int32 total = 2;
  int32 total_pages = 3;  // 新增
  bool has_more = 4;       // 新增
}
```

ListSMSRequest / ListSMSResponse 对称改动。

### 新增 cursor 模式 RPC

```proto
rpc ListEmailsByCursor(ListEmailsByCursorRequest) returns (ListEmailsByCursorResponse) { ... }

message ListEmailsByCursorRequest {
  // 过滤条件复用
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  // 排序
  SortField sort_field = 8;
  SortDirection sort_direction = 9;
  // cursor 分页
  int32 page_size = 10;
  string page_token = 11;
  bool include_total = 12;
}

message ListEmailsByCursorResponse {
  repeated EmailRecord records = 1;
  int32 total = 2;           // include_total=false 时为 0
  string next_page_token = 3; // 无下一页时为空
}
```

ListSMSByCursor 对称。

## DAL 改动

`internal/store/dal/email_record.go`、`sms_record.go`：

### Filter 拆分

共用 WHERE 过滤的 base struct + 两个分页模式独立方法：

```go
type EmailListFilter struct {
    Vendor, Scene, Status pb...
    Target, SenderID string
    StartTime, EndTime *time.Time
    SortField     pb.SortField
    SortDirection pb.SortDirection
}
```

> 不嵌入 `dbx.PageParams` / `dbx.Pagination`，避免两个 PageSize 语义打架。分页参数以独立参数传入。

### 新方法签名

```go
// offset 模式
func ListEmails(ctx, tx, f EmailListFilter, p dbx.PageParams) (*dbx.PageResult[models.MessageEmailRecord], error)

// cursor 模式
func ListEmailsByCursor(ctx, tx, f EmailListFilter, p dbx.Pagination, afterCreatedAt time.Time) ([]models.MessageEmailRecord, error)
```

返回 `dbx.PageResult[T]` 复用 dbx 已有结构（List/Total/TotalPages），DAL 不自己造。

### 内部 helper

```go
// 统一双键排序：始终带 ID tiebreaker
func applyEmailOrderBy(q, f) {
    switch f.SortField {
    default: // CREATED_AT + UNSPECIFIED
        dir := f.SortDirection
        if dir == ASC {
            q.Order(CreatedAt.Asc()).Order(ID.Asc())
        } else {
            q.Order(CreatedAt.Desc()).Order(ID.Desc())  // 默认 DESC
        }
    }
}

// cursor 推进：row-value 比较，DESC 时 < tuple，ASC 时 > tuple
func applyEmailCursor(q, f, afterID, afterCreatedAt) {
    if afterID == 0 { return q }
    if f.SortDirection == ASC {
        q.Where("created_at > ? OR (created_at = ? AND id > ?)", ...)
    } else {
        q.Where("created_at < ? OR (created_at = ? AND id < ?)", ...)
    }
}
```

> sort_field 当前只有 CREATED_AT，switch 仅为未来扩展（SENT_AT 等）预留骨架。`applyFileCursor` 模式直接复用 storage-service。

## Service 改动

`internal/service/message/email.go`、`sms.go`：

### ListEmails（offset）

```go
f := dal.EmailListFilter{
    SortField:     req.GetSortField(),
    SortDirection: req.GetSortDirection(),
    // 其他过滤字段
}
result, err := dal.ListEmails(ctx, s.db, f, dbx.PageParams{
    Page:     int(req.GetPage()),
    PageSize: int(req.GetPageSize()),
    Count:    true,
})
return &pb.ListEmailsResponse{
    Records:    toProtoEmailRecords(result.List),
    Total:      int32(result.Total),
    TotalPages: int32(result.TotalPages),
    HasMore:    req.GetPage() < int32(result.TotalPages),
}
```

### ListEmailsByCursor（cursor）

```go
f := dal.EmailListFilter{ ... }
pg := dbx.Pagination{
    PageSize: int(req.GetPageSize()),
}
var afterCreatedAt time.Time
if token := req.GetPageToken(); token != "" {
    cursor, err := pagination.DecodePageCursor(token)
    if err != nil { return nil, xcodes.ErrBadRequest.Wrap(err) }
    pg.AfterID = cursor.ID
    afterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
}
records, total, err := dal.ListEmailsByCursor(ctx, s.db, f, pg, afterCreatedAt, req.GetIncludeTotal())
records, hasNext := dbx.TrimPage(records, pg.PageSize)

var nextToken string
if hasNext {
    last := records[len(records)-1]
    nextToken = pagination.EncodePageCursor(pagination.PageCursor{
        ID:        last.ID,
        CreatedAt: pagination.CursorFromCreatedAt(last.CreatedAt),
    })
}
return &pb.ListEmailsByCursorResponse{ ... }
```

## 新增 internal/pagination/cursor.go

参考 storage-service `internal/utils/pagination/cursor.go`，message-service 版本只含 `ID` + `CreatedAt` 字段（不需要 Filename 等）：

```go
type PageCursor struct {
    ID        int64  `json:"id"`
    CreatedAt string `json:"ca,omitempty"` // RFC3339Nano
}

const cursorVersion = "v1"

func EncodePageCursor(c PageCursor) string { ... }
func DecodePageCursor(token string) (PageCursor, error) { ... }
func CursorFromCreatedAt(t time.Time) string { ... }
func CursorToCreatedAt(s string) time.Time { ... }
```

## 改动文件清单

| 文件 | 改动 |
|------|------|
| `api/proto/message/v1/message.proto` | 加 2 enum + 改 2 request/response + 加 2 RPC + 加 2 cursor request/response |
| `gen/message/v1/*.pb.go`、`*pb.gw.go`、`*_grpc.pb.go` | `make proto` 自动 |
| `internal/store/dal/email_record.go`、`sms_record.go` | 重构 ListEmails 返回 dbx.PageResult；新增 ListEmailsByCursor；抽 applyOrderBy/applyCursor helper |
| `internal/store/dal/email_record_test.go`、`sms_record_test.go` | 加 tiebreaker / ASC / cursor 测试 |
| `internal/service/message/email.go`、`sms.go` | ListEmails 传 Sort + 填新 response 字段；实现 ListEmailsByCursor |
| `internal/service/message/email_test.go`、`sms_test.go` | 加 cursor 流程测试 |
| `internal/pagination/cursor.go`（新增） | 内嵌 storage-service cursor 包精简版 |
| `internal/pagination/cursor_test.go`（新增） | encode/decode 往返 |

## 测试策略

| 场景 | 验证点 |
|------|--------|
| tiebreaker（offset） | 构造 N 条 `created_at` 完全相同的记录，分页拉取，断言无重复无遗漏，总数对得上 |
| ASC / DESC 双向（offset） | 同一批数据两个方向拉，验证顺序对称 |
| cursor 基本流程 | 首页不带 token → 取 next_page_token → 下一页内容正确 |
| cursor tiebreaker | 同上 tiebreaker 场景用 cursor 拉完，断言完整 |
| cursor include_total=false | response.total == 0，且不触发 COUNT(*) |
| cursor 反向 | ASC 拉一遍 + DESC 拉一遍，结果集一致 |
| 现有兼容 | 不传 sort_field/sort_direction 时，行为 = DESC（与改造前一致）+ tiebreaker 修复 |

集成测试已用 `dbx.SetupTestDB` 真库，直接 INSERT 可控时间戳。

## 兼容性

- 旧 ListEmails/ListSMS 调用方：不传新字段 → 行为不变（DESC + tiebreaker 修复）；response 多字段是追加，向后兼容
- 新 ListEmailsByCursor/ListSMSByCursor：新 RPC，无破坏
- DB schema：无改动，无需迁移

## 非目标

- 不直接用 `dbx.OffsetPaginate[T]`（typed chain 不兼容）
- cursor 包暂不上推 go-common（跨项目改动，留给后续统一）
- 不引入 SENT_AT 等新排序字段（switch 骨架预留即可）
- 不做 cursor token 的 legacy bare-numeric 兼容（首发无历史负担）

## 关联

**上一版设计：** 无（首发）

**相关代码：**
- `internal/store/dal/email_record.go:113` — 现有 ListEmails Order 实现（待修复）
- `internal/store/dal/sms_record.go:107` — 同上
- `github.com/servekit/go-common/dbx/pagination.go` — dbx 分页封装
- `storage-service/internal/utils/pagination/cursor.go` — cursor 包参考源
- `storage-service/internal/store/dal/file.go:127-149` — cursor DAL 模式参考
