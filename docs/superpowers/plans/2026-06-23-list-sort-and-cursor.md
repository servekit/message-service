# List 排序稳定性与 cursor 分页实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 ListEmails/ListSMS 排序不稳定（加 ID tiebreaker），加 SortField/SortDirection 双 enum 排序选项，并新增 cursor 模式 RPC（ListEmailsByCursor / ListSMSByCursor）支持大数据量稳定分页。

**Architecture:** proto 加 2 个 enum + 扩展 offset 模式 request/response 字段 + 新增 2 个 cursor RPC；DAL 用 dbx.PageResult / dbx.Pagination，typed chain 上手写 ORDER BY 与 cursor 推进；内嵌 `internal/pagination/cursor.go`（参考 storage-service）负责 opaque token 编解码。

**Tech Stack:** Go 1.22+、gorm gen（typed chain）、PostgreSQL（dbx.SetupTestDB testcontainer）、buf + grpc-gateway、protobuf + protovalidate。

**Spec:** `docs/superpowers/specs/2026-06-23-list-sort-and-cursor-design.md`

---

## File Structure

| 文件 | 角色 | 改动 |
|------|------|------|
| `api/proto/message/v1/message.proto` | RPC 与消息定义 | 加 `SortField` / `SortDirection` enum，扩展 ListEmails/ListSMS request/response，新增 ListEmailsByCursor/ListSMSByCursor RPC 及对应 request/response |
| `gen/message/v1/*.pb.go` 等 | buf 自动生成 | `make proto` 重生成 |
| `internal/pagination/cursor.go`（新增） | opaque page token 编解码 | 复制 storage-service cursor 包精简版（仅 ID + CreatedAt） |
| `internal/pagination/cursor_test.go`（新增） | cursor 包测试 | encode/decode 往返 + 边界 |
| `internal/store/dal/email_record.go` | Email DAL | EmailListFilter 加 Sort 字段；ListEmailRecords 返回 dbx.PageResult；新增 ListEmailsByCursor；抽 applyEmailOrderBy / applyEmailCursor helper |
| `internal/store/dal/email_record_test.go` | Email DAL 测试 | 改旧测试匹配新签名；加 tiebreaker、ASC/DESC、cursor 流程测试 |
| `internal/store/dal/sms_record.go` | SMS DAL | 与 Email 对称 |
| `internal/store/dal/sms_record_test.go` | SMS DAL 测试 | 与 Email 对称 |
| `internal/service/message/email.go` | Email service | ListEmails 传 Sort + 填新 response 字段；新增 ListEmailsByCursor RPC 实现 |
| `internal/service/message/email_test.go` | Email service 测试 | 加 Sort / cursor 测试 |
| `internal/service/message/sms.go` | SMS service | 与 Email 对称 |
| `internal/service/message/sms_test.go` | SMS service 测试 | 与 Email 对称 |

---

## Phase 1: Proto 与代码生成

### Task 1: proto 加 SortField / SortDirection enum，扩展 ListEmails/ListSMS request/response，加 ListEmailsByCursor/ListSMSByCursor RPC

**Files:**
- Modify: `api/proto/message/v1/message.proto`

- [ ] **Step 1: 在 SmsScene enum 之后追加 SortField 和 SortDirection enum**

打开 `api/proto/message/v1/message.proto`，定位到 `enum SmsScene { ... }` 块（约第 56-66 行），在其结束 `}` 之后追加：

```proto
// SortField selects the column used to order List responses. Reserved for
// future expansion (SENT_AT, etc.); current implementors must accept
// unknown values without erroring.
enum SortField {
  SORT_FIELD_UNSPECIFIED = 0;
  SORT_FIELD_CREATED_AT = 1;
}

// SortDirection selects ascending or descending order. UNSPECIFIED must be
// treated as DESC by the service layer (preserves current behavior).
enum SortDirection {
  SORT_DIRECTION_UNSPECIFIED = 0;
  SORT_DIRECTION_ASC = 1;
  SORT_DIRECTION_DESC = 2;
}
```

- [ ] **Step 2: 扩展 ListEmailsRequest 加 SortField/SortDirection**

将 `message ListEmailsRequest { ... }` 改为：

```proto
message ListEmailsRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  int32 page = 8;
  int32 page_size = 9;
  SortField sort_field = 10;
  SortDirection sort_direction = 11;
}
```

- [ ] **Step 3: 扩展 ListEmailsResponse 加 total_pages 和 has_more**

将 `message ListEmailsResponse { ... }` 改为：

```proto
message ListEmailsResponse {
  repeated EmailRecord records = 1;
  int32 total = 2;
  int32 total_pages = 3;
  bool has_more = 4;
}
```

- [ ] **Step 4: 对 ListSMSRequest / ListSMSResponse 做对称改动**

```proto
message ListSMSRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  int32 page = 8;
  int32 page_size = 9;
  SortField sort_field = 10;
  SortDirection sort_direction = 11;
}

message ListSMSResponse {
  repeated SMSRecord records = 1;
  int32 total = 2;
  int32 total_pages = 3;
  bool has_more = 4;
}
```

- [ ] **Step 5: 加 ListEmailsByCursor 和 ListSMSByCursor RPC**

在 `service MessageService { ... }` 块内、`rpc ListEmails` 之后追加：

```proto
  // ListEmailsByCursor returns a cursor-paginated list of email records.
  // Prefer this over ListEmails for large datasets or streaming scans;
  // it skips COUNT(*) by default and stays stable under concurrent writes.
  rpc ListEmailsByCursor(ListEmailsByCursorRequest) returns (ListEmailsByCursorResponse) {
    option (google.api.http) = {get: "/v1/emails:cursor"};
  }
```

在 `rpc ListSMS` 之后追加：

```proto
  // ListSMSByCursor is the cursor-paginated counterpart of ListSMS.
  rpc ListSMSByCursor(ListSMSByCursorRequest) returns (ListSMSByCursorResponse) {
    option (google.api.http) = {get: "/v1/sms:cursor"};
  }
```

- [ ] **Step 6: 加 cursor 模式的 request/response message**

