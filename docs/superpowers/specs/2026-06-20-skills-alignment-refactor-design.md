# Skills 对齐重构 — 设计文档

**日期**: 2026-06-20
**主题**: 根据 `.claude/skills` 下的 4 个 skills(`go-common-usage`、`golang-development`、`gorm-cli-development`、`proto-development`)重构 message-service

## 1. 背景与目标

`.claude/skills` 指向 ai-kit-studio 和 go-common 仓库下的 4 个 skill 文档,定义了 Go 服务开发的统一规范。message-service 当前已大量遵循这些规范,但仍有结构性差距。

本次重构目标:**全面对齐 4 个 skills,同时保证不破坏 wire 兼容性、不引入回归**。

### 非目标

- 不引入新业务功能
- 不做架构层面的重新设计(消息发送流程、vendor 路由等保持现状)
- 不拆分 proto 文件(1-1-1)或改 package 名

## 2. 差距总览

| Skill | 差距等级 | 主要差距 |
|---|---|---|
| `gorm-cli-development` | **结构性(最大)** | repository 模式 vs dal 包级函数模式 |
| `go-common-usage` | 小 | 未用 `dbx.OffsetPaginate`,其他已符合 |
| `golang-development` | 小 | 文件声明顺序、doc comment、错误处理细节 |
| `proto-development` | 小 | proto 硬编码 `go_package`,字段注释缺失 |

## 3. 设计

### 3.1 dal 层重构(gorm-cli-development 对齐)

**目录变化**:
```
internal/store/
├── models/
│   ├── base.go              # 保留:Database/Stopper/AllModels
│   ├── genconfig.go         # 新增:从 models.go 拆出 genconfig.Config
│   ├── message_record.go    # MessageRecord model
│   └── (删除 models.go)
├── generated/               # 不动
└── dal/                     # 重命名自 repository/
    └── message_record.go    # 包级函数 + 带表前缀方法名
```

**dal 包级函数签名**(事务由 service 开,dal 不持有 db):

```go
package dal

func CreateMessageRecord(ctx context.Context, tx *gorm.DB, record *models.MessageRecord) error
func GetMessageRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageRecord, error)
func ListMessageRecords(ctx context.Context, tx *gorm.DB, filter ListFilter) ([]*models.MessageRecord, int64, error)
func CountMessageStats(ctx context.Context, tx *gorm.DB, filter StatsFilter) (*Stats, error)
func ListMessageVendorStats(ctx context.Context, tx *gorm.DB, filter StatsFilter) ([]VendorStat, error)
```

**类型跟着表走**(放 `dal/message_record.go`):
- `ListFilter`、`StatsFilter`、`Stats`、`VendorStat`

**设计要点**:
- 错误包装职责:dal 返回原始 error(`gorm.ErrRecordNotFound` 等),由 service 层用 `xcodes.ErrXxx.Wrap()` 包装。**理由**:dal 是数据访问层,不应感知业务错误码;service 才知道"找不到 message 记录"对应 `ErrMessageNotFound`。
- 之前的 `applyListFilter` / `applyStatsFilter` helper 保留,挪到 dal 文件底部。

### 3.2 service 层调整

**struct 变化**:
```go
type MessageService struct {
    pb.UnimplementedMessageServiceServer

    db            *gorm.DB
    database      *models.Database // 持有所有权,Stop 时关闭
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router
    manager       *lifecycle.Manager
}
```

- 删除字段 `repo *repository.MessageRecordRepository`
- 新增字段 `db *gorm.DB`(从 `database.DB` 取,二者指向同一对象)
- service 内部调用从 `s.repo.X(ctx, ...)` 改为 `dal.X(ctx, s.db, ...)`

**事务策略**:
- 单条 `Create`(发邮件/短信记录):不开事务(skill §8 明确)
- 未来若有多步写入(如 outbox 模式),在 service 方法内 `s.db.Transaction(func(tx *gorm.DB) error { ... })`,传 tx 给 dal

**错误处理细节**:
- service.New 中清理资源的 `_ = database.Stop()` 改为显式 log:
  ```go
  if err := database.Stop(); err != nil {
      slog.Error("cleanup database during init failure", "error", err)
  }
  ```
  理由:`Stop` 在 init 失败路径中可能也失败,日志记录而非吞错,便于排查。

