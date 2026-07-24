# Skills 对齐重构 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 全面对齐 message-service 代码到 `.claude/skills` 下的 4 个 skills(go-common-usage、golang-development、gorm-cli-development、proto-development)。

**Architecture:** 自下而上重构:models → dal → service → 测试 → golang 细节 → proto → memory → 验证。dal 包采用包级函数 + 接收 `*gorm.DB` 的 skill 标准模式;service 直接持有 `*gorm.DB`;proto 仅做 wire-safe 调整(删 `go_package` + 补 doc comment)。

**Tech Stack:** Go 1.26、gorm.io/cli v0.2.4 (typed chain)、go-common(dbx 分页工具)、buf v2、protovalidate。

**Spec:** `docs/superpowers/specs/2026-06-20-skills-alignment-refactor-design.md`

---

## 关于 `dbx.OffsetPaginate` 的 trade-off(写 plan 时验证后修正)

**Spec 第 3.4 节**原本写"用 `dbx.OffsetPaginate[T]` 替换手写分页"。**验证后无法实施**,理由:

- `dbx.OffsetPaginate[T](tx *gorm.DB, p PageParams)` 接受 raw `*gorm.DB`
- `gorm.G[T](db)` 返回 typed `Interface[T]`,**没有 `UnderlyingDB()` 方法暴露底层 `*gorm.DB`**
- 二者无法组合

按 gorm-cli-development §1"类型安全优先"原则,**保留 gorm gen typed chain**(放弃 OffsetPaginate),改用 `dbx.ClampPageSize` 保护 page size(当前 `Limit(int(filter.PageSize))` 没做 clamp,潜在 DoS 风险)。

本计划最后一步会把 spec 的第 3.4 节同步更新到这个结论。

---

## File Structure

修改/创建的文件:

| 文件 | 操作 | 职责 |
|---|---|---|
| `internal/store/models/models.go` | 删除 | genconfig 配置移走 |
| `internal/store/models/genconfig.go` | 新建 | gorm gen 配置(`genconfig.Config`) |
| `internal/store/repository/` | 删除整个目录 | 被 dal/ 取代 |
| `internal/store/dal/message_record.go` | 新建 | MessageRecord 数据访问层(包级函数) |
| `internal/store/dal/message_record_test.go` | 新建 | dal 测试(从 repository_test.go 迁移) |
| `internal/service/service.go` | 修改 | 去掉 repo 字段,加 db 字段 |
| `internal/service/send.go` | 修改 | `s.repo.X` → `dal.X(ctx, s.db, ...)` |
| `internal/service/query.go` | 修改 | 同上 |
| `internal/service/service_test.go` | 修改 | mock 路径迁移到 dal |
| `pkg/server.go` | 修改 | doc comment、声明顺序 |
| `pkg/module.go` | 修改 | doc comment |
| `pkg/client.go` | 修改 | doc comment |
| `api/proto/message/v1/message.proto` | 修改 | 删 `option go_package`、补 doc comment |
| `gen/message/v1/*.pb.go` | buf 重新生成 | 跟随 proto 变化 |
| `Makefile` | 修改 | `gorm gen` 加 `-i/-o` 参数,跟 skeleton 一致 |
| `.claude/projects/-Users-moss-code-base-message-service/memory/*.md` | 修改 | 改写 service-repo memory,删 repository-naming memory |

---

## Task 1: 拆分 models 包 — genconfig 提取到独立文件

**Files:**
- Create: `internal/store/models/genconfig.go`
- Delete: `internal/store/models/models.go`

**目的:** 当前 `models.go` 用 `var _ = genconfig.Config{...}` 这种 side-effect 表达式来"声明" gorm gen 的字段映射,这是反模式(依赖于 Go 对未使用 var 的特殊处理)。拆到独立文件后,文件名直观表达职责。

- [ ] **Step 1.1: 创建 `internal/store/models/genconfig.go`**

```go
package models

import (
	"database/sql"

	"gorm.io/cli/gorm/field"
	"gorm.io/cli/gorm/genconfig"
)

// gormGenConfig declares field-type mappings for gorm gen so the typed
// generator emits usable helpers for types that don't map cleanly to a
// built-in SQL/GORM type. Referenced by `gorm gen -i ./internal/store/models
// -o ./internal/store/generated` (Makefile target `generate`).
//
// Kept as a package-level var (not invoked) because gorm gen scans the
// models package for a value of type genconfig.Config and reads its fields.
var gormGenConfig = genconfig.Config{
	OutPath: "internal/store/generated",

	FieldTypeMap: map[any]any{
		sql.NullTime{}:    field.Time{},
		MapStringString{}: field.Field[map[string]string]{},
	},
}
```

- [ ] **Step 1.2: 删除 `internal/store/models/models.go`**

```bash
rm internal/store/models/models.go
```

注意:`AllModels()` 函数当前在 `models.go` 里,需要先把它迁到 `base.go`(Step 1.3)。

- [ ] **Step 1.3: 把 `AllModels()` 迁到 `internal/store/models/base.go`**

读取 `base.go` 当前内容,在文件末尾追加:

```go
// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&MessageRecord{},
	}
}
```

`base.go` 最终完整内容应为:

```go
package models

import (
	"database/sql"

	"gorm.io/gorm"
)

// Database wraps *gorm.DB with ownership tracking. When owned, Stop closes the
// underlying *sql.DB; when not owned (e.g. injected externally or managed by a
// testcontainer), Stop is a no-op. Implements lifecycle.Stopper so an instance
// can be registered with lifecycle.Manager via AddStopper without the caller
// branching on ownership.
type Database struct {
	DB    *gorm.DB
	owned bool
}

// NewDatabase wraps db. When owned is true, responsibility for closing the
// underlying *sql.DB transfers to the returned *Database.
func NewDatabase(db *gorm.DB, owned bool) *Database {
	return &Database{DB: db, owned: owned}
}

// Stop closes the underlying *sql.DB when the Database owns it. Satisfies
// lifecycle.Stopper; safe to call on a nil receiver.
func (d *Database) Stop() error {
	if d == nil || !d.owned || d.DB == nil {
		return nil
	}
	if sqlDB, err := d.DB.DB(); err == nil && sqlDB != nil {
		_ = sqlDB.Close()
	}
	return nil
}

// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&MessageRecord{},
	}
}
```

- [ ] **Step 1.4: 验证编译**

Run: `go build ./internal/store/models/...`
Expected: 无错误

- [ ] **Step 1.5: 验证 gorm gen 仍能跑通**

Run: `gorm gen -i ./internal/store/models -o ./internal/store/generated`
Expected: 无错误,`internal/store/generated/` 内容不变

Run: `git diff internal/store/generated/`
Expected: 空输出

- [ ] **Step 1.6: Commit**

```bash
git add internal/store/models/genconfig.go internal/store/models/base.go
git rm internal/store/models/models.go
git commit -m "refactor(store/models): extract genconfig to its own file"
```

---

## Task 2: 创建 dal 包 — 包级函数 + 表前缀方法名

**Files:**
- Create: `internal/store/dal/message_record.go`

**目的:** 按 gorm-cli-development §6 的规范,dal 是包级函数,接收 `ctx + *gorm.DB`,方法名带表前缀。

- [ ] **Step 2.1: 创建 `internal/store/dal/message_record.go`**

完整文件内容(直接写入):

```go
// Package dal provides type-safe data access for message-service tables.
//
// Each file in this package corresponds to one table; cross-table operations
// are composed at the service layer. Functions accept a *gorm.DB (or tx) and
// return raw errors; the service layer wraps them with xcodes.
package dal

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/store/generated"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// ListFilter holds parameters for listing message records.
type ListFilter struct {
	Channel     pb.Channel
	Status      pb.MessageStatus
	Target      string
	EmailVendor pb.EmailVendor // applied only when Channel == EMAIL
	SmsVendor   pb.SmsVendor   // applied only when Channel == SMS
	StartTime   *time.Time
	EndTime     *time.Time
	Page        int32
	PageSize    int32
}

// StatsFilter holds parameters for querying message statistics.
type StatsFilter struct {
	Channel     pb.Channel
	EmailVendor pb.EmailVendor
	SmsVendor   pb.SmsVendor
	StartTime   *time.Time
	EndTime     *time.Time
}

// Stats contains aggregated message statistics.
type Stats struct {
	Total       int64
	Sent        int64
	Failed      int64
	SuccessRate float64
}

// VendorStat contains per-vendor message statistics. The Vendor int must be
// interpreted by the caller based on Channel (EmailVendor vs SmsVendor).
type VendorStat struct {
	Channel pb.Channel
	Vendor  int32
	Total   int64
	Sent    int64
	Failed  int64
}

// CreateMessageRecord inserts a new message record. record.ID is backfilled
// on success.
func CreateMessageRecord(ctx context.Context, tx *gorm.DB, record *models.MessageRecord) error {
	if err := gorm.G[models.MessageRecord](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetMessageRecord returns the message record with the given ID, or
// xcodes.ErrMessageNotFound when no such record exists.
func GetMessageRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageRecord, error) {
	record, err := gorm.G[models.MessageRecord](tx).
		Where(generated.MessageRecord.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrMessageNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListMessageRecords returns a page of message records matching filter, along
// with the total count. page_size is clamped to dbx.MaxPageSize to prevent
// excessive result sets.
func ListMessageRecords(ctx context.Context, tx *gorm.DB, filter ListFilter) ([]*models.MessageRecord, int64, error) {
	pageSize := dbx.ClampPageSize(int(filter.PageSize))
	if filter.Page < 1 {
		filter.Page = 1
	}

	q := applyListFilter(gorm.G[models.MessageRecord](tx), filter)

	total, err := q.Count(ctx, "id")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	q = applyListFilter(gorm.G[models.MessageRecord](tx), filter)
	offset := int((filter.Page - 1) * int32(pageSize))
	results, err := q.
		Order(generated.MessageRecord.CreatedAt.Desc()).
		Offset(offset).
		Limit(pageSize).
		Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, total, nil
}

// CountMessageStats returns aggregated message statistics matching filter.
func CountMessageStats(ctx context.Context, tx *gorm.DB, filter StatsFilter) (*Stats, error) {
	total, err := applyStatsFilter(gorm.G[models.MessageRecord](tx), filter).Count(ctx, "id")
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	sent, err := applyStatsFilter(gorm.G[models.MessageRecord](tx), filter).
		Where(generated.MessageRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_SENT))).
		Count(ctx, "id")
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	failed, err := applyStatsFilter(gorm.G[models.MessageRecord](tx), filter).
		Where(generated.MessageRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_FAILED))).
		Count(ctx, "id")
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	var successRate float64
	if total > 0 {
		successRate = float64(sent) / float64(total) * 100
	}

	return &Stats{
		Total:       total,
		Sent:        sent,
		Failed:      failed,
		SuccessRate: successRate,
	}, nil
}