在 `message ListSMSResponse { ... }` 块之后追加：

```proto
// ListEmailsByCursorRequest filters and cursor-paginates a ListEmailsByCursor
// query. page_token is opaque — obtain it from the previous response's
// next_page_token; empty means first page.
message ListEmailsByCursorRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  SortField sort_field = 8;
  SortDirection sort_direction = 9;
  int32 page_size = 10;
  string page_token = 11;
  // include_total controls whether COUNT(*) runs. Defaults to false: cursor
  // users usually don't need the total, and COUNT(*) is expensive on large
  // tables.
  bool include_total = 12;
}

message ListEmailsByCursorResponse {
  repeated EmailRecord records = 1;
  // total is 0 when include_total = false. Otherwise reflects the count
  // matching the filter (ignoring cursor position).
  int32 total = 2;
  // next_page_token is empty when there is no next page.
  string next_page_token = 3;
}

// ListSMSByCursorRequest filters and cursor-paginates a ListSMSByCursor query.
message ListSMSByCursorRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  SortField sort_field = 8;
  SortDirection sort_direction = 9;
  int32 page_size = 10;
  string page_token = 11;
  bool include_total = 12;
}

message ListSMSByCursorResponse {
  repeated SMSRecord records = 1;
  int32 total = 2;
  string next_page_token = 3;
}
```

- [ ] **Step 7: 校验 proto 语法**

Run: `buf lint api/proto`
Expected: 无报错。若报 `SortField` 重复之类错误，说明 enum 名拼错，回查 step 1。

- [ ] **Step 8: Commit**

```bash
git add api/proto/message/v1/message.proto
git commit -m "feat(proto): add SortField/SortDirection enums and cursor-mode List RPCs"
```

---

### Task 2: 重新生成 protobuf 代码

**Files:**
- Modify (auto): `gen/message/v1/message.pb.go`、`gen/message/v1/message.pb.gw.go`、`gen/message/v1/message_grpc.pb.go`

- [ ] **Step 1: 运行 buf generate**

Run: `make proto`
Expected: 命令成功退出，`gen/message/v1/message.pb.go` 包含 `SortField`、`SortDirection`、`ListEmailsByCursorRequest` 等新类型。

- [ ] **Step 2: 校验生成产物**

Run: `grep -c "ListEmailsByCursor" gen/message/v1/message.pb.go gen/message/v1/message_grpc.pb.go`
Expected: 两个文件都至少返回 1。

Run: `go build ./...`
Expected: 编译失败，原因：`Service` 类型尚未实现 `ListEmailsByCursor` / `ListSMSByCursor` 方法（接口未满足）。这是预期，后续 Task 实现后会通过。

- [ ] **Step 3: Commit**

```bash
git add gen/
git commit -m "chore(proto): regenerate after SortField/Direction and cursor RPCs"
```

---

## Phase 2: 内嵌 pagination cursor 包

### Task 3: 新建 `internal/pagination/cursor.go` 及测试（TDD）

**Files:**
- Create: `internal/pagination/cursor.go`
- Create: `internal/pagination/cursor_test.go`

- [ ] **Step 1: 先写测试 `cursor_test.go`**

```go
package pagination

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	c := PageCursor{
		ID:        12345,
		CreatedAt: "2026-06-23T10:00:00.123456789Z",
	}
	token, err := EncodePageCursor(c), error(nil)
	_ = err
	decoded, err := DecodePageCursor(token)
	require.NoError(t, err)
	assert.Equal(t, c.ID, decoded.ID)
	assert.Equal(t, c.CreatedAt, decoded.CreatedAt)
}

func TestDecodePageCursor_Empty(t *testing.T) {
	_, err := DecodePageCursor("")
	require.Error(t, err)
}

func TestDecodePageCursor_Malformed(t *testing.T) {
	_, err := DecodePageCursor("v1.not-base64-!!!")
	require.Error(t, err)
}

func TestCursorFromCreatedAt_ZeroTime(t *testing.T) {
	assert.Equal(t, "", CursorFromCreatedAt(time.Time{}))
}

func TestCursorRoundTrip_CreatedAt(t *testing.T) {
	original := time.Date(2026, 6, 23, 10, 0, 0, 123456789, time.UTC)
	s := CursorFromCreatedAt(original)
	assert.NotEmpty(t, s)
	back := CursorToCreatedAt(s)
	assert.True(t, original.Equal(back))
}

func TestCursorToCreatedAt_Empty(t *testing.T) {
	assert.True(t, CursorToCreatedAt("").IsZero())
}
```

- [ ] **Step 2: 运行测试，验证失败**

Run: `go test ./internal/pagination/`
Expected: FAIL，原因：`package pagination is not found` 或类型未定义。

- [ ] **Step 3: 实现 `cursor.go`**

```go
// Package pagination encodes and decodes opaque page cursors for the
// ListEmailsByCursor / ListSMSByCursor RPCs.
//
// The token carries the last row's (id, created_at) so that ORDER BY
// created_at pages without dropping or duplicating rows under tied
// timestamps. The on-wire format is `v1.<base64url(json)>`; the version
// prefix lets future formats coexist with already-issued tokens.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PageCursor is the in-memory shape of a page token.
type PageCursor struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"ca,omitempty"` // RFC3339Nano
}

// cursorVersion is the prefix tag for the current encoding.
const cursorVersion = "v1"

// EncodePageCursor serializes a cursor into an opaque token.
func EncodePageCursor(c PageCursor) string {
	payload, err := json.Marshal(c)
	if err != nil {
		// PageCursor only holds primitive types; json.Marshal cannot fail
		// in practice. Fall back to the smallest valid token rather than
		// surfacing an error.
		payload = []byte(`{}`)
	}
	return cursorVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
}

// DecodePageCursor parses a token produced by EncodePageCursor.
func DecodePageCursor(token string) (PageCursor, error) {
	if token == "" {
		return PageCursor{}, fmt.Errorf("empty page token")
	}
	if !strings.HasPrefix(token, cursorVersion+".") {
		return PageCursor{}, fmt.Errorf("malformed page token")
	}
	payload := strings.TrimPrefix(token, cursorVersion+".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return PageCursor{}, fmt.Errorf("decode page token base64: %w", err)
	}
	var c PageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return PageCursor{}, fmt.Errorf("decode page token json: %w", err)
	}
	return c, nil
}