### 3.3 golang-development 细节对齐

**文件声明顺序(decorder 标准)**:

按 gorm-cli-development §7 + golang-development §7:
1. `package` + 包注释
2. `import`
3. 类型 `type`
4. 构造函数 `New*`
5. 常量 `const`(若有)
6. 包级 `var`(若有,包括 `var _ pb.XxxServer = (*Yyy)(nil)`)
7. 导出方法(按 receiver 分组)
8. 导出函数(非 method)
9. 非导出方法
10. 非导出工具函数(文件底部)

调整范围:`internal/service/*.go`、`pkg/server.go` 等。

**doc comment 补全**:
- 所有导出标识符都要有 doc comment
- doc comment 以标识符名开头
- 重点补:`Start`、`Stop`、`SendEmail` 等 gRPC stub 方法(可以简短)

**反模式检查**(代码已基本合规,只确认):
- ✓ 无 `interface{}`(已用 `any`)
- ✓ 无 `fmt.Println` / `log.Println`(库代码不打日志)
- ✓ 配置子 struct 用指针
- ✓ New 函数接收 `*Config` 指针
- ✓ 错误用 `xerr.Wrap` 包装

### 3.4 go-common-usage 增强

**`dbx.OffsetPaginate` 替换手写分页**:

`dal.ListMessageRecords` 当前手写 `Limit(int(filter.PageSize))`,未做 page size 限制,存在潜在 DoS 风险(请求方传超大 page_size)。

**实施时发现**:`dbx.OffsetPaginate[T](tx *gorm.DB, p PageParams)` 接受 raw `*gorm.DB`,但 `gorm.G[T](db)` 返回 typed `Interface[T]`,**没有暴露 `UnderlyingDB()` 方法**,二者不能组合。按 `gorm-cli-development` §1 "类型安全优先" 原则,保留 gorm gen typed chain(放弃 OffsetPaginate),改为在 `ListMessageRecords` 入口处对 `filter.PageSize` 调 `dbx.ClampPageSize(int(filter.PageSize))`(自动 clamp 到 `[20, 100]`,见 `dbx.MaxPageSize` / `dbx.DefaultPageSize`)。这样既保留类型安全,又防止超大 page size 攻击。

具体实现(已落地,见 `internal/store/dal/message_record.go` `ListMessageRecords`):
```go
pageSize := dbx.ClampPageSize(int(filter.PageSize))
if filter.Page < 1 {
    filter.Page = 1
}
// ... applyListFilter + Count + applyListFilter + Order/Offset/Limit/Find
```

**其他 go-common 工具使用情况**(已合规,无需改):
- ✓ `configx.Load`
- ✓ `dbx.New` / `dbx.AutoMigrate` / `dbx.SetupTestDB` / `dbx.ClampPageSize`
- ✓ `logging.Setup`
- ✓ `grpcx.New`
- ✓ `lifecycle.NewManager`
- ✓ `signalx.RunWithForceQuit`
- ✓ `xerr` + `xcodes`

### 3.5 proto 调整(wire-safe only)

**必做**:
1. **删除 `option go_package = "message-service/gen/message/v1";`**
   - buf.gen.yaml 已配置 managed mode + `go_package_prefix: message-service/gen`
   - 留着硬编码 option 会被 managed mode 警告(或冲突)
   - 删除后 `buf generate` 重新生成 gen/

2. **补 doc comment**
   - 给所有 message、enum、enum value、RPC method、service 补完整 doc comment
   - 当前已有部分,补齐缺失的
   - 提升可读性 + godoc buf 插件输出

**不做**(均为 wire-unsafe 或非必要):
- ❌ 拆分 1-1-1 文件(当前 message.proto 内消息强耦合,拆分收益 < 维护成本)
- ❌ `SendResponse` 拆成 `SendEmailResponse` + `SendSMSResponse`(lint 已 disable,共用合理)
- ❌ `int64` 时间戳 → `google.protobuf.Timestamp`(wire-unsafe,影响 JSON 客户端)
- ❌ 改 package 名 / 目录路径(已有调用方依赖)

**lint 配置**:
- buf.yaml 当前已 disable `RPC_REQUEST_RESPONSE_UNIQUE` / `RPC_REQUEST_STANDARD_NAME` / `RPC_RESPONSE_STANDARD_NAME`,保持现状(允许共用 SendResponse)
- 启用其他默认 STANDARD lint,跑 `buf lint` 确认无新增 violation