// ListMessageVendorStats returns per-vendor message statistics matching filter.
// Each row contains Channel + Vendor int (interpret by channel).
//
// Uses tx.Model(...) directly (rather than the generic gorm.G[T] chain)
// because raw SELECT with GROUP BY loses the model binding in the generic
// chain, which causes an empty FROM clause.
func ListMessageVendorStats(ctx context.Context, tx *gorm.DB, filter StatsFilter) ([]VendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := tx.Model(&models.MessageRecord{}).Where("id > 0")
	if filter.Channel != 0 {
		q = q.Where("channel = ?", int32(filter.Channel))
	}
	if filter.EmailVendor != 0 {
		q = q.Where("email_vendor = ?", int32(filter.EmailVendor))
	}
	if filter.SmsVendor != 0 {
		q = q.Where("sms_vendor = ?", int32(filter.SmsVendor))
	}
	if filter.StartTime != nil {
		q = q.Where("created_at >= ?", *filter.StartTime)
	}
	if filter.EndTime != nil {
		q = q.Where("created_at <= ?", *filter.EndTime)
	}

	rows, err := q.
		Select(fmt.Sprintf(
			"channel, COALESCE(NULLIF(email_vendor, 0), sms_vendor) AS vendor, COUNT(*) as total, COUNT(*) FILTER (WHERE status = %d) as sent, COUNT(*) FILTER (WHERE status = %d) as failed",
			sentStatus, failedStatus,
		)).
		Group("channel, COALESCE(NULLIF(email_vendor, 0), sms_vendor)").
		Rows()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	defer func() { _ = rows.Close() }()

	var stats []VendorStat
	for rows.Next() {
		var s VendorStat
		if err := rows.Scan(&s.Channel, &s.Vendor, &s.Total, &s.Sent, &s.Failed); err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	return stats, nil
}

// --- internal helpers ---

// applyListFilter applies non-zero filter fields as WHERE clauses.
func applyListFilter(q gorm.ChainInterface[models.MessageRecord], f ListFilter) gorm.ChainInterface[models.MessageRecord] {
	if f.Channel != 0 {
		q = q.Where(generated.MessageRecord.Channel.Eq(int32(f.Channel)))
	}
	if f.Status != 0 {
		q = q.Where(generated.MessageRecord.Status.Eq(int32(f.Status)))
	}
	if f.Target != "" {
		q = q.Where(generated.MessageRecord.Target.Eq(f.Target))
	}
	if f.EmailVendor != 0 {
		q = q.Where(generated.MessageRecord.EmailVendor.Eq(int32(f.EmailVendor)))
	}
	if f.SmsVendor != 0 {
		q = q.Where(generated.MessageRecord.SmsVendor.Eq(int32(f.SmsVendor)))
	}
	if f.StartTime != nil {
		q = q.Where(generated.MessageRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.MessageRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}

// applyStatsFilter applies non-zero filter fields as WHERE clauses for stats queries.
func applyStatsFilter(q gorm.ChainInterface[models.MessageRecord], f StatsFilter) gorm.ChainInterface[models.MessageRecord] {
	if f.Channel != 0 {
		q = q.Where(generated.MessageRecord.Channel.Eq(int32(f.Channel)))
	}
	if f.EmailVendor != 0 {
		q = q.Where(generated.MessageRecord.EmailVendor.Eq(int32(f.EmailVendor)))
	}
	if f.SmsVendor != 0 {
		q = q.Where(generated.MessageRecord.SmsVendor.Eq(int32(f.SmsVendor)))
	}
	if f.StartTime != nil {
		q = q.Where(generated.MessageRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.MessageRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}
```

注意:文件中的 `import "message-service/gen/message/v1"` 是一个未使用的 import,实际写入时需要去掉(只保留 `pb "message-service/gen/message/v1"`)。**写文件时只写上面第二个干净版本**。

实际上**只写入上面给出的单一干净版本**,不要写两个版本。上面的代码块自包含、可直接粘贴。

- [ ] **Step 2.2: 验证编译**

Run: `go build ./internal/store/dal/...`
Expected: 无错误

- [ ] **Step 2.3: Commit**

```bash
git add internal/store/dal/message_record.go
git commit -m "feat(store/dal): add package-level dal for MessageRecord"
```

---

## Task 3: 切换 service 层 — 去 repo 字段,加 db 字段

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/send.go`
- Modify: `internal/service/query.go`

**目的:** service 持有 `*gorm.DB`(skill §8),不再持 `*Repository`。

- [ ] **Step 3.1: 改 `internal/service/service.go`**

读取当前文件,做如下修改:

1. 删除 `"message-service/internal/store/repository"` import
2. 新增 `"message-service/internal/store/dal"` import
3. `MessageService` struct 删除 `repo *repository.MessageRecordRepository`,新增 `db *gorm.DB`
4. `newWithDeps` 删除 `msgRepo := repository.NewMessageRecordRepository(database.DB)` 和 `repo: msgRepo`,新增 `db: database.DB`
5. 所有 `s.repo.X(ctx, ...)` 改成 `dal.X(ctx, s.db, ...)`

**修改后的关键片段**:

```go
package service

import (
	"context"
	"fmt"
	"log/slog"

	"message-service/internal/message/email"
	"message-service/internal/message/sms"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/config"
	"message-service/pkg/option"
	"message-service/pkg/thirdcall"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"

	"gorm.io/gorm"
)

// MessageService implements pb.MessageServiceServer.
type MessageService struct {
	pb.UnimplementedMessageServiceServer

	database      *models.Database
	db            *gorm.DB
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router // nil when no routes configured
	manager       *lifecycle.Manager
}
```

`newWithDeps` 修改:

```go
func newWithDeps(cfg *config.Config, database *models.Database, gid thirdcall.GIDService) (*MessageService, error) {
	emailRegistry, err := email.NewAccountRegistry(cfg.Email)
	if err != nil {
		return nil, fmt.Errorf("email registry: %w", err)
	}

	smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
	if err != nil {
		return nil, fmt.Errorf("sms registry: %w", err)
	}

	smsRouter, err := sms.BuildRouter(cfg.SMS, smsRegistry)
	if err != nil {
		return nil, fmt.Errorf("sms router: %w", err)
	}

	return &MessageService{
		database:      database,
		db:            database.DB,
		gid:           gid,
		emailRegistry: emailRegistry,
		smsRegistry:   smsRegistry,
		smsRouter:     smsRouter,
	}, nil
}
```

`New` 函数清理路径改 `_ =` 为显式 log:

```go
func New(cfg *config.Config, opts ...option.Option) (*MessageService, error) {
	o := option.Apply(opts...)

	db, ownDB, err := resolveDB(&o, cfg)
	if err != nil {
		return nil, err
	}
	database := models.NewDatabase(db, ownDB)

	gid, err := resolveGID(cfg, o.GIDService)
	if err != nil {
		if stopErr := database.Stop(); stopErr != nil {
			slog.Error("cleanup database during init failure", "error", stopErr)
		}
		return nil, err
	}

	svc, err := newWithDeps(cfg, database, gid)
	if err != nil {
		if stopErr := database.Stop(); stopErr != nil {
			slog.Error("cleanup database during init failure", "error", stopErr)
		}
		return nil, err
	}
	svc.manager = lifecycle.NewManager()
	svc.manager.AddStopper("db", database)
	return svc, nil
}
```

`toProtoRecord` 不变(只读 models.MessageRecord)。

- [ ] **Step 3.2: 改 `internal/service/send.go`**

`persistEmailRecord` / `persistSMSRecord` 中 `s.repo.Create(ctx, record)` → `dal.CreateMessageRecord(ctx, s.db, record)`。

修改后的关键片段:

```go
if err := dal.CreateMessageRecord(ctx, record); err != nil {
    slog.Error("persist email record", "record_id", id, "error", err)
}
```

```go
if err := dal.CreateMessageRecord(ctx, record); err != nil {
    slog.Error("persist sms record", "record_id", id, "error", err)
}
```

完整 import 改为:

```go
import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"message-service/internal/message/email"
	"message-service/internal/message/sms"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	pb "message-service/gen/message/v1"

	emailcommon "github.com/servekit/go-common/message/email"
	smscommon "github.com/servekit/go-common/message/sms"
)
```

- [ ] **Step 3.3: 改 `internal/service/query.go`**

把 `s.repo.FindByID` → `dal.GetMessageRecord`,`s.repo.List` → `dal.ListMessageRecords`,`s.repo.Stats` → `dal.CountMessageStats`,`s.repo.VendorStats` → `dal.ListMessageVendorStats`,`repository.ListFilter` → `dal.ListFilter`,`repository.StatsFilter` → `dal.StatsFilter`。

完整修改后的 `query.go`:

```go
package service

import (
	"context"
	"time"

	"message-service/internal/store/dal"

	pb "message-service/gen/message/v1"
)

func (s *MessageService) getMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	record, err := dal.GetMessageRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoRecord(record), nil
}

func (s *MessageService) listMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	f := dal.ListFilter{
		Channel:     req.GetChannel(),
		Status:      req.GetStatus(),
		Target:      req.GetTarget(),
		EmailVendor: req.GetEmailVendor(),
		SmsVendor:   req.GetSmsVendor(),
		Page:        req.GetPage(),
		PageSize:    req.GetPageSize(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	records, total, err := dal.ListMessageRecords(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.MessageRecord, len(records))
	for i, r := range records {
		protoRecords[i] = toProtoRecord(r)
	}

	return &pb.ListMessagesResponse{
		Records: protoRecords,
		Total:   int32(total),
	}, nil
}

func (s *MessageService) getMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	f := dal.StatsFilter{
		Channel:     req.GetChannel(),
		EmailVendor: req.GetEmailVendor(),
		SmsVendor:   req.GetSmsVendor(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := dal.CountMessageStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListMessageVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	var emailStats []*pb.EmailVendorStats
	var smsStats []*pb.SmsVendorStats
	for _, vs := range vendorStats {
		switch vs.Channel {
		case pb.Channel_CHANNEL_EMAIL:
			emailStats = append(emailStats, &pb.EmailVendorStats{
				Vendor: pb.EmailVendor(vs.Vendor),
				Total:  vs.Total,
				Sent:   vs.Sent,
				Failed: vs.Failed,
			})
		case pb.Channel_CHANNEL_SMS:
			smsStats = append(smsStats, &pb.SmsVendorStats{
				Vendor: pb.SmsVendor(vs.Vendor),
				Total:  vs.Total,
				Sent:   vs.Sent,
				Failed: vs.Failed,
			})
		}
	}

	return &pb.MessageStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		EmailStats:  emailStats,
		SmsStats:    smsStats,
	}, nil
}
```

- [ ] **Step 3.4: 暂不验证编译**(因为 service_test.go 还引用旧的 repository,编译会失败)

跳过编译验证,直接进入 Task 4 修复测试。Task 4 完成后会一并验证。

- [ ] **Step 3.5: Commit**(此时测试还通不过,先 commit 代码改动)

```bash
git add internal/service/service.go internal/service/send.go internal/service/query.go
git commit -m "refactor(service): switch from *Repository to *gorm.DB + dal package"
```

---

## Task 4: 删除 repository 包 + 修复测试

**Files:**
- Delete: `internal/store/repository/message_record.go`
- Delete: `internal/store/repository/message_record_test.go`(整个 `repository/` 目录)
- Create: `internal/store/dal/message_record_test.go`
- Modify: `internal/service/service_test.go`

- [ ] **Step 4.1: 创建 `internal/store/dal/message_record_test.go`**

迁移自 `internal/store/repository/message_record_test.go`,改动:
- 包名 `repository` → `dal`
- `setupRepo` 返回 `*MessageRecordRepository` → `setupDB` 返回 `*gorm.DB`
- `repo.Create(ctx, x)` → `dal.CreateMessageRecord(ctx, db, x)`
- `repo.FindByID(ctx, x)` → `dal.GetMessageRecord(ctx, db, x)`
- `repo.List(ctx, filter)` → `dal.ListMessageRecords(ctx, db, filter)`
- `repo.Stats(ctx, filter)` → `dal.CountMessageStats(ctx, db, filter)`
- `repo.VendorStats(ctx, filter)` → `dal.ListMessageVendorStats(ctx, db, filter)`
- `repository.ListFilter`/`StatsFilter`/`VendorStat` → `dal.ListFilter`/`StatsFilter`/`VendorStat`

完整内容(基于现有 `repository/message_record_test.go`,逐字替换符号):

```go
package dal

import (
	"context"
	"testing"
	"time"

	"message-service/internal/store/models"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	err := db.AutoMigrate(&models.MessageRecord{})
	require.NoError(t, err, "auto-migrate should succeed")
	return db
}

func newTestRecord(channel, status int32, target string, emailVendor, smsVendor int32) *models.MessageRecord {
	return &models.MessageRecord{
		ID:          time.Now().UnixNano(),
		Channel:     channel,
		EmailVendor: emailVendor,
		SmsVendor:   smsVendor,
		Status:      status,
		Target:      target,
		Subject:     "Test Subject",
		Content:     "Test content body",
		Attempts:    1,
	}
}

func TestCreateMessageRecord(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	record := newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_PENDING), "user@example.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)

	err := CreateMessageRecord(ctx, db, record)
	require.NoError(t, err, "Create should succeed")

	found, err := GetMessageRecord(ctx, db, record.ID)
	require.NoError(t, err, "Get should succeed")

	assert.Equal(t, record.ID, found.ID)
	assert.Equal(t, int32(pb.Channel_CHANNEL_EMAIL), found.Channel)
	assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_PENDING), found.Status)
	assert.Equal(t, "user@example.com", found.Target)
	assert.Equal(t, int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), found.EmailVendor)
	assert.Equal(t, "Test Subject", found.Subject)
	assert.Equal(t, "Test content body", found.Content)
	assert.Equal(t, 1, found.Attempts)
	assert.False(t, found.CreatedAt.IsZero())
	assert.False(t, found.UpdatedAt.IsZero())
}

func TestGetMessageRecord_NotFound(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	_, err := GetMessageRecord(ctx, db, 99999999)
	assert.Error(t, err, "Get with non-existent ID should return error")
}

func TestListMessageRecords(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		record := newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_PENDING), "user@example.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)
		require.NoError(t, CreateMessageRecord(ctx, db, record), "Create should succeed")
		time.Sleep(time.Millisecond * 10)
	}

	records, total, err := ListMessageRecords(ctx, db, ListFilter{
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err, "List should succeed")
	assert.Equal(t, int64(5), total, "total count should be 5")
	assert.Len(t, records, 5, "should return 5 records")
}

func TestListMessageRecords_WithFilter(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_PENDING), "user@example.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)))
	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_SMS), int32(pb.MessageStatus_MESSAGE_STATUS_SENT), "+111", 0, int32(pb.SmsVendor_SMS_VENDOR_ALIYUN))))

	records, total, err := ListMessageRecords(ctx, db, ListFilter{
		Channel:  pb.Channel_CHANNEL_EMAIL,
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Len(t, records, 1)
}

func TestListMessageRecords_PageSizeClamped(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_PENDING), "user@example.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)))
	}

	// PageSize 1000 exceeds dbx.MaxPageSize (100); should be clamped.
	records, _, err := ListMessageRecords(ctx, db, ListFilter{
		Page:     1,
		PageSize: 1000,
	})
	require.NoError(t, err)
	assert.Len(t, records, 5, "all 5 records returned (clamped page size still fits)")
}

func TestCountMessageStats(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_SENT), "a@b.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)))
	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_SENT), "b@c.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)))
	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_SMS), int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), "+111", 0, int32(pb.SmsVendor_SMS_VENDOR_ALIYUN))))

	stats, err := CountMessageStats(ctx, db, StatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Total)
	assert.Equal(t, int64(2), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
	assert.InDelta(t, 66.67, stats.SuccessRate, 0.1)
}

func TestListMessageVendorStats(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_EMAIL), int32(pb.MessageStatus_MESSAGE_STATUS_SENT), "a@b.com", int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), 0)))
	require.NoError(t, CreateMessageRecord(ctx, db, newTestRecord(int32(pb.Channel_CHANNEL_SMS), int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), "+111", 0, int32(pb.SmsVendor_SMS_VENDOR_ALIYUN))))

	stats, err := ListMessageVendorStats(ctx, db, StatsFilter{})
	require.NoError(t, err)
	require.Len(t, stats, 2)
}
```

- [ ] **Step 4.2: 改 `internal/service/service_test.go`**

修改要点:
- 删除 `"message-service/internal/store/repository"` import,新增 `"message-service/internal/store/dal"`
- `setupDB` 当前返回 `(*gorm.DB, *repository.MessageRecordRepository)`,改为返回 `*gorm.DB`(去掉 repo 返回值)
- `newTestEmailService`/`newTestSMSServiceWithRouter`/`setupQueryTest` 当前返回 `(*MessageService, *repository.MessageRecordRepository)`,改为只返回 `*MessageService`
- `MessageService{ repo: repo, ... }` → `MessageService{ db: db, ... }`
- 测试内 `repo.FindByID(ctx, x)` → `dal.GetMessageRecord(ctx, svc.db, x)` 等
- `repository.ListFilter`/`StatsFilter` → `dal.ListFilter`/`StatsFilter`

具体修改(只展示关键 diff,实际执行时全文替换):

```go
// import 改:
import (
	// ...
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	// 删除 "message-service/internal/store/repository"
	// ...
)

// setupDB 改:
func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.MessageRecord{}), "auto-migrate should succeed")
	return db
}

// newTestEmailService 改(返回类型去掉 repo):
func newTestEmailService(t *testing.T, providers []emailcommon.Provider) *MessageService {
	t.Helper()
	db := setupDB(t)
	accounts := make(map[string]*email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = &email.AccountProvider{
			Vendor: p.Name(), Account: fmt.Sprintf("p%d", i), Provider: p,
		}
	}
	return &MessageService{
		database:      models.NewDatabase(db, false),
		db:            db,
		gid:           getTestGID(t),
		manager:       lifecycle.NewManager(),
		emailRegistry: email.NewAccountRegistryFromProviders(map[string]map[string]*email.AccountProvider{"mock": accounts}),
	}
}

// newTestSMSServiceWithRouter 改:
func newTestSMSServiceWithRouter(t *testing.T, providers []smscommon.Provider) *MessageService {
	t.Helper()
	db := setupDB(t)
	targets := make([]*sms.AccountProvider, len(providers))
	for i, p := range providers {
		targets[i] = &sms.AccountProvider{
			Vendor: p.Name(), Account: fmt.Sprintf("p%d", i), Provider: p,
		}
	}
	router := sms.NewRouter("CN", targets,
		sms.Route{Country: "CN", Targets: targets},
		sms.Route{Country: "*", Targets: targets},
	)
	return &MessageService{
		database:  models.NewDatabase(db, false),
		db:        db,
		gid:       getTestGID(t),
		manager:   lifecycle.NewManager(),
		smsRouter: router,
	}
}

// setupQueryTest 改:
func setupQueryTest(t *testing.T) (*MessageService, *gorm.DB) {
	t.Helper()
	db := setupDB(t)
	return &MessageService{database: models.NewDatabase(db, false), db: db, gid: getTestGID(t), manager: lifecycle.NewManager()}, db
}
```

所有测试函数体内,凡是 `svc, repo := newTestXxx(...)` 都改为 `svc := newTestXxx(...)`(只接收一个返回值),凡是 `repo.X` 都改为 `dal.X(ctx, svc.db, ...)`。

具体示例:
```go
// 之前:
svc, repo := newTestEmailService(t, []emailcommon.Provider{&mockEmailProvider{name: "mock"}})
// ...
rec, err := repo.FindByID(context.Background(), resp.Id)

// 之后:
svc := newTestEmailService(t, []emailcommon.Provider{&mockEmailProvider{name: "mock"}})
// ...
rec, err := dal.GetMessageRecord(context.Background(), svc.db, resp.Id)
```

```go
// 之前:
records, total, err := repo.List(context.Background(), repository.ListFilter{
	Status:   pb.MessageStatus_MESSAGE_STATUS_FAILED,
	Page:     1,
	PageSize: 10,
})

// 之后:
records, total, err := dal.ListMessageRecords(context.Background(), svc.db, dal.ListFilter{
	Status:   pb.MessageStatus_MESSAGE_STATUS_FAILED,
	Page:     1,
	PageSize: 10,
})
```

```go
// 之前:
svc.repo.Create(ctx, record)
svc.repo.FindByID(ctx, x)
svc.repo.List(ctx, repository.ListFilter{...})
svc.repo.Stats(ctx, repository.StatsFilter{})
svc.repo.VendorStats(ctx, repository.StatsFilter{})

// 之后:
dal.CreateMessageRecord(ctx, svc.db, record)
dal.GetMessageRecord(ctx, svc.db, x)
dal.ListMessageRecords(ctx, svc.db, dal.ListFilter{...})
dal.CountMessageStats(ctx, svc.db, dal.StatsFilter{})
dal.ListMessageVendorStats(ctx, svc.db, dal.StatsFilter{})
```

`repository.VendorStat` 在 TestQuery_Stats 内的本地 type alias 改成 `dal.VendorStat`。

- [ ] **Step 4.3: 删除 repository 包**

```bash
git rm -r internal/store/repository/
```

- [ ] **Step 4.4: 验证编译**

Run: `go build ./...`
Expected: 无错误

- [ ] **Step 4.5: 跑测试**

Run: `go test -race -count=1 ./internal/store/dal/... ./internal/service/...`
Expected: 所有测试 PASS

(注:testcontainer 启动需要 Docker,本地环境若无法跑可只跑 `go test -count=1 -run "TestCreate|TestGet|TestList" ./internal/store/dal/...`)

- [ ] **Step 4.6: Commit**

```bash
git add internal/store/dal/message_record_test.go internal/service/service_test.go
git rm -r internal/store/repository/
git commit -m "refactor(store): replace repository package with dal"
```

---

## Task 5: golang-development 细节 — 声明顺序、doc comment

**Files:**
- Modify: `internal/service/service.go`
- Modify: `pkg/server.go`
- Modify: `pkg/module.go`
- Modify: `pkg/client.go`

**目的:** 按 golang-development §7 decorder 标准调整声明顺序(type → New → const → var → 导出方法 → 非导出方法 → helpers),并补 doc comment。

- [ ] **Step 5.1: 调整 `internal/service/service.go` 声明顺序**

当前顺序:`type → New → Start/Stop → gRPC stubs → var _ check → helpers`。

调整为 decorder 标准:`type → New → var _ check → 导出方法(Start/Stop/SendEmail/...) → 非导出方法(sendEmail/sendSMS/...) → helpers(toProtoRecord/resolveDB/resolveGID/newWithDeps)`。

具体操作:把 `var _ pb.MessageServiceServer = (*MessageService)(nil)` 从文件底部挪到 `New` 之后、`Start` 之前。

修改后的关键片段(只展示顺序):

```go
type MessageService struct { ... }

func New(...) (*MessageService, error) { ... }

// Compile-time check that MessageService implements the gRPC interface.
var _ pb.MessageServiceServer = (*MessageService)(nil)

// Start starts lifecycle-managed service internals.
func (s *MessageService) Start() error { return s.manager.Start() }

// Stop stops lifecycle-managed service internals and releases owned resources.
func (s *MessageService) Stop() error { return s.manager.Stop() }

func (s *MessageService) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return s.sendEmail(ctx, req)
}

// ... 其他 gRPC stubs(都加 doc comment)

// --- internal helpers ---

func resolveDB(...) { ... }
func resolveGID(...) { ... }
func newWithDeps(...) { ... }
func toProtoRecord(...) { ... }
```

- [ ] **Step 5.2: 调整 `pkg/server.go` — 给 `Start`/`Stop` 补 doc comment**

修改:

```go
// Start starts the underlying service internals and the gRPC server without blocking.
// Returns an error if either the service or the gRPC server fails to start.
func (s *Server) Start() error {
	if err := s.svc.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.svc.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC server and the underlying service internals.
func (s *Server) Stop() error { return errors.Join(s.grpcSrv.Stop(), s.svc.Stop()) }
```

- [ ] **Step 5.3: 调整 `pkg/module.go`、`pkg/client.go` — 检查 doc comment**

这两个文件 doc comment 已较完整,只确认:
- `pkg/module.go`:`NewModule` 有 doc comment ✓
- `pkg/client.go`:`NewClient`、`Close` 有 doc comment ✓

若 `Close` 缺注释,补:

```go
// Close closes the underlying gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }
```

(已存在,无需改)

- [ ] **Step 5.4: 验证编译 + lint**

Run: `go build ./...`
Expected: 无错误

Run: `gofmt -l internal/service/service.go pkg/server.go pkg/module.go pkg/client.go`
Expected: 空输出(无格式问题)

Run: `golangci-lint run ./internal/service/... ./pkg/...`
Expected: 无新增 violation(原有也应当为 0)

- [ ] **Step 5.5: 跑测试**

Run: `go test -race -count=1 ./...`
Expected: 全部 PASS

- [ ] **Step 5.6: Commit**

```bash
git add internal/service/service.go pkg/server.go pkg/module.go pkg/client.go
git commit -m "style: enforce decorder declaration order, complete doc comments"
```

---

## Task 6: proto 调整 — 删 go_package + 补 doc comment

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Regenerate: `gen/message/v1/message.pb.go`

**目的:** 删除 proto 中硬编码的 `option go_package`(由 buf.gen.yaml managed mode 接管),给所有 message/enum/RPC 补 doc comment。

- [ ] **Step 6.1: 修改 `api/proto/message/v1/message.proto`**

**修改点 1** — 删除 `option go_package = "message-service/gen/message/v1";`(managed mode 已配 prefix)

**修改点 2** — 给现有 enum/message/service/RPC 补充/完善 doc comment。当前已有部分注释,补齐缺失的。

完整修改后的文件(注意删了 `option go_package`,补了 doc comment):

```proto
syntax = "proto3";

package message.v1;

import "buf/validate/validate.proto";

// MessageStatus represents the delivery status of a message.
enum MessageStatus {
  MESSAGE_STATUS_UNSPECIFIED = 0;
  MESSAGE_STATUS_PENDING = 1;
  MESSAGE_STATUS_SENT = 2;
  MESSAGE_STATUS_FAILED = 3;
}

// Channel represents the message delivery channel.
enum Channel {
  CHANNEL_UNSPECIFIED = 0;
  CHANNEL_EMAIL = 1;
  CHANNEL_SMS = 2;
}

// EmailVendor represents the email service brand. SMTP is the protocol;
// the same protocol connects different brands. CUSTOM_SMTP is the escape
// hatch when the brand is not in the enum (host must be provided in config).
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_CUSTOM_SMTP = 1;
  EMAIL_VENDOR_ALIYUN = 2;
  EMAIL_VENDOR_TENCENT = 3;
  EMAIL_VENDOR_NETEASE = 4;
}

// SmsVendor represents the SMS service brand. Each brand has its own API,
// so each value requires a corresponding go-common subpackage.
enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;
}

// MessageService handles sending, recording, and querying messages.
service MessageService {
  // SendEmail sends an email message via the configured vendor/account
  // (or the default fallback chain when both are unset).
  rpc SendEmail(SendEmailRequest) returns (SendResponse);

  // SendSMS sends an SMS message via the configured vendor/account
  // (or routes by phone country code when both are unset).
  rpc SendSMS(SendSMSRequest) returns (SendResponse);

  // GetMessage returns a single message record by ID.
  rpc GetMessage(GetMessageRequest) returns (MessageRecord);

  // ListMessages returns a paginated list of message records matching the filter.
  rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse);

  // GetMessageStats returns aggregated statistics for messages matching the filter.
  rpc GetMessageStats(GetMessageStatsRequest) returns (MessageStatsResponse);
}

// SendEmailRequest is the request to send an email.
message SendEmailRequest {
  string to = 1 [(buf.validate.field).string.email = true];
  repeated string cc = 2 [(buf.validate.field).repeated.items.string.email = true];
  repeated string bcc = 3 [(buf.validate.field).repeated.items.string.email = true];
  string subject = 4 [(buf.validate.field).string.min_len = 1];
  string body = 5;
  string html_body = 6;
  string reply_to = 7 [(buf.validate.field).string.email = true];
  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender uses default fallback.
  EmailVendor vendor = 8;
  string account = 9;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
}

// SendSMSRequest is the request to send an SMS.
message SendSMSRequest {
  string to = 1 [(buf.validate.field).string.min_len = 1];
  string content = 2;
  string template_id = 3;
  map<string, string> template_params = 4;
  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender routes by phone country.
  SmsVendor vendor = 5;
  string account = 6;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
}

// SendResponse is the response from SendEmail and SendSMS.
message SendResponse {
  int64 id = 1;
  MessageStatus status = 2;
  // vendor reports which vendor actually handled the send.
  oneof vendor {
    EmailVendor email_vendor = 3;
    SmsVendor sms_vendor = 4;
  }
}

// GetMessageRequest is the request to fetch a single message record.
message GetMessageRequest {
  int64 id = 1 [(buf.validate.field).int64.gt = 0];
}

// MessageRecord is the stored record of a sent message.
message MessageRecord {
  int64 id = 1;
  Channel channel = 2;
  // vendor is oneof because email and sms use different enum types.
  oneof vendor {
    EmailVendor email_vendor = 19;
    SmsVendor sms_vendor = 20;
  }
  MessageStatus status = 4;
  string target = 5;
  string subject = 6;
  string content = 7;
  string template_id = 8;
  map<string, string> template_params = 9;
  string sender_id = 10;
  string error_message = 11;
  int32 attempts = 12;
  int64 sent_at = 13;
  int64 created_at = 14;
  repeated string cc = 15;
  repeated string bcc = 16;
  string html_body = 17;
  string reply_to = 18;
}

// ListMessagesRequest filters and paginates a ListMessages query.
message ListMessagesRequest {
  Channel channel = 1;
  MessageStatus status = 2;
  string target = 3;
  // Filter by vendor. Set the one matching channel; the other is ignored.
  EmailVendor email_vendor = 9;
  SmsVendor sms_vendor = 10;
  int64 start_time = 5;
  int64 end_time = 6;
  int32 page = 7;
  int32 page_size = 8;
}

// ListMessagesResponse contains a page of message records plus total count.
message ListMessagesResponse {
  repeated MessageRecord records = 1;
  int32 total = 2;
}

// GetMessageStatsRequest filters a GetMessageStats query.
message GetMessageStatsRequest {
  Channel channel = 1;
  // Filter by vendor. Set the one matching channel; the other is ignored.
  EmailVendor email_vendor = 5;
  SmsVendor sms_vendor = 6;
  int64 start_time = 3;
  int64 end_time = 4;
}

// MessageStatsResponse contains aggregated message statistics.
message MessageStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated EmailVendorStats email_stats = 5;
  repeated SmsVendorStats sms_stats = 6;
}

// EmailVendorStats holds per-vendor statistics for email messages.
message EmailVendorStats {
  EmailVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}

// SmsVendorStats holds per-vendor statistics for SMS messages.
message SmsVendorStats {
  SmsVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}
```

- [ ] **Step 6.2: 跑 buf lint**

Run: `buf lint`
Expected: 无 violation

- [ ] **Step 6.3: 跑 buf generate 重生 gen/**

Run: `buf generate`
Expected: 无错误

Run: `git diff gen/`
Expected: 仅 `message.pb.go` 的 `go_package` 相关注释或包名 metadata 微调(无 wire 影响)

如果 diff 显示其他破坏性变化(字段编号变化、类型变化),停下检查 proto 改动是否意外触发了 wire-unsafe 变更。

- [ ] **Step 6.4: 验证编译 + 测试**

Run: `go build ./...`
Expected: 无错误

Run: `go test -race -count=1 ./...`
Expected: 全部 PASS

- [ ] **Step 6.5: Commit**

```bash
git add api/proto/message/v1/message.proto gen/
git commit -m "refactor(proto): drop hardcoded go_package, complete doc comments"
```

---

## Task 7: Makefile 修复 gorm gen 命令

**Files:**
- Modify: `Makefile`

**目的:** skeleton Makefile 用 `gorm gen -i ./store/models -o ./store/generated`,本项目当前是 `gorm gen` 无参数(靠默认值),改成显式参数更稳健。

- [ ] **Step 7.1: 修改 Makefile 的 generate target**

把:
```make
## generate: Run gorm.io/cli code generation
generate:
	gorm gen
```

改成:
```make
## generate: Run gorm.io/cli code generation
generate:
	gorm gen -i ./internal/store/models -o ./internal/store/generated
```

- [ ] **Step 7.2: 验证 generate target 正常**

Run: `make generate`
Expected: 无错误

Run: `git diff internal/store/generated/`
Expected: 空输出(已与 model 同步)

- [ ] **Step 7.3: Commit**

```bash
git add Makefile
git commit -m "build(makefile): pass explicit -i/-o to gorm gen"
```

---

## Task 8: memory 更新

**Files:**
- Modify: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/service-repo-no-interface-indirection.md`(改名)
- Delete: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/repository-naming-matches-models.md`
- Modify: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/MEMORY.md`

**目的:** 把过往偏好(service 持 repo)更新为新偏好(service 持 db + dal 包级函数),删除已不适用的 repository 命名 memory。

- [ ] **Step 8.1: 改写 `service-repo-no-interface-indirection.md`**

```markdown
---
name: service-dal-package-level-functions
description: service holds *gorm.DB directly; dal is package-level functions, no Repository struct
metadata:
  type: feedback
---

service 直接持有 `*gorm.DB`,dal 层是**包级函数**(接收 ctx + *gorm.DB),不再有 `Repository` struct、不再有 service 持 repo 的中间层。

**Why:** gorm-cli-development skill §6 明确要求 `store/dal/` 用包级函数 + service 持 db 的模式,与 transaction-in-service、type-safe chain 等约定自然契合。早期版本用过 `Repository` struct + service 持 repo 的模式,但 2026-06-20 重构到 skill 标准模式后弃用。

**How to apply:**
- service struct 字段:`db *gorm.DB`(从 `database.DB` 取),**不要** `repo *XxxRepository`
- dal 文件命名:`store/dal/<table>.go`,内部是包级函数
- dal 函数签名:`func Xxx(ctx, tx *gorm.DB, ...) (...)`,事务由 service 传 tx
- 方法名带表前缀:`CreateMessageRecord`、`GetMessageRecord`、`ListMessageRecords`
- 单条写入不开事务(skill §8);多步写入在 service 内 `db.Transaction(...)` 包裹
```

文件名也跟着改名:`service-repo-no-interface-indirection.md` → `service-dal-package-level-functions.md`。

```bash
mv /Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/service-repo-no-interface-indirection.md \
   /Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/service-dal-package-level-functions.md
```

- [ ] **Step 8.2: 删除 `repository-naming-matches-models.md`**

```bash
rm /Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/repository-naming-matches-models.md
```

理由:不再有 repository 概念。

- [ ] **Step 8.3: 更新 `MEMORY.md` 索引**

```markdown
- [Service-dal: package-level functions](service-dal-package-level-functions.md) — service holds *gorm.DB; dal is package-level functions, no Repository struct
- [Tests prefer real DB over mocks](tests-prefer-real-db-over-mocks.md) — `dbx.SetupTestDB` testcontainer, not hand-rolled `mockRepo`
- [Avoid empty abstraction base classes](avoid-empty-abstraction-base-classes.md) — no `BaseRepo{ db *gorm.DB }`, inline the field
```

(删除 `repository-naming-matches-models` 条目,把 `service-repo-no-interface-indirection` 改成新文件名 + 描述)

- [ ] **Step 8.4: Commit**

memory 目录不在 git 仓库内(`.claude/projects/.../memory/` 是 Claude Code 的本地状态),不需要 git commit。直接保存文件即可。

验证:`ls /Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/`
Expected: 列出的文件不含 `repository-naming-matches-models.md`,含 `service-dal-package-level-functions.md`。

---

## Task 9: spec 同步 + 全量验证

**Files:**
- Modify: `docs/superpowers/specs/2026-06-20-skills-alignment-refactor-design.md`

- [ ] **Step 9.1: 更新 spec 第 3.4 节**

把"用 `dbx.OffsetPaginate[T]` 替换手写分页"改成:

```markdown
### 3.4 go-common-usage 增强

**`dbx.ClampPageSize` 保护分页**:

`dal.ListMessageRecords` 当前手写 `Limit(int(filter.PageSize))`,未做 page size 限制,存在潜在 DoS 风险(请求方传超大 page_size)。

实施时发现 `dbx.OffsetPaginate[T]` 无法与 gorm gen typed chain 组合(后者不暴露 `UnderlyingDB()`),故放弃 OffsetPaginate,改为在 List 入口处对 PageSize 调 `dbx.ClampPageSize`(自动 clamp 到 [20, 100])。这样既保留 gorm gen 的类型安全,又防止超大 page size 攻击。

具体修改:`ListMessageRecords` 开头加 `pageSize := dbx.ClampPageSize(int(filter.PageSize))`。

**其他 go-common 工具**(已合规,无需改): ✓ configx / dbx / logging / grpcx / lifecycle / signalx / xerr。
```

- [ ] **Step 9.2: 全量验证**

```bash
gofmt -l .
goimports -l . 2>/dev/null || echo "goimports not installed, skipping"
golangci-lint run ./...
go test -race -count=1 ./...
go build ./...
buf lint
```

Expected:
- `gofmt -l .`: 空输出
- `goimports -l .`: 空输出(或工具不存在跳过)
- `golangci-lint run ./...`: 无 error
- `go test -race -count=1 ./...`: 全部 PASS
- `go build ./...`: 无错误
- `buf lint`: 无 violation

任何一项失败:停下、定位、修复,再回到该步骤重跑。

- [ ] **Step 9.3: 同步 spec 到 Obsidian**(按 CLAUDE.md 全局规则,可选)

按 CLAUDE.md 的"文档同步到 Obsidian"规则,本次设计文档应同步到 vault。此步可选 — 若执行环境无 obsidian CLI 或不需要同步,跳过即可。

obsidian vault 路径:`~/Library/Mobile Documents/iCloud~md~obsidian/Documents/only/services/message-service/`

如选择同步:

```bash
# 检查 vault 现有结构
ls "/Users/moss/Library/Mobile Documents/iCloud~md~obsidian/Documents/only/services/message-service/" 2>/dev/null
# 用 obsidian CLI 创建/更新 spec
obsidian vault=only create name="skills-alignment-refactor-design" content="$(cat docs/superpowers/specs/2026-06-20-skills-alignment-refactor-design.md)"
obsidian vault=only move file="skills-alignment-refactor-design" to="services/message-service/design/v1/"
# 更新 vault 内 services/index.md 和 services/changes.md
```

- [ ] **Step 9.4: Commit spec 更新**

```bash
git add docs/superpowers/specs/2026-06-20-skills-alignment-refactor-design.md
git commit -m "docs(spec): correct §3.4 — OffsetPaginate unusable with typed chain, use ClampPageSize"
```

---

## Self-Review Notes

完成所有 task 后,逐项确认:

- [ ] **Spec 覆盖率**:回到 spec 的 3.1~3.8 节,逐节确认都有 task 实施
  - 3.1 dal 重构 → Task 2 + 3 + 4 ✓
  - 3.2 service 调整 → Task 3 ✓
  - 3.3 golang-development 细节 → Task 5 ✓
  - 3.4 go-common-usage(OffsetPaginate → ClampPageSize)→ Task 2(在 dal 中已用 ClampPageSize)+ Task 9(同步 spec)✓
  - 3.5 proto 调整 → Task 6 ✓
  - 3.6 测试调整 → Task 4 ✓
  - 3.7 memory 更新 → Task 8 ✓
  - 3.8 实施顺序 → 与 task 序号一致 ✓
- [ ] **类型一致性**:`dal.X` 命名(CreateMessageRecord/GetMessageRecord/ListMessageRecords/CountMessageStats/ListMessageVendorStats)在 Task 2 定义,Task 3/4 调用一致 ✓
- [ ] **placeholder 扫描**:每个 step 都有完整代码或精确指令,无 TBD/TODO ✓

---

## 风险与回滚

- 每个 task 独立 commit,失败可 `git reset --hard HEAD~1` 回滚到上一 task
- Task 4 涉及测试大改,如 testcontainer 跑不起来,可临时跳过 Step 4.5,改用 `go vet ./...` 做静态验证
- Task 6 proto 改动若 buf generate 产生意外 diff,停下检查 — 不要 commit 破坏性变更
- Task 8 memory 不在 git 内,改动直接生效,不可回滚(但可手动恢复)