// CursorFromCreatedAt formats a time.Time into the RFC3339Nano string
// carried by PageCursor.CreatedAt. Returns "" for the zero time so the
// field is omitted from the encoded token.
func CursorFromCreatedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// CursorToCreatedAt reverses CursorFromCreatedAt.
func CursorToCreatedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
```

- [ ] **Step 4: 运行测试，验证通过**

Run: `go test ./internal/pagination/ -v`
Expected: 所有测试 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/pagination/
git commit -m "feat(pagination): add internal cursor encode/decode package"
```

---

## Phase 3: Email DAL 改造

### Task 4: EmailListFilter 加 Sort 字段，抽 applyEmailOrderBy（修 tiebreaker），ListEmailRecords 返回 dbx.PageResult

**Files:**
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/email_record_test.go`

- [ ] **Step 1: 改 EmailListFilter struct（去掉 Page/PageSize，加 SortField/SortDirection）**

在 `internal/store/dal/email_record.go` 中将：

```go
type EmailListFilter struct {
	Vendor    pb.EmailVendor
	Scene     pb.EmailScene
	Status    pb.MessageStatus
	Target    string
	SenderID  string
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}
```

改为：

```go
// EmailListFilter holds parameters for listing email records. Page / PageSize
// live on dbx.PageParams (offset mode) or dbx.Pagination (cursor mode) and
// are passed to ListEmailRecords / ListEmailsByCursor separately — keeping
// the two modes' pagination fields separate avoids the "which PageSize wins"
// ambiguity.
type EmailListFilter struct {
	Vendor       pb.EmailVendor
	Scene        pb.EmailScene
	Status       pb.MessageStatus
	Target       string
	SenderID     string
	StartTime    *time.Time
	EndTime      *time.Time
	SortField    pb.SortField
	SortDirection pb.SortDirection
}
```

- [ ] **Step 2: 重写 ListEmailRecords 签名和实现**

将原 `ListEmailRecords` 函数整体替换为：

```go
// ListEmailRecords returns a page of email records matching filter, along
// with the total count and derived total pages. Page and PageSize come from
// dbx.PageParams; PageSize is clamped to dbx.MaxPageSize.
//
// Ordering is always (sort_field, id) — id is the tiebreaker that keeps
// pagination stable when sort_field has tied values (e.g. multiple records
// sharing a created_at timestamp).
func ListEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter, p dbx.PageParams) (*dbx.PageResult[models.MessageEmailRecord], error) {
	p = p.Normalize()

	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	var total int64
	if p.Count {
		count, err := q.Count(ctx, generated.MessageEmailRecord.ID.Column().Name)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		total = count
	}

	q = applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	q = applyEmailOrderBy(q, filter)

	results, err := q.
		Offset(int((p.Page - 1) * p.PageSize)).
		Limit(p.PageSize).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageEmailRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}

	var totalPages int
	if p.Count && p.PageSize > 0 {
		totalPages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}

	return &dbx.PageResult[models.MessageEmailRecord]{
		List:       records,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}