### 3.6 测试调整

**`internal/store/repository/message_record_test.go`** → **`internal/store/dal/message_record_test.go`**:
- 包名 `repository` → `dal`
- `setupRepo` 返回 `*MessageRecordRepository` → `setupDB` 返回 `*gorm.DB`
- 调用从 `repo.Create(ctx, record)` → `dal.CreateMessageRecord(ctx, db, record)`
- 测试用例逻辑不变

**`internal/service/service_test.go`**:
- mock 路径不变(原本就用 `dbx.SetupTestDB` 启真实 DB)
- 调用从 `svc.repo.X` → `dal.X(ctx, svc.db, ...)`
- service 内部 `repo` 字段已删除,测试需要适配

**email/sms 包测试**:不涉及 store,不动。

### 3.7 memory 更新

| 当前 memory | 操作 |
|---|---|
| `service-repo-no-interface-indirection`(hold concrete `*repo` in service) | **改写**:从"service 持 repo"改为"service 持 `*gorm.DB`,dal 是包级函数" |
| `repository-naming-matches-models`(`MessageRecord` → `MessageRecordRepository`) | **删除**:不再有 Repository 概念 |
| `avoid-empty-abstraction-base-classes`(no `BaseRepo{ db *gorm.DB }`) | **保留**:仍适用,且 dal 包级函数比 BaseRepo 更彻底 |
| `tests-prefer-real-db-over-mocks`(`dbx.SetupTestDB`) | **保留**:继续使用 |

### 3.8 实施顺序(供 writing-plans 参考)

1. **dal 重构**:目录重命名 + 包级函数迁移 + 调用方 service 改写 + 测试调整
2. **gorm gen 重生**:`gorm gen` 重新生成 `internal/store/generated/`(应该无变化,但跑一遍确认)
3. **golang-development 细节**:声明顺序、doc comment、错误处理
4. **go-common-usage**:`dbx.OffsetPaginate` 替换手写分页
5. **proto 调整**:删 `go_package` option + 补 doc comment + `buf generate`
6. **memory 更新**:改写 + 删除对应文件
7. **全量验证**:`gofmt` + `goimports` + `golangci-lint run ./...` + `go test -race ./...`

每一步独立可提交,失败可回滚。

## 4. 风险与缓解

| 风险 | 缓解 |
|---|---|
| dal 错误处理从 service 下沉后,service 错误码可能遗漏 | 实现时检查每个 dal 调用点,显式 wrap |
| `dbx.OffsetPaginate` API 与假设签名不符 | 实现阶段先读 go-common 实际签名再写,不匹配则保留手写分页 |
| proto 删 go_package 后 buf generate 输出路径变 | managed mode 已配 prefix,生成路径应不变;`git diff` 验证 |
| 测试遗漏调用点 | 用 `grep -rn "\.repo\." internal/` 全局扫描 |
| `models.go` 拆出 genconfig.go 后 gorm gen 找不到配置 | gorm gen 通过 `genconfig.Config` 类型识别,文件名不影响;跑 `make generate` 验证 |

## 5. 验证清单

完成后必须全部通过:
- [ ] `gofmt -l .` 无输出
- [ ] `goimports -l .` 无输出
- [ ] `golangci-lint run ./...` 无 error
- [ ] `go test -race -count=1 ./...` 全部通过
- [ ] `go build ./...` 无错
- [ ] `buf lint` 无 violation
- [ ] `buf generate` 后 `git diff gen/` 与 proto 调整相符
- [ ] `gorm gen` 后 `git diff internal/store/generated/` 无 diff
- [ ] 手动运行 `go run ./cmd/server/`(若本地有 PG)确认启动无 panic

## 6. 关联

**相关历史 spec**:
- `2026-06-16-vendor-enum-design.md`(vendor 字段 enum 化)
- `2026-06-16-go-common-message-slimdown-design.md`(精简 go-common/message 包装)
- `2026-06-15-account-registry-extract-design.md`(账号注册表抽取)

**实现计划**:见 `docs/superpowers/plans/2026-06-20-skills-alignment-refactor-plan.md`(待 writing-plans 生成)