```

- [ ] **Step 3: 在文件底部 helper 区追加 applyEmailOrderBy**

```go
// applyEmailOrderBy attaches an (ORDER BY sort_field [dir], id [dir]) clause
// to q. The id column is always present as a tiebreaker so pagination stays
// stable under tied sort values. UNSPECIFIED sort_field/direction fall back
// to (created_at DESC, id DESC) — the historical default.
func applyEmailOrderBy(q gorm.ChainInterface[models.MessageEmailRecord], f EmailListFilter) gorm.ChainInterface[models.MessageEmailRecord] {
	asc := f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC
	switch f.SortField {
	default:
		if asc {
			return q.Order(generated.MessageEmailRecord.CreatedAt.Asc()).Order(generated.MessageEmailRecord.ID.Asc())
		}
		return q.Order(generated.MessageEmailRecord.CreatedAt.Desc()).Order(generated.MessageEmailRecord.ID.Desc())
	}
}
```

> 注：当前 SortField 只有 `CREATED_AT` 一个非 UNSPECIFIED 值，但 switch 骨架已为未来扩展（SENT_AT 等）预留。所有路径都走到 default 分支，行为一致。

- [ ] **Step 4: 更新现有测试以匹配新签名**

在 `internal/store/dal/email_record_test.go` 中：

- `TestListEmailRecords_ByScene`：把

  ```go
  records, total, err := ListEmailRecords(ctx, db, EmailListFilter{
      Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
      Page:     1,
      PageSize: 10,
  })
  require.NoError(t, err)
  assert.Equal(t, int64(1), total)
  assert.Len(t, records, 1)
  assert.Equal(t, "a@b.com", records[0].Target)
  ```

  改为：

  ```go
  result, err := ListEmailRecords(ctx, db, EmailListFilter{
      Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
  }, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
  require.NoError(t, err)
  assert.Equal(t, int64(1), result.Total)
  require.Len(t, result.List, 1)
  assert.Equal(t, "a@b.com", result.List[0].Target)
  ```

- `TestListEmailRecords_ByVendor`：同样把 `records, total, err := ListEmailRecords(...)` 改为 `result, err := ListEmailRecords(...)`，把 `total` 断言改为 `result.Total`，`records` 断言改为 `result.List`。原 Page/PageSize 字段从 filter 中删除，挪到 `dbx.PageParams{Page: 1, PageSize: 10, Count: true}` 参数。

- `TestListEmailRecords_PageSizeClamped`：同样改签名，原 `records` 引用改为 `result.List`：

  ```go
  result, err := ListEmailRecords(ctx, db, EmailListFilter{}, dbx.PageParams{
      Page:     1,
      PageSize: 1000,
      Count:    true,
  })
  require.NoError(t, err)
  assert.LessOrEqual(t, len(result.List), 100)
  assert.Len(t, result.List, 5)
  ```

- [ ] **Step 5: 运行测试，验证现有用例仍通过**

Run: `go test ./internal/store/dal/ -run TestListEmailRecords`
Expected: 3 个测试 PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/store/dal/email_record.go internal/store/dal/email_record_test.go
git commit -m "refactor(dal/email): return dbx.PageResult and add SortField/Direction with id tiebreaker"
```

---

### Task 5: 加 tiebreaker 集成测试（同 created_at 分页完整拉取）

**Files:**
- Modify: `internal/store/dal/email_record_test.go`

- [ ] **Step 1: 在 `newTestEmailRecord` 下方加一个 helper**

```go
// newTestEmailRecordAt is newTestEmailRecord with an explicit CreatedAt,
// so multiple records can share the same timestamp to exercise the
// (created_at, id) tiebreaker.
func newTestEmailRecordAt(id int64, createdAt time.Time, target string) *models.MessageEmailRecord {
	r := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		target,
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)
	r.ID = id
	r.CreatedAt = createdAt
	r.UpdatedAt = createdAt
	return r
}
```

- [ ] **Step 2: 写失败测试 `TestListEmailRecords_Tiebreaker_StablePagination`**

在文件末尾追加：

```go
// TestListEmailRecords_Tiebreaker_StablePagination verifies that rows sharing
// the same created_at are paged without duplication or loss. Pre-fix the
// query only had ORDER BY created_at; under tied values PG returns rows in
// an undefined order, so different OFFSETs could skip or repeat rows.
func TestListEmailRecords_Tiebreaker_StablePagination(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	// 5 records all sharing the same created_at.
	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, ts, fmt.Sprintf("u%d@x.com", i))))
	}

	// Page through all 5 records with page_size = 2.
	seen := make(map[int64]struct{})
	for page := 1; page <= 5; page++ {
		result, err := ListEmailRecords(ctx, db, EmailListFilter{}, dbx.PageParams{
			Page:     page,
			PageSize: 2,
			Count:    true,
		})
		require.NoError(t, err)
		if len(result.List) == 0 {
			break
		}
		for _, r := range result.List {
			// Detect duplication across pages.
			_, ok := seen[r.ID]
			assert.False(t, ok, "id %d appeared on multiple pages", r.ID)
			seen[r.ID] = struct{}{}
		}
	}

	// Every record was seen exactly once.
	assert.Len(t, seen, 5, "expected to page through all 5 records without loss or duplication")
}
```

并在文件顶部 `import` 块追加 `"fmt"`（如果还没有）。

- [ ] **Step 3: 运行测试，验证通过**

Run: `go test ./internal/store/dal/ -run TestListEmailRecords_Tiebreaker -v`
Expected: PASS。如果 FAIL（重复或丢失 id），说明 tiebreaker 没生效，回查 Task 4 step 3 的 `applyEmailOrderBy`。

- [ ] **Step 4: Commit**

```bash
git add internal/store/dal/email_record_test.go
git commit -m "test(dal/email): cover tied-timestamp stable pagination"
```

---

### Task 6: 加 ASC 方向测试

**Files:**
- Modify: `internal/store/dal/email_record_test.go`

- [ ] **Step 1: 写测试 `TestListEmailRecords_ASC_Ordering`**

在文件末尾追加：

```go
// TestListEmailRecords_ASC_Ordering verifies that SortDirection_ASC returns
// rows oldest-first, mirroring the default DESC behavior.
func TestListEmailRecords_ASC_Ordering(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("u%d@x.com", i))))
	}

	result, err := ListEmailRecords(ctx, db, EmailListFilter{
		SortDirection: pb.SortDirection_SORT_DIRECTION_ASC,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, result.List, 3)

	// Oldest first: id=1 has the earliest created_at.
	assert.Equal(t, int64(1), result.List[0].ID)
	assert.Equal(t, int64(2), result.List[1].ID)
	assert.Equal(t, int64(3), result.List[2].ID)
}
```

- [ ] **Step 2: 运行测试，验证通过**

Run: `go test ./internal/store/dal/ -run TestListEmailRecords_ASC -v`
Expected: PASS。

- [ ] **Step 3: Commit**

```bash
git add internal/store/dal/email_record_test.go
git commit -m "test(dal/email): cover ASC sort direction"
```

---

### Task 7: 实现 ListEmailsByCursor + applyEmailCursor helper（TDD）

**Files:**
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/email_record_test.go`

- [ ] **Step 1: 写失败测试 `TestListEmailsByCursor_FullSweep`**

在 `email_record_test.go` 末尾追加：

```go
// TestListEmailsByCursor_FullSweep pages through every record with cursor
// mode, including the tiebreaker case (same created_at), and verifies no
// loss or duplication.
func TestListEmailsByCursor_FullSweep(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, ts, fmt.Sprintf("u%d@x.com", i))))
	}

	seen := make(map[int64]struct{})
	pg := dbx.Pagination{PageSize: 2}
	var afterCreatedAt time.Time
	for i := 0; i < 10; i++ { // upper bound to prevent infinite loop on bug
		records, err := ListEmailsByCursor(ctx, db, EmailListFilter{}, pg, afterCreatedAt)
		require.NoError(t, err)
		if len(records) == 0 {
			break
		}
		for _, r := range records {
			_, ok := seen[r.ID]
			assert.False(t, ok, "id %d duplicated across cursor pages", r.ID)
			seen[r.ID] = struct{}{}
		}
		last := records[len(records)-1]
		pg.AfterID = last.ID
		afterCreatedAt = last.CreatedAt
		if len(records) < pg.PageSize {
			break
		}
	}
	assert.Len(t, seen, 5, "cursor sweep should cover all 5 records")
}
```

- [ ] **Step 2: 运行测试，验证失败**

Run: `go test ./internal/store/dal/ -run TestListEmailsByCursor_FullSweep`
Expected: FAIL — `ListEmailsByCursor` 未定义。

- [ ] **Step 3: 加 `ListEmailsByCursor` 和 `applyEmailCursor`**

在 `email_record.go` 中，紧跟 `ListEmailRecords` 之后追加：

```go
// ListEmailsByCursor returns the next page of email records past the
// (afterCreatedAt, afterID) cursor. Caller passes pageSize on pg and the
// previous page's last (id, created_at) on subsequent calls. Pass a zero
// pg.AfterID + zero afterCreatedAt for the first page.
//
// The returned slice may be up to pageSize+1 long; use dbx.TrimPage to
// detect "has next page".
func ListEmailsByCursor(ctx context.Context, tx *gorm.DB, filter EmailListFilter, pg dbx.Pagination, afterCreatedAt time.Time) ([]*models.MessageEmailRecord, error) {
	pg = pg.Normalize()

	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	q = applyEmailOrderBy(q, filter)
	q = applyEmailCursor(q, filter, pg.AfterID, afterCreatedAt)

	results, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageEmailRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, nil
}

// applyEmailCursor advances q past the (afterCreatedAt, afterID) tuple from
// the previous page's last row. Returns q unchanged when afterID == 0
// (first page).
//
// Callers MUST pass both afterID and afterCreatedAt; passing only afterID
// would degrade to a bare `id < ?` cursor, which drops rows whose id
// ordering disagrees with the sort column under tied created_at values.
func applyEmailCursor(q gorm.ChainInterface[models.MessageEmailRecord], f EmailListFilter, afterID int64, afterCreatedAt time.Time) gorm.ChainInterface[models.MessageEmailRecord] {
	if afterID == 0 {
		return q
	}
	if f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC {
		return q.Where("created_at > ? OR (created_at = ? AND id > ?)", afterCreatedAt, afterCreatedAt, afterID)
	}
	return q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreatedAt, afterCreatedAt, afterID)
}
```

- [ ] **Step 4: 运行测试，验证通过**

Run: `go test ./internal/store/dal/ -run TestListEmailsByCursor -v`
Expected: PASS。

- [ ] **Step 5: 加 `TestListEmailsByCursor_ASC` 反向测试**

```go
func TestListEmailsByCursor_ASC(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("u%d@x.com", i))))
	}

	filter := EmailListFilter{SortDirection: pb.SortDirection_SORT_DIRECTION_ASC}
	pg := dbx.Pagination{PageSize: 10}
	records, err := ListEmailsByCursor(ctx, db, filter, pg, time.Time{})
	require.NoError(t, err)
	require.Len(t, records, 3)
	// Oldest first.
	assert.Equal(t, int64(1), records[0].ID)
	assert.Equal(t, int64(3), records[2].ID)
}
```

Run: `go test ./internal/store/dal/ -run TestListEmailsByCursor_ASC -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/store/dal/email_record.go internal/store/dal/email_record_test.go
git commit -m "feat(dal/email): add ListEmailsByCursor with row-value cursor advance"
```

---

## Phase 4: SMS DAL 改造（与 Email 对称）

### Task 8: SmsListFilter 加 Sort 字段，抽 applySmsOrderBy，ListSMSRecords 返回 dbx.PageResult

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`

- [ ] **Step 1: 改 SmsListFilter struct**

将：

```go
type SmsListFilter struct {
	Vendor    pb.SmsVendor
	Scene     pb.SmsScene
	Status    pb.MessageStatus
	Target    string
	SenderID  string
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}
```

改为：

```go
// SmsListFilter mirrors EmailListFilter for SMS records. See its doc comment
// for the rationale on splitting pagination fields out of the filter.
type SmsListFilter struct {
	Vendor        pb.SmsVendor
	Scene         pb.SmsScene
	Status        pb.MessageStatus
	Target        string
	SenderID      string
	StartTime     *time.Time
	EndTime       *time.Time
	SortField     pb.SortField
	SortDirection pb.SortDirection
}
```

- [ ] **Step 2: 重写 ListSMSRecords**

将整个 `ListSMSRecords` 函数替换为：

```go
// ListSMSRecords returns a page of SMS records matching filter, with total
// count and derived total pages. See ListEmailRecords for ordering rationale.
func ListSMSRecords(ctx context.Context, tx *gorm.DB, filter SmsListFilter, p dbx.PageParams) (*dbx.PageResult[models.MessageSMSRecord], error) {
	p = p.Normalize()

	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	var total int64
	if p.Count {
		count, err := q.Count(ctx, generated.MessageSMSRecord.ID.Column().Name)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		total = count
	}

	q = applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	q = applySmsOrderBy(q, filter)

	results, err := q.
		Offset(int((p.Page - 1) * p.PageSize)).
		Limit(p.PageSize).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageSMSRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}

	var totalPages int
	if p.Count && p.PageSize > 0 {
		totalPages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}

	return &dbx.PageResult[models.MessageSMSRecord]{
		List:       records,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}
```

- [ ] **Step 3: 追加 applySmsOrderBy helper**

```go
// applySmsOrderBy mirrors applyEmailOrderBy for SMS records.
func applySmsOrderBy(q gorm.ChainInterface[models.MessageSMSRecord], f SmsListFilter) gorm.ChainInterface[models.MessageSMSRecord] {
	asc := f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC
	switch f.SortField {
	default:
		if asc {
			return q.Order(generated.MessageSMSRecord.CreatedAt.Asc()).Order(generated.MessageSMSRecord.ID.Asc())
		}
		return q.Order(generated.MessageSMSRecord.CreatedAt.Desc()).Order(generated.MessageSMSRecord.ID.Desc())
	}
}
```

- [ ] **Step 4: 更新现有 SMS 测试以匹配新签名**

在 `internal/store/dal/sms_record_test.go` 中：

- 找到所有 `records, total, err := ListSMSRecords(...)` 调用（参考 `TestListSMSRecords_ByScene`），改为 `result, err := ListSMSRecords(...)`：
  - 把 `Page` / `PageSize` 字段从 `SmsListFilter{}` 中移除
  - 添加第四个参数 `dbx.PageParams{Page: 1, PageSize: 10, Count: true}`（PageSize 按各测试的实际值）
  - 把 `total` 断言改为 `result.Total`
  - 把 `records` 断言改为 `result.List`

参考 Task 4 step 4 中 Email 测试的同样改法。

- [ ] **Step 5: 运行测试，验证通过**

Run: `go test ./internal/store/dal/ -run TestListSMSRecords`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go
git commit -m "refactor(dal/sms): return dbx.PageResult and add SortField/Direction with id tiebreaker"
```

---

### Task 9: SMS tiebreaker + ASC 测试

**Files:**
- Modify: `internal/store/dal/sms_record_test.go`

- [ ] **Step 1: 加 `newTestSMSRecordAt` helper**

参考现有 `newTestSMSRecord`（如不存在，按 `newTestEmailRecord` 同构形式编写），追加一个带显式 ID + CreatedAt 的版本：

```go
func newTestSMSRecordAt(id int64, createdAt time.Time, target string) *models.MessageSMSRecord {
	r := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		target,
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)
	r.ID = id
	r.CreatedAt = createdAt
	r.UpdatedAt = createdAt
	return r
}
```

> 如 `newTestSMSRecord` 的参数顺序与上述不同，按现有签名调整。

- [ ] **Step 2: 加 tiebreaker 测试 `TestListSMSRecords_Tiebreaker_StablePagination`**

按 Task 5 step 2 中 Email 版本同构改写：5 条记录同 created_at，page_size=2 翻页，断言全部见且不重复。

- [ ] **Step 3: 加 ASC 测试 `TestListSMSRecords_ASC_Ordering`**

按 Task 6 step 1 中 Email 版本同构改写：3 条不同 created_at，断言 ASC 顺序为 id=1,2,3。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/store/dal/ -run "TestListSMSRecords_(Tiebreaker|ASC)" -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/store/dal/sms_record_test.go
git commit -m "test(dal/sms): cover tied-timestamp pagination and ASC direction"
```

---

### Task 10: 实现 ListSMSByCursor + applySmsCursor（TDD）

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`

- [ ] **Step 1: 写失败测试 `TestListSMSByCursor_FullSweep` 和 `TestListSMSByCursor_ASC`**

按 Task 7 step 1 / step 5 中 Email 版本同构改写，类型改为 `models.MessageSMSRecord`、`SmsListFilter`、`ListSMSByCursor`、`newTestSMSRecordAt`。

- [ ] **Step 2: 运行测试，验证失败**

Run: `go test ./internal/store/dal/ -run TestListSMSByCursor`
Expected: FAIL — `ListSMSByCursor` 未定义。

- [ ] **Step 3: 加 `ListSMSByCursor` 和 `applySmsCursor`**

在 `sms_record.go` 中，紧跟 `ListSMSRecords` 之后追加（按 Task 7 step 3 中 Email 版本同构改写）：

```go
// ListSMSByCursor returns the next page of SMS records past the
// (afterCreatedAt, afterID) cursor. See ListEmailsByCursor for cursor
// semantics.
func ListSMSByCursor(ctx context.Context, tx *gorm.DB, filter SmsListFilter, pg dbx.Pagination, afterCreatedAt time.Time) ([]*models.MessageSMSRecord, error) {
	pg = pg.Normalize()

	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	q = applySmsOrderBy(q, filter)
	q = applySmsCursor(q, filter, pg.AfterID, afterCreatedAt)

	results, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageSMSRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, nil
}

// applySmsCursor mirrors applyEmailCursor for SMS records.
func applySmsCursor(q gorm.ChainInterface[models.MessageSMSRecord], f SmsListFilter, afterID int64, afterCreatedAt time.Time) gorm.ChainInterface[models.MessageSMSRecord] {
	if afterID == 0 {
		return q
	}
	if f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC {
		return q.Where("created_at > ? OR (created_at = ? AND id > ?)", afterCreatedAt, afterCreatedAt, afterID)
	}
	return q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreatedAt, afterCreatedAt, afterID)
}
```

- [ ] **Step 4: 运行测试，验证通过**

Run: `go test ./internal/store/dal/ -run TestListSMSByCursor -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go
git commit -m "feat(dal/sms): add ListSMSByCursor with row-value cursor advance"
```

---

## Phase 5: Email Service 改造

### Task 11: ListEmails 传 Sort 字段，填新 response 字段

**Files:**
- Modify: `internal/service/message/email.go`

- [ ] **Step 1: 改 ListEmails 的 filter 构造和返回值**

在 `internal/service/message/email.go` 中，将 `ListEmails` 函数体替换为：

```go
// ListEmails returns a paginated list of email records matching the filter.
func (s *Service) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	f := dal.EmailListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	result, err := dal.ListEmailRecords(ctx, s.db, f, dbx.PageParams{
		Page:     int(req.GetPage()),
		PageSize: int(req.GetPageSize()),
		Count:    true,
	})
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.EmailRecord, len(result.List))
	for i, r := range result.List {
		protoRecords[i] = toProtoEmailRecord(r)
	}

	return &pb.ListEmailsResponse{
		Records:    protoRecords,
		Total:      int32(result.Total),
		TotalPages: int32(result.TotalPages),
		HasMore:    int32(req.GetPage()) < int32(result.TotalPages),
	}, nil
}
```

并在 import 块中追加 `"github.com/servekit/go-common/dbx"`（若已有则跳过）。

- [ ] **Step 2: 验证现有 service 测试仍通过**

Run: `go test ./internal/service/message/ -run TestListEmails`
Expected: PASS。`TestListEmails_ByScene` 默认不传 Sort，应得到 DESC 行为（与历史一致）。

> 注：`HasMore` 在 page=1 / total_pages=1 时为 `1 < 1` = false，符合预期。

- [ ] **Step 3: 加测试 `TestListEmails_ASC_WithTotalPages`**

在 `internal/service/message/email_test.go` 中追加：

```go
func TestListEmails_ASC_WithTotalPages(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock"},
	})

	for i := 0; i < 3; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       fmt.Sprintf("u%d@x.com", i),
			Subject:  "T",
			Body:     "B",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		Scene:         pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SortDirection: pb.SortDirection_SORT_DIRECTION_ASC,
		Page:          1,
		PageSize:      2,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(3), resp.Total)
	assert.Equal(t, int32(2), resp.TotalPages)
	assert.True(t, resp.HasMore)
	assert.Len(t, resp.Records, 2)
}
```

确保文件顶部 import 包含 `"fmt"`（如还没有则追加）。

- [ ] **Step 4: 运行测试，验证通过**

Run: `go test ./internal/service/message/ -run TestListEmails_ASC -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(service/email): pass Sort to dal and fill total_pages/has_more"
```

---

### Task 12: 实现 ListEmailsByCursor RPC（TDD）

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/email_test.go`

- [ ] **Step 1: 写失败测试 `TestListEmailsByCursor_TwoPageFlow`**

在 `email_test.go` 末尾追加：

```go
func TestListEmailsByCursor_TwoPageFlow(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock"},
	})

	// 3 records, page_size = 2 → expect 2 pages.
	for i := 0; i < 3; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       fmt.Sprintf("u%d@x.com", i),
			Subject:  "T",
			Body:     "B",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	// Page 1: empty token, page_size = 2.
	first, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize: 2,
	})
	require.NoError(t, err)
	assert.Len(t, first.Records, 2)
	assert.NotEmpty(t, first.NextPageToken, "must return a next-page token when more rows remain")
	assert.Equal(t, int32(0), first.Total, "include_total defaults to false → total stays 0")

	// Page 2: pass token.
	second, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:     pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: first.NextPageToken,
	})
	require.NoError(t, err)
	assert.Len(t, second.Records, 1)
	assert.Empty(t, second.NextPageToken, "no third page expected")

	// No duplication across pages.
	ids := map[int64]struct{}{}
	for _, r := range append(first.Records, second.Records...) {
		ids[r.Id] = struct{}{}
	}
	assert.Len(t, ids, 3, "cursor flow must return each record exactly once")
}
```

- [ ] **Step 2: 写失败测试 `TestListEmailsByCursor_BadToken`**

```go
func TestListEmailsByCursor_BadToken(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock"},
	})

	_, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:     pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: "garbage-token",
	})
	require.Error(t, err)
}
```

- [ ] **Step 3: 运行测试，验证失败**

Run: `go test ./internal/service/message/ -run TestListEmailsByCursor`
Expected: FAIL — `ListEmailsByCursor` 方法不存在。

- [ ] **Step 4: 实现 ListEmailsByCursor**

在 `email.go` 中紧跟 `ListEmails` 之后追加：

```go
// ListEmailsByCursor returns a cursor-paginated list of email records.
// Prefer this over ListEmails for large datasets or when COUNT(*) is
// expensive — set include_total = true to opt in to a count query.
func (s *Service) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	f := dal.EmailListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	pg := dbx.Pagination{PageSize: int(req.GetPageSize())}
	var afterCreatedAt time.Time
	if token := req.GetPageToken(); token != "" {
		cursor, err := pagination.DecodePageCursor(token)
		if err != nil {
			return nil, xcodes.ErrBadRequest.Wrap(err)
		}
		pg.AfterID = cursor.ID
		afterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
	}

	records, err := dal.ListEmailsByCursor(ctx, s.db, f, pg, afterCreatedAt)
	if err != nil {
		return nil, err
	}

	trimmed, hasNext := dbx.TrimPage(records, pg.PageSize)

	protoRecords := make([]*pb.EmailRecord, len(trimmed))
	for i, r := range trimmed {
		protoRecords[i] = toProtoEmailRecord(r)
	}

	var total int32
	if req.GetIncludeTotal() {
		// Cheap path: if hasNext is false and we know pg.AfterID == 0,
		// total == len(trimmed). Otherwise run a real count.
		if !hasNext && pg.AfterID == 0 {
			total = int32(len(trimmed))
		} else {
			count, err := dal.CountEmailRecords(ctx, s.db, f)
			if err != nil {
				return nil, err
			}
			total = int32(count)
		}
	}

	var nextToken string
	if hasNext {
		last := trimmed[len(trimmed)-1]
		nextToken = pagination.EncodePageCursor(pagination.PageCursor{
			ID:        last.ID,
			CreatedAt: pagination.CursorFromCreatedAt(last.CreatedAt),
		})
	}

	return &pb.ListEmailsByCursorResponse{
		Records:      protoRecords,
		Total:        total,
		NextPageToken: nextToken,
	}, nil
}
```

- [ ] **Step 5: 加 `dal.CountEmailRecords` helper**

在 `internal/store/dal/email_record.go` 中，紧跟 `ListEmailRecords` 之后追加：

```go
// CountEmailRecords returns the total number of email records matching
// filter, ignoring pagination. Used by ListEmailsByCursor when callers opt
// in via include_total = true.
func CountEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter) (int64, error) {
	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	count, err := q.Count(ctx, generated.MessageEmailRecord.ID.Column().Name)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return count, nil
}
```

- [ ] **Step 6: 添加 import 块的 `"message-service/internal/pagination"`**

在 `email.go` 的 import 块中追加：

```go
"message-service/internal/pagination"
```

- [ ] **Step 7: 运行测试，验证通过**

Run: `go test ./internal/service/message/ -run TestListEmailsByCursor -v`
Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go internal/store/dal/email_record.go
git commit -m "feat(service/email): add ListEmailsByCursor RPC"
```

---

## Phase 6: SMS Service 改造（与 Email 对称）

### Task 13: ListSMS 传 Sort 字段，填新 response 字段

**Files:**
- Modify: `internal/service/message/sms.go`

- [ ] **Step 1: 改 ListSMS 的 filter 构造和返回值**

将 `ListSMS` 函数体替换为（按 Task 11 step 1 的 Email 版本同构改写）：

```go
// ListSMS returns a paginated list of SMS records matching the filter.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	result, err := dal.ListSMSRecords(ctx, s.db, f, dbx.PageParams{
		Page:     int(req.GetPage()),
		PageSize: int(req.GetPageSize()),
		Count:    true,
	})
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.SMSRecord, len(result.List))
	for i, r := range result.List {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	return &pb.ListSMSResponse{
		Records:    protoRecords,
		Total:      int32(result.Total),
		TotalPages: int32(result.TotalPages),
		HasMore:    int32(req.GetPage()) < int32(result.TotalPages),
	}, nil
}
```

并在 import 块中追加 `"github.com/servekit/go-common/dbx"`（若已有则跳过）。

- [ ] **Step 2: 验证现有 service 测试仍通过**

Run: `go test ./internal/service/message/ -run TestListSMS`
Expected: PASS。

- [ ] **Step 3: 加测试 `TestListSMS_ASC_WithTotalPages`**

在 `internal/service/message/sms_test.go` 中追加（按 Task 11 step 3 的 Email 版本同构改写，类型与 scene 改为 SMS 对应）。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/service/message/ -run TestListSMS_ASC -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go
git commit -m "feat(service/sms): pass Sort to dal and fill total_pages/has_more"
```

---

### Task 14: 实现 ListSMSByCursor RPC（TDD）

**Files:**
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/message/sms_test.go`
- Modify: `internal/store/dal/sms_record.go`

- [ ] **Step 1: 写失败测试 `TestListSMSByCursor_TwoPageFlow` 和 `TestListSMSByCursor_BadToken`**

按 Task 12 step 1-2 的 Email 版本同构改写，使用 SMS 类型、SMS scene、`svc.ListSMSByCursor`、`&pb.ListSMSByCursorRequest{}`。

- [ ] **Step 2: 运行测试，验证失败**

Run: `go test ./internal/service/message/ -run TestListSMSByCursor`
Expected: FAIL — `ListSMSByCursor` 方法不存在。

- [ ] **Step 3: 实现 ListSMSByCursor**

在 `sms.go` 中紧跟 `ListSMS` 之后追加（按 Task 12 step 4 的 Email 版本同构改写）：

```go
// ListSMSByCursor is the cursor-paginated counterpart of ListSMS.
func (s *Service) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	pg := dbx.Pagination{PageSize: int(req.GetPageSize())}
	var afterCreatedAt time.Time
	if token := req.GetPageToken(); token != "" {
		cursor, err := pagination.DecodePageCursor(token)
		if err != nil {
			return nil, xcodes.ErrBadRequest.Wrap(err)
		}
		pg.AfterID = cursor.ID
		afterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
	}

	records, err := dal.ListSMSByCursor(ctx, s.db, f, pg, afterCreatedAt)
	if err != nil {
		return nil, err
	}

	trimmed, hasNext := dbx.TrimPage(records, pg.PageSize)

	protoRecords := make([]*pb.SMSRecord, len(trimmed))
	for i, r := range trimmed {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	var total int32
	if req.GetIncludeTotal() {
		if !hasNext && pg.AfterID == 0 {
			total = int32(len(trimmed))
		} else {
			count, err := dal.CountSMSRecords(ctx, s.db, f)
			if err != nil {
				return nil, err
			}
			total = int32(count)
		}
	}

	var nextToken string
	if hasNext {
		last := trimmed[len(trimmed)-1]
		nextToken = pagination.EncodePageCursor(pagination.PageCursor{
			ID:        last.ID,
			CreatedAt: pagination.CursorFromCreatedAt(last.CreatedAt),
		})
	}

	return &pb.ListSMSByCursorResponse{
		Records:      protoRecords,
		Total:        total,
		NextPageToken: nextToken,
	}, nil
}
```

在 import 块中追加 `"message-service/internal/pagination"`。

- [ ] **Step 4: 加 `dal.CountSMSRecords` helper**

在 `internal/store/dal/sms_record.go` 中，紧跟 `ListSMSRecords` 之后追加：

```go
// CountSMSRecords returns the total number of SMS records matching filter.
// Used by ListSMSByCursor when callers opt in via include_total = true.
func CountSMSRecords(ctx context.Context, tx *gorm.DB, filter SmsListFilter) (int64, error) {
	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	count, err := q.Count(ctx, generated.MessageSMSRecord.ID.Column().Name)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return count, nil
}
```

- [ ] **Step 5: 运行测试，验证通过**

Run: `go test ./internal/service/message/ -run TestListSMSByCursor -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go internal/store/dal/sms_record.go
git commit -m "feat(service/sms): add ListSMSByCursor RPC"
```

---

## Phase 7: 收尾

### Task 15: 全量 lint + 测试 + 编译

**Files:** 无修改

- [ ] **Step 1: 跑全部测试**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: 全部 PASS。重点观察 `internal/pagination/`、`internal/store/dal/`、`internal/service/message/` 三个包的覆盖率不低于改造前。

- [ ] **Step 2: 跑 lint**

Run: `golangci-lint run ./...`
Expected: 无 error。如果有 `unused` / `ineffectual assignment` 等 warning，回查对应 Task。

- [ ] **Step 3: 跑 gofmt / goimports**

Run:
```bash
gofmt -w $(find internal/ pkg/ -name "*.go")
goimports -w $(find internal/ pkg/ -name "*.go")
```

Expected: 无 diff（或全部格式化后已 clean）。

- [ ] **Step 4: 编译验证**

Run: `go build ./...`
Expected: 通过。重点确认接口满足：`Service` 已实现 `ListEmailsByCursor` 和 `ListSMSByCursor`。

- [ ] **Step 5: 清理 coverage 临时文件**

Run: `rm -f coverage.out`
Expected: 文件不存在。

- [ ] **Step 6: 最终 commit（如有遗漏的格式化改动）**

```bash
git status
# 若有未提交的格式化改动：
git add -p
git commit -m "style: gofmt/goimports after cursor-pagination feature"
```

---

## 完成判据

- [ ] `go build ./...` 通过
- [ ] `go test -race ./...` 全部 PASS
- [ ] `golangci-lint run ./...` 无 error
- [ ] `internal/pagination/`、`internal/store/dal/`、`internal/service/message/` 三个包均有覆盖 tiebreaker / ASC / cursor / 旧兼容性
- [ ] proto `make proto` 重生成产物已提交
- [ ] 每个 Task 一个独立 commit，message 符合 Conventional Commits
- [ ] 设计文档 `docs/superpowers/specs/2026-06-23-list-sort-and-cursor-design.md` 内容已实现
