# Service 架构层重构 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按 `golang-service-development` skill 重构 message-service:vendor 协议封装挪到 `internal/provider/`,加 `internal/jobs/` 包,加 handler/service 分层,子包化业务逻辑,删 ownDB bool,启用 HTTP gateway,补 DevOps 文件。

**Architecture:** 自下而上:先把 `internal/message/` 改名为 `internal/provider/`,加 `internal/jobs/` 包(独立 task),再拆 service.go 为 facade + message 子包,创建 pkg/handler 薄壳,改 server.go/module.go 引用新结构,资源管理改 lifecycle.StopFunc,接入 jobs.Scheduler,启用 HTTP gateway,补 DevOps 文件,迁移测试,更新 memory。

**Tech Stack:** Go 1.26、gorm.io/cli v0.2.4、go-common(lifecycle / grpcx / signalx / dbx / configx / xerr / cronx)、buf v2 + protovalidate + grpc-gateway、robfig/cron/v3。

**Spec:** `docs/superpowers/specs/2026-06-21-service-architecture-refactor-design.md`

---

## File Structure

修改/创建的文件:

| 文件 | 操作 | 职责 |
|---|---|---|
| `internal/provider/email/` | 改名自 `internal/message/email/` | 邮件 provider(SMTP/Aliyun SDK 封装) |
| `internal/provider/sms/` | 改名自 `internal/message/sms/` | 短信 provider |
| `internal/jobs/jobs.go` | 新建 | jobs.Scheduler 实现 lifecycle.Service(空架子) |
| `internal/service/service.go` | 重写 | Service 本体 + New + Start/Stop + setupJobs + 5 个 facade |
| `internal/service/send.go` | 删除 | 业务迁到 message 子包 |
| `internal/service/query.go` | 删除 | 业务迁到 message 子包 |
| `internal/service/service_test.go` | 删除/迁移 | 测试迁到 message 子包 |
| `internal/service/message/message.go` | 新建 | 子包业务实现 |
| `internal/service/message/message_test.go` | 新建 | 子包测试 |
| `pkg/handler/message.go` | 新建 | Handler 薄壳 + 5 个 RPC 委托 + Start/Stop |
| `pkg/server.go` | 重写 | Server 持 grpcSrv + hdl,启用 gateway |
| `pkg/module.go` | 重写 | NewModule 返回 *handler.Handler |
| `pkg/config/config.go` | 改 | 加 `Cron *CronConfig` 字段,Email/SMS import 改 path |
| `internal/store/models/base.go` | 改 | 删 Database / NewDatabase / Stop,保留 AllModels() |
| `api/proto/message/v1/message.proto` | 改 | 加 google.api.http annotation |
| `gen/message/v1/message.pb.gw.go` | 新建 | buf 自动生成 |
| `Makefile` | 改 | 加 migrate target |
| `Dockerfile` | 新建 | 多阶段构建,distroless |
| `.golangci.yml` | 新建 | 复用 ai-kit-studio 模板 |

---

## Task 1: 改名 `internal/message/` → `internal/provider/`

**Files:**
- Rename: `internal/message/` → `internal/provider/`
- Modify: 所有引用 `message-service/internal/message` 的 `.go` 文件

**目的:** vendor 协议封装(SMTP/Aliyun SDK 调用层)挪到 skill §1 标准目录 `internal/provider/`。

- [ ] **Step 1.1: 用 git mv 改名**

```bash
git mv internal/message internal/provider
```

这会移动整个目录(`internal/provider/email/` 和 `internal/provider/sms/`)。

- [ ] **Step 1.2: 更新所有 import 路径**

找出所有引用:

```bash
grep -rln "message-service/internal/message" --include="*.go" .
```

Expected 输出(基于当前代码):
- `internal/service/service.go`
- `internal/service/send.go`
- `internal/service/query.go`
- `internal/service/service_test.go`
- `pkg/config/config.go`

每个文件用 sed 替换:

```bash
grep -rl "message-service/internal/message" --include="*.go" . | \
    xargs sed -i '' 's|message-service/internal/message|message-service/internal/provider|g'
```

(macOS 用 `sed -i ''`,Linux 用 `sed -i`。)

- [ ] **Step 1.3: 检查文件内的包注释**

读 `internal/provider/email/sender.go` 和 `internal/provider/sms/sender.go` 的包注释,确认还是 `package email` / `package sms`(不变,因为目录改名后包名跟随目录名)。

注意:`internal/provider/email/registry.go` 等文件里的注释可能提到 "email" 或 "message",确认含义不变。

- [ ] **Step 1.4: 验证编译**

```bash
go build ./...
```

Expected: 无错误。

- [ ] **Step 1.5: 跑非 testcontainer 测试**

```bash
go test -count=1 ./internal/provider/... ./pkg/xcodes/...
```

Expected: 全过(email/sms/xcodes 包测试)。

- [ ] **Step 1.6: Commit**

```bash
git status
git add -A
git commit -m "refactor(provider): rename internal/message → internal/provider per skill §1"
```

---

## Task 2: 创建 `internal/jobs/jobs.go`

**Files:**
- Create: `internal/jobs/jobs.go`

**目的:** 按 skill §5 加 jobs.Scheduler(空架子)。直接拷贝 demo-service 的 jobs 包。

- [ ] **Step 2.1: 创建 `internal/jobs/jobs.go`**

完整内容(直接拷贝 `demo-service/internal/jobs/jobs.go`,无业务逻辑改动):

```go
// Package jobs owns the cron scheduler for periodic background work. The
// Scheduler type is a pure cron wrapper: it knows how to build (or accept)
// a cron.Cron, expose AddFunc for callers to register jobs, and adapt to
// lifecycle.Service. It does NOT know which domain jobs to run — the parent
// service decides that and calls AddFunc inside its setupJobs method.
package jobs

import (
	"fmt"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/robfig/cron/v3"
)

// Scheduler wraps a cron.Cron and adapts it to lifecycle.Service. Callers
// register periodic work via AddFunc; Start launches the scheduler, Stop
// blocks until in-flight jobs drain — but ONLY when Scheduler owns the cron
// (built it itself). When a cron is injected via Deps.Cron, the caller owns
// its lifecycle and Start/Stop are no-ops; robfig/cron's Stop() is not
// idempotent (each call spawns a goroutine waiting on jobWaiter), so a
// borrowed cron must be lifecycle-managed by exactly one owner.
type Scheduler struct {
	cron     *cron.Cron
	ownsCron bool
}

// Deps injects the cronx.Config used to build a new cron, and an optional
// pre-built cron. If Cron is non-nil, Config is ignored and the caller
// retains lifecycle ownership (Start/Stop become no-ops on the Scheduler).
type Deps struct {
	Config *cronx.Config
	Cron   *cron.Cron
}

// Compile-time assertion that *Scheduler satisfies lifecycle.Service.
var _ lifecycle.Service = (*Scheduler)(nil)

// New returns a Scheduler wrapping either the injected cron or a freshly
// built one. At least one of Deps.Config or Deps.Cron must be non-nil.
func New(d *Deps) (*Scheduler, error) {
	c := d.Cron
	owns := false
	if c == nil {
		if d.Config == nil {
			return nil, fmt.Errorf("jobs: Deps.Config required when Deps.Cron is nil")
		}
		var err error
		c, err = cronx.New(d.Config)
		if err != nil {
			return nil, fmt.Errorf("jobs: init cron: %w", err)
		}
		owns = true
	}
	return &Scheduler{cron: c, ownsCron: owns}, nil
}

// AddFunc registers a periodic job on the scheduler. Caller picks the spec
// and the function; Scheduler does not interpret either.
func (s *Scheduler) AddFunc(spec string, cmd func()) error {
	if _, err := s.cron.AddFunc(spec, cmd); err != nil {
		return fmt.Errorf("jobs: add func: %w", err)
	}
	return nil
}

// Start launches the cron scheduler. No-op when the cron was injected via
// Deps.Cron (caller owns lifecycle in that case).
func (s *Scheduler) Start() error {
	if s.ownsCron {
		s.cron.Start()
	}
	return nil
}

// Stop signals the scheduler to halt and blocks until all in-flight jobs
// finish. No-op when the cron was injected via Deps.Cron.
func (s *Scheduler) Stop() error {
	if s.ownsCron {
		<-s.cron.Stop().Done()
	}
	return nil
}
```

- [ ] **Step 2.2: 验证编译**

```bash
go build ./internal/jobs/...
```

Expected: 无错误。

- [ ] **Step 2.3: Commit**

```bash
git add internal/jobs/jobs.go
git commit -m "feat(jobs): add cronx-based Scheduler (lifecycle.Service adapter)"
```

---

## Task 3: 拆 service.go — 创建 message 子包 + Service facade

**Files:**
- Create: `internal/service/message/message.go`
- Modify: `internal/service/service.go`(重写)
- Delete: `internal/service/send.go`, `internal/service/query.go`

**目的:** 按 skill §2 要求,service.go 只装本体 + facade,业务逻辑全部下沉到 `internal/service/message/` 子包。

- [ ] **Step 3.1: 创建 `internal/service/message/message.go`**

完整内容(直接写入):

```go
// Package message contains the message domain business logic.
//
// Layering contract (see golang-service-development skill §2):
//   - This is a SUBPACKAGE under internal/service/. Message methods are
//     NOT on the outer *service.Service; the outer service exposes them via
//     one-line facade methods (see ../service.go).
//   - Methods take proto types DIRECTLY and return proto types — no
//     intermediate Go structs. Conversion happens at the store boundary.
//   - Resources (db, gid, vendor registries) are injected via New; the
//     subpackage does NOT manage their lifecycle.
package message

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/provider/email"
	"message-service/internal/provider/sms"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/thirdcall"
	"message-service/pkg/xcodes"

	emailcommon "github.com/servekit/go-common/message/email"
	smscommon "github.com/servekit/go-common/message/sms"

	"gorm.io/gorm"
)

// Service is the message domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db            *gorm.DB
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router // nil when no routes configured
}

// New constructs a message domain service with injected resources.
func New(
	db *gorm.DB,
	gid thirdcall.GIDService,
	emailRegistry *email.AccountRegistry,
	smsRegistry *sms.AccountRegistry,
	smsRouter *sms.Router,
) *Service {
	return &Service{
		db:            db,
		gid:           gid,
		emailRegistry: emailRegistry,
		smsRegistry:   smsRegistry,
		smsRouter:     smsRouter,
	}
}

// SendEmail sends an email via the configured vendor/account, or the default
// fallback chain when both are unset.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	vendorStr := emailVendorToString(req.GetVendor())
	sender, err := s.emailRegistry.SenderFor(vendorStr, req.GetAccount())
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	id, err := s.gid.NextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	msg := &emailcommon.Message{
		To:       req.GetTo(),
		Cc:       req.GetCc(),
		Bcc:      req.GetBcc(),
		Subject:  req.GetSubject(),
		Body:     req.GetBody(),
		HTMLBody: req.GetHtmlBody(),
		ReplyTo:  req.GetReplyTo(),
	}

	result, err := sender.Send(ctx, msg)
	if err != nil {
		if result != nil {
			s.persistEmailRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(err)
	}

	s.persistEmailRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_EmailVendor{
			EmailVendor: emailVendorFromString(result.Vendor),
		},
	}, nil
}

// SendSMS sends an SMS via the configured vendor/account, or routes by phone
// country code when both are unset.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	id, err := s.gid.NextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	msg := &smscommon.Message{
		To:       req.GetTo(),
		Content:  req.GetContent(),
		Template: req.GetTemplateId(),
		Params:   models.MapStringString(req.GetTemplateParams()),
	}

	var result *sms.SendResult
	var sendErr error
	if req.GetVendor() != 0 && req.GetAccount() != "" {
		vendorStr := smsVendorToString(req.GetVendor())
		sender, err := s.smsRegistry.SenderFor(vendorStr, req.GetAccount())
		if err != nil {
			return nil, xcodes.ErrBadRequest.Wrap(err)
		}
		result, sendErr = sender.Send(ctx, msg)
	} else {
		if s.smsRouter == nil {
			return nil, xcodes.ErrBadRequest.New("sms routes not configured; specify vendor and account explicitly")
		}
		result, sendErr = s.smsRouter.Send(ctx, msg)
	}

	if sendErr != nil {
		if result != nil {
			s.persistSMSRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(sendErr)
	}

	s.persistSMSRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{
			SmsVendor: smsVendorFromString(result.Vendor),
		},
	}, nil
}

// GetMessage returns a single message record by ID.
func (s *Service) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	record, err := dal.GetMessageRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoRecord(record), nil
}

// ListMessages returns a paginated list of message records matching the filter.
func (s *Service) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
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

// GetMessageStats returns aggregated statistics for messages matching the filter.
func (s *Service) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
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

// --- record persistence (synchronous, error-logged) ---

func (s *Service) persistEmailRecord(ctx context.Context, id int64, req *pb.SendEmailRequest, result *email.SendResult) {
	record := &models.MessageRecord{
		ID:          id,
		Channel:     int32(pb.Channel_CHANNEL_EMAIL),
		Target:      req.GetTo(),
		Cc:          models.StringSlice(req.GetCc()),
		Bcc:         models.StringSlice(req.GetBcc()),
		Subject:     req.GetSubject(),
		Content:     req.GetBody(),
		HTMLBody:    req.GetHtmlBody(),
		ReplyTo:     req.GetReplyTo(),
		Attempts:    result.Attempts,
		EmailVendor: int32(emailVendorFromString(result.Vendor)),
		Account:     result.Account,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = result.Error.Error()
		}
	}

	if err := dal.CreateMessageRecord(ctx, s.db, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
	}
}

func (s *Service) persistSMSRecord(ctx context.Context, id int64, req *pb.SendSMSRequest, result *sms.SendResult) {
	record := &models.MessageRecord{
		ID:             id,
		Channel:        int32(pb.Channel_CHANNEL_SMS),
		Target:         req.GetTo(),
		Content:        req.GetContent(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
		SmsVendor:      int32(smsVendorFromString(result.Vendor)),
		Account:        result.Account,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = result.Error.Error()
		}
	}

	if err := dal.CreateMessageRecord(ctx, s.db, record); err != nil {
		slog.Error("persist sms record", "record_id", id, "error", err)
	}
}

// --- proto ↔ model conversion ---

func toProtoRecord(r *models.MessageRecord) *pb.MessageRecord {
	rec := &pb.MessageRecord{
		Id:             r.ID,
		Channel:        pb.Channel(r.Channel),
		Status:         pb.MessageStatus(r.Status),
		Target:         r.Target,
		Cc:             []string(r.Cc),
		Bcc:            []string(r.Bcc),
		Subject:        r.Subject,
		Content:        r.Content,
		HtmlBody:       r.HTMLBody,
		ReplyTo:        r.ReplyTo,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		SenderId:       r.SenderID,
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
	}
	switch pb.Channel(r.Channel) {
	case pb.Channel_CHANNEL_EMAIL:
		rec.Vendor = &pb.MessageRecord_EmailVendor{EmailVendor: pb.EmailVendor(r.EmailVendor)}
	case pb.Channel_CHANNEL_SMS:
		rec.Vendor = &pb.MessageRecord_SmsVendor{SmsVendor: pb.SmsVendor(r.SmsVendor)}
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}

// --- vendor enum ↔ string helpers ---

func emailVendorToString(v pb.EmailVendor) string {
	switch v {
	case pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP:
		return "custom_smtp"
	case pb.EmailVendor_EMAIL_VENDOR_ALIYUN:
		return "aliyun"
	case pb.EmailVendor_EMAIL_VENDOR_TENCENT:
		return "tencent"
	case pb.EmailVendor_EMAIL_VENDOR_NETEASE:
		return "netease"
	default:
		return ""
	}
}

func smsVendorToString(v pb.SmsVendor) string {
	switch v {
	case pb.SmsVendor_SMS_VENDOR_ALIYUN:
		return "aliyun"
	default:
		return ""
	}
}

func emailVendorFromString(s string) pb.EmailVendor {
	switch s {
	case "custom_smtp":
		return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	default:
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	}
}

func smsVendorFromString(s string) pb.SmsVendor {
	switch s {
	case "aliyun":
		return pb.SmsVendor_SMS_VENDOR_ALIYUN
	default:
		return pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	}
}
```

- [ ] **Step 3.2: 删除 `internal/service/send.go` 和 `internal/service/query.go`**

```bash
git rm internal/service/send.go internal/service/query.go
```

- [ ] **Step 3.3: 重写 `internal/service/service.go`**

完整内容(直接覆盖):

```go
// Package service contains message-service business logic.
//
// Layering contract (see golang-service-development skill §2):
//   - This is the SERVICE ROOT. It holds Service struct + New + Start/Stop +
//     resource resolve helpers + setupJobs + one-line facade methods (one per RPC).
//   - Business logic lives in SUBPACKAGES (internal/service/<domain>/). This
//     file does NOT contain CRUD implementations — only delegations.
//   - handler calls service.X; service.X is a one-line facade that calls
//     s.<domain>.X in the subpackage.
//   - Service methods take proto types DIRECTLY and return proto types.
package service

import (
	"context"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	pb "message-service/gen/message/v1"
	"message-service/internal/provider/email"
	"message-service/internal/provider/sms"
	"message-service/internal/service/message"
	"message-service/internal/store/models"
	"message-service/pkg/config"
	"message-service/pkg/option"
	"message-service/pkg/thirdcall"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
)

// Service holds message-service business state.
type Service struct {
	cfg *config.Config
	mgr *lifecycle.Manager

	database *models.Database // lifecycle owner; Task 6 will remove
	db       *gorm.DB         // query handle, alias of database.DB
	gid      thirdcall.GIDService

	message *message.Service
}

// New constructs a Service from config and functional options.
//
// NOTE: This intermediate version still uses models.Database wrapper for DB
// lifecycle (Task 6 will replace it with lifecycle.StopFunc). setupJobs
// wiring is added in Task 7.
func New(cfg *config.Config, opts ...option.Option) (*Service, error) {
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

	emailRegistry, err := email.NewAccountRegistry(cfg.Email)
	if err != nil {
		if stopErr := database.Stop(); stopErr != nil {
			slog.Error("cleanup database during init failure", "error", stopErr)
		}
		return nil, fmt.Errorf("email registry: %w", err)
	}

	smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
	if err != nil {
		if stopErr := database.Stop(); stopErr != nil {
			slog.Error("cleanup database during init failure", "error", stopErr)
		}
		return nil, fmt.Errorf("sms registry: %w", err)
	}

	smsRouter, err := sms.BuildRouter(cfg.SMS, smsRegistry)
	if err != nil {
		if stopErr := database.Stop(); stopErr != nil {
			slog.Error("cleanup database during init failure", "error", stopErr)
		}
		return nil, fmt.Errorf("sms router: %w", err)
	}

	svc := &Service{
		cfg:      cfg,
		database: database,
		db:       database.DB,
		gid:      gid,
		message:  message.New(db, gid, emailRegistry, smsRegistry, smsRouter),
	}
	svc.mgr = lifecycle.NewManager()
	svc.mgr.AddStopper("db", database)
	return svc, nil
}

// Start starts lifecycle-managed service internals.
func (s *Service) Start() error { return s.mgr.Start() }

// Stop stops lifecycle-managed service internals and releases owned resources.
func (s *Service) Stop() error { return s.mgr.Stop() }

// --- facade methods (one per RPC, delegate to subpackage) ---

// SendEmail delegates to the message subpackage.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return s.message.SendEmail(ctx, req)
}

// SendSMS delegates to the message subpackage.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return s.message.SendSMS(ctx, req)
}

// GetMessage delegates to the message subpackage.
func (s *Service) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	return s.message.GetMessage(ctx, req)
}

// ListMessages delegates to the message subpackage.
func (s *Service) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	return s.message.ListMessages(ctx, req)
}

// GetMessageStats delegates to the message subpackage.
func (s *Service) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	return s.message.GetMessageStats(ctx, req)
}

// --- internal helpers ---

func resolveDB(o *option.Options, cfg *config.Config) (db *gorm.DB, own bool, err error) {
	if o.DB != nil {
		return o.DB, false, nil
	}
	db, err = dbx.New(cfg.Database)
	if err != nil {
		return nil, false, fmt.Errorf("database: %w", err)
	}
	return db, true, nil
}

func resolveGID(cfg *config.Config, external thirdcall.GIDService) (thirdcall.GIDService, error) {
	if external != nil {
		return external, nil
	}
	return thirdcall.NewGIDService(cfg.ThirdParty.GID)
}
```

- [ ] **Step 3.4: 验证编译(非测试)**

```bash
go build ./internal/service/message/... ./internal/store/...
go build ./internal/service/
```

Expected: 无错误。(`service_test.go` 此刻仍引用旧 `MessageService`,编译失败是预期的 — Task 10 处理测试。)

- [ ] **Step 3.5: Commit**

```bash
git status
git add internal/service/service.go internal/service/message/message.go
git rm internal/service/send.go internal/service/query.go
git commit -m "refactor(service): split service.go into facade + message subpackage"
```

---

## Task 4: 创建 pkg/handler/

**Files:**
- Create: `pkg/handler/message.go`

**目的:** 按 skill §1 创建 handler 薄壳。

- [ ] **Step 4.1: 创建 `pkg/handler/message.go`**

完整内容:

```go
// Package handler implements message.v1.MessageServiceServer as a thin shim
// over internal/service. Each method is a one-line delegation.
//
// Handlers hold NO business logic and NO conversion logic. Anything beyond
// `return h.svc.X(ctx, req)` belongs in internal/service.
//
// Handler also implements signalx.Service (Start/Stop) by delegating to the
// underlying Service.
package handler

import (
	"context"

	pb "message-service/gen/message/v1"
	"message-service/internal/service"
)

// Handler implements message.v1.MessageServiceServer.
type Handler struct {
	pb.UnimplementedMessageServiceServer

	svc *service.Service
}

// New constructs a Handler wrapping svc.
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Compile-time assertion: Handler implements the gRPC server interface.
var _ pb.MessageServiceServer = (*Handler)(nil)

// Start starts service-internal components.
func (h *Handler) Start() error { return h.svc.Start() }

// Stop releases resources owned by the service.
func (h *Handler) Stop() error { return h.svc.Stop() }

// SendEmail delegates to service.SendEmail.
func (h *Handler) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return h.svc.SendEmail(ctx, req)
}

// SendSMS delegates to service.SendSMS.
func (h *Handler) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return h.svc.SendSMS(ctx, req)
}

// GetMessage delegates to service.GetMessage.
func (h *Handler) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	return h.svc.GetMessage(ctx, req)
}

// ListMessages delegates to service.ListMessages.
func (h *Handler) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	return h.svc.ListMessages(ctx, req)
}

// GetMessageStats delegates to service.GetMessageStats.
func (h *Handler) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	return h.svc.GetMessageStats(ctx, req)
}
```

- [ ] **Step 4.2: 验证编译**

```bash
go build ./pkg/handler/...
```

Expected: 无错误。

- [ ] **Step 4.3: Commit**

```bash
git add pkg/handler/message.go
git commit -m "feat(handler): add thin gRPC stub delegate to service.Service"
```

---

## Task 5: 改 pkg/server.go + pkg/module.go

**Files:**
- Modify: `pkg/server.go`
- Modify: `pkg/module.go`

**目的:** Server 通过 Handler 注册 gRPC service;NewModule 返回 *Handler。HTTP gateway 暂不启用(Task 8 做)。

- [ ] **Step 5.1: 重写 `pkg/server.go`**

完整内容:

```go
package messageservice

import (
	"errors"

	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/signalx"

	pb "message-service/gen/message/v1"
	"message-service/internal/service"
	"message-service/pkg/config"
	"message-service/pkg/handler"
	"message-service/pkg/option"
)

// Compile-time assertion: *Server satisfies signalx.Service.
var _ signalx.Service = (*Server)(nil)

// Server wraps a gRPC + HTTP gateway server for message-service.
type Server struct {
	grpcSrv *grpcx.Server
	hdl     *handler.Handler
}

// ServerOption configures a Server instance.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []option.Option
}

// WithServiceOptions forwards options to the service layer.
func WithServiceOptions(opts ...option.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// NewServer constructs a Server with all dependencies wired.
//
// NOTE: HTTP gateway is currently disabled (registerGW=nil). Task 8 will
// enable it after the proto gains google.api.http annotations.
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var so serverOptions
	for _, opt := range opts {
		opt(&so)
	}

	svc, err := service.New(cfg, so.serviceOpts...)
	if err != nil {
		return nil, err
	}

	hdl := handler.New(svc)

	validator, err := protovalidate.New()
	if err != nil {
		return nil, err
	}

	grpcSrv := grpcx.New(
		grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPCAddr,
			GatewayAddr: cfg.Server.HTTPAddr,
		},
		func(s *grpc.Server) { pb.RegisterMessageServiceServer(s, hdl) },
		nil, // gateway disabled until Task 8
		grpcx.ErrorInterceptor,
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)

	return &Server{grpcSrv: grpcSrv, hdl: hdl}, nil
}

// Start starts service internals and the gRPC server without blocking.
func (s *Server) Start() error {
	if err := s.hdl.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.hdl.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC server and service internals.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.hdl.Stop())
}
```

- [ ] **Step 5.2: 重写 `pkg/module.go`**

完整内容:

```go
// Package messageservice provides in-process and gRPC client access to
// message-service.
package messageservice

import (
	pb "message-service/gen/message/v1"
	"message-service/internal/service"
	"message-service/pkg/config"
	"message-service/pkg/handler"
	"message-service/pkg/option"
)

// Handler is the in-process entry point. Aliased to *handler.Handler so
// external code references it as messageservice.Handler.
type Handler = handler.Handler

// Compile-time assertion: *Handler satisfies the gRPC server interface.
var _ pb.MessageServiceServer = (*Handler)(nil)

// NewModule constructs an in-process message-service for embedding.
//
// Returns only the Handler — Handler IS the public capability and ALSO
// satisfies signalx.Service (Start/Stop).
func NewModule(cfg *config.Config, opts ...option.Option) (*Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return handler.New(svc), nil
}
```

注意:原 `pkg/module.go` 有 `type MessageService = service.MessageService` alias,要删除(Task 3 已重命名为 `service.Service`,旧 alias 失效)。

- [ ] **Step 5.3: 检查 `MessageService` 残留引用**

```bash
grep -rn "MessageService\b" --include="*.go" . | grep -v "_test.go\|/gen/\|Unimplemented\|MessageServiceServer\|MessageServiceClient"
```

Expected: 为空(只有 handler 的 embed 和 gen/ 的接口)。如果有遗漏,改成 `Service` 或 `Handler`。

- [ ] **Step 5.4: 验证编译**

```bash
go build ./pkg/...
```

Expected: 无错误。

- [ ] **Step 5.5: Commit**

```bash
git status
git add pkg/server.go pkg/module.go
git commit -m "refactor(pkg): Server wraps Handler; NewModule returns *Handler"
```

---

## Task 6: 资源管理重构 — 删 ownDB,用 lifecycle.StopFunc

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/store/models/base.go`
- Modify: `internal/service/service_test.go`(临时,Task 10 整体重写)

**目的:** 删 `models.Database{ owned bool }` 反模式,统一用 `lifecycle.Manager` 注册 Stopper。

- [ ] **Step 6.1: 改 `internal/service/service.go`**

**修改 1** — Service struct 删除 `database` 字段:

```go
type Service struct {
	cfg *config.Config
	mgr *lifecycle.Manager

	db  *gorm.DB
	gid thirdcall.GIDService

	message *message.Service
}
```

**修改 2** — `New` 函数重写(用 mgr + resolveDB/resolveGID 新签名):

```go
func New(cfg *config.Config, opts ...option.Option) (*Service, error) {
	o := option.Apply(opts...)
	mgr := lifecycle.NewManager()

	db, err := resolveDB(cfg, o.DB, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after db resolve failure", "error", cerr)
		}
		return nil, err
	}
	gid, err := resolveGID(cfg, o.GIDService, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after gid resolve failure", "error", cerr)
		}
		return nil, err
	}

	emailRegistry, err := email.NewAccountRegistry(cfg.Email)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after email registry failure", "error", cerr)
		}
		return nil, fmt.Errorf("email registry: %w", err)
	}
	smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after sms registry failure", "error", cerr)
		}
		return nil, fmt.Errorf("sms registry: %w", err)
	}
	smsRouter, err := sms.BuildRouter(cfg.SMS, smsRegistry)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after sms router failure", "error", cerr)
		}
		return nil, fmt.Errorf("sms router: %w", err)
	}

	return &Service{
		cfg:     cfg,
		mgr:     mgr,
		db:      db,
		gid:     gid,
		message: message.New(db, gid, emailRegistry, smsRegistry, smsRouter),
	}, nil
}
```

**修改 3** — `resolveDB` / `resolveGID` 新签名:

```go
func resolveDB(cfg *config.Config, injected *gorm.DB, mgr *lifecycle.Manager) (*gorm.DB, error) {
	if injected != nil {
		return injected, nil
	}
	db, err := dbx.New(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	mgr.AddStopper("db", lifecycle.StopFunc(func() {
		sqlDB, err := db.DB()
		if err != nil {
			slog.Warn("get sql db for close", "error", err)
			return
		}
		if err := sqlDB.Close(); err != nil {
			slog.Warn("close db", "error", err)
		}
	}))
	return db, nil
}

func resolveGID(cfg *config.Config, injected thirdcall.GIDService, mgr *lifecycle.Manager) (thirdcall.GIDService, error) {
	if injected != nil {
		return injected, nil
	}
	gid, err := thirdcall.NewGIDService(cfg.ThirdParty.GID)
	if err != nil {
		return nil, fmt.Errorf("gid service: %w", err)
	}
	if closer, ok := gid.(interface{ Close() error }); ok {
		mgr.AddStopper("gid", lifecycle.StopFunc(func() {
			if err := closer.Close(); err != nil {
				slog.Warn("close gid", "error", err)
			}
		}))
	}
	return gid, nil
}
```

**修改 4** — 删除 `models` import(service.go 不再用 `models.Database`)。

- [ ] **Step 6.2: 删 `models.Database` 类型**

读 `internal/store/models/base.go`,删除 `Database` / `NewDatabase` / `(*Database).Stop`。保留 `AllModels()`。

修改后的 `base.go`:

```go
package models

// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&MessageRecord{},
	}
}
```

- [ ] **Step 6.3: 检查 cmd/migrate/main.go**

```bash
grep -n "models.Database\|models.NewDatabase" cmd/migrate/main.go
```

Expected: 无引用(migrate 用 `dbx.New` 直接)。

- [ ] **Step 6.4: 临时修复 `service_test.go` 让代码可编译**

读当前 `service_test.go`,把所有 `database: models.NewDatabase(db, false)` 字段赋值删除(`&MessageService{...}` 改成 `&Service{ db: db, ... }`,不设 database)。

注意:此时 `MessageService` 类型已重命名为 `Service`,所以 test 文件里的 `*MessageService` 也要改成 `*Service`。这只是临时编译修复,Task 10 整体重写测试。

具体改动模式:
```go
// 之前
return &MessageService{
    database:      models.NewDatabase(db, false),
    db:            db,
    gid:           getTestGID(t),
    manager:       lifecycle.NewManager(),
    emailRegistry: ...,
}, repo

// 之后(临时)
return &Service{
    db:            db,
    gid:           getTestGID(t),
    mgr:           lifecycle.NewManager(),
    emailRegistry: ...,
}
```

注意:`manager` 字段名也改成 `mgr`(Task 3 已改)。

- [ ] **Step 6.5: 验证编译**

```bash
go build ./...
```

Expected: 无错误。

- [ ] **Step 6.6: Commit**

```bash
git status
git add internal/service/service.go internal/store/models/base.go internal/service/service_test.go
git commit -m "refactor(service): replace Database.owned bool with lifecycle.StopFunc"
```

---

## Task 7: 接入 jobs.Scheduler(config 加 Cron 字段 + setupJobs)

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `internal/service/service.go`(加 setupJobs)
- Modify: `config.yaml`(加 cron 配置)

**目的:** 按 skill §5,service.New 末尾调 setupJobs(),把 jobs.Scheduler 注册到 lifecycle.Manager。

- [ ] **Step 7.1: 改 `pkg/config/config.go`**

读当前文件,加 `Cron *CronConfig` 字段和 `CronConfig` 类型。在 `Config` struct 里加字段,在文件末尾加类型:

```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Log        *logging.Config
	Email      *email.Config
	SMS        *sms.Config
	Cron       *CronConfig          // 新增
	ThirdParty *ThirdPartyConfig
}

// CronConfig configures jobs.Scheduler's cronx instance.
type CronConfig struct {
	Timezone string `default:"Asia/Shanghai"`
}
```

注意:`email.Config` 和 `sms.Config` 的 import 路径在 Task 1 改名后是 `message-service/internal/provider/email` 和 `message-service/internal/provider/sms`(Task 1 已改完)。如果 Task 1 完成后 config.go 没自动改 import,这里一并改。

- [ ] **Step 7.2: 改 `internal/service/service.go` 加 setupJobs**

读当前文件,加 `jobs` 和 `cronx` import,加 setupJobs 方法,在 New() 末尾调用:

**import 改动**:
```go
import (
	// ...
	"message-service/internal/jobs"

	"github.com/servekit/go-common/cronx"
	// ...
)
```

**在 New() return 之前加**:
```go
svc := &Service{
	cfg:     cfg,
	mgr:     mgr,
	db:      db,
	gid:     gid,
	message: message.New(db, gid, emailRegistry, smsRegistry, smsRouter),
}

if err := svc.setupJobs(); err != nil {
	if cerr := mgr.Stop(); cerr != nil {
		slog.Error("rollback after setupJobs failure", "error", cerr)
	}
	return nil, err
}

return svc, nil
```

**在 helpers 区加 setupJobs 方法**:
```go
// setupJobs builds the jobs.Scheduler, registers it on s.mgr (so its
// lifecycle is managed alongside db/gid), and wires periodic jobs.
// Signature is receiver-only: future jobs are added inside this method as
// additional scheduler.AddFunc calls.
func (s *Service) setupJobs() error {
	scheduler, err := jobs.New(&jobs.Deps{
		Config: &cronx.Config{
			Timezone:      s.cfg.Cron.Timezone,
			OverlapPolicy: "skip",
		},
	})
	if err != nil {
		return fmt.Errorf("init jobs: %w", err)
	}
	s.mgr.Add("jobs", scheduler)

	// Register periodic jobs here when needed. Currently empty.
	return nil
}
```

- [ ] **Step 7.3: 改 `config.yaml` 加 cron 配置**

```yaml
# 在 log: 之后,或合适位置加:
cron:
  timezone: "Asia/Shanghai"
```

- [ ] **Step 7.4: 验证编译**

```bash
go build ./...
```

Expected: 无错误。

- [ ] **Step 7.5: 跑非 testcontainer 测试**

```bash
go test -count=1 ./internal/message/... ./pkg/xcodes/... ./pkg/handler/...
```

Expected: 全过。

- [ ] **Step 7.6: Commit**

```bash
git status
git add pkg/config/config.go internal/service/service.go config.yaml
git commit -m "feat(service): wire jobs.Scheduler via setupJobs + Cron config"
```

---

## Task 8: proto HTTP annotation + 启用 gateway

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Modify: `pkg/server.go`(启用 gateway)
- Regenerate: `gen/message/v1/message.pb.gw.go`

- [ ] **Step 8.1: 修改 proto**

读 `api/proto/message/v1/message.proto`,在 import 部分加 `google/api/annotations.proto`,每个 RPC 加 `option (google.api.http)`。

具体修改:

1. **加 import**:
```proto
import "buf/validate/validate.proto";
import "google/api/annotations.proto";
```

2. **每个 RPC 加 http annotation**:
```proto
service MessageService {
  rpc SendEmail(SendEmailRequest) returns (SendResponse) {
    option (google.api.http) = {
      post: "/v1/messages:email"
      body: "*"
    };
  }
  rpc SendSMS(SendSMSRequest) returns (SendResponse) {
    option (google.api.http) = {
      post: "/v1/messages:sms"
      body: "*"
    };
  }
  rpc GetMessage(GetMessageRequest) returns (MessageRecord) {
    option (google.api.http) = {
      get: "/v1/messages/{id}"
    };
  }
  rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse) {
    option (google.api.http) = {
      get: "/v1/messages"
    };
  }
  rpc GetMessageStats(GetMessageStatsRequest) returns (MessageStatsResponse) {
    option (google.api.http) = {
      get: "/v1/messages:stats"
    };
  }
}
```

不要改任何字段编号或类型。

- [ ] **Step 8.2: buf lint + generate**

```bash
buf lint
buf generate
```

Expected: 无 lint 错误;生成 `gen/message/v1/message.pb.gw.go`。

- [ ] **Step 8.3: 启用 gateway in `pkg/server.go`**

把 `nil` 改为 `pb.RegisterMessageServiceHandlerFromEndpoint`:

```go
grpcSrv := grpcx.New(
    grpcx.ServerConfig{
        GRPCAddr:    cfg.Server.GRPCAddr,
        GatewayAddr: cfg.Server.HTTPAddr,
    },
    func(s *grpc.Server) { pb.RegisterMessageServiceServer(s, hdl) },
    pb.RegisterMessageServiceHandlerFromEndpoint,  // was nil
    grpcx.ErrorInterceptor,
    protovalidate_middleware.UnaryServerInterceptor(validator),
)
```

删除 server.go 顶部"NOTE: HTTP gateway is currently disabled"注释。

- [ ] **Step 8.4: 验证 build**

```bash
go build ./...
```

Expected: 无错误。

- [ ] **Step 8.5: Commit**

```bash
git status
git add api/proto/message/v1/message.proto pkg/server.go gen/
git commit -m "feat(proto): enable HTTP gateway via google.api.http annotations"
```

---

## Task 9: DevOps 文件 — Makefile migrate + Dockerfile + .golangci.yml

**Files:**
- Modify: `Makefile`
- Create: `Dockerfile`
- Create: `.golangci.yml`

- [ ] **Step 9.1: 改 Makefile 加 migrate target**

读当前 Makefile,在合适位置(比如 generate 和 proto 之间)加:

```make
## migrate: Run database migrations (AutoMigrate)
migrate:
	go run ./cmd/migrate/
```

- [ ] **Step 9.2: 创建 Dockerfile**

```dockerfile
# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

# Runtime stage: distroless static (no shell, no libc)
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/server /server
USER nonroot:nonroot
ENTRYPOINT ["/server"]
```

- [ ] **Step 9.3: 创建 `.golangci.yml`**

拷贝 ai-kit-studio 模板,MODULE_NAME 替换为 message-service:

```bash
sed 's/MODULE_NAME/message-service/g' /Users/moss/code/base/ai-kit-studio/skills/golang-development/.golangci.yml > .golangci.yml
```

验证:
```bash
head -25 .golangci.yml
```
Expected: `local-prefixes` 下是 `- message-service`。

- [ ] **Step 9.4: 验证 lint**

```bash
golangci-lint run ./... 2>&1 | tail -10
```

Expected: 0 issues。

- [ ] **Step 9.5: Commit**

```bash
git add Makefile Dockerfile .golangci.yml
git commit -m "build: add Makefile migrate, Dockerfile (distroless), .golangci.yml"
```

---

## Task 10: 测试迁移 — service_test.go → message/message_test.go

**Files:**
- Delete: `internal/service/service_test.go`
- Create: `internal/service/message/message_test.go`

- [ ] **Step 10.1: 创建 `internal/service/message/message_test.go`**

完整内容(基于现有 service_test.go,改造为子包测试):

```go
package message

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"message-service/internal/provider/email"
	"message-service/internal/provider/sms"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/config"
	"message-service/pkg/thirdcall"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	emailcommon "github.com/servekit/go-common/message/email"
	smscommon "github.com/servekit/go-common/message/sms"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks ---

type mockEmailProvider struct {
	name string
	err  error
}

func (m *mockEmailProvider) Name() string { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, _ *emailcommon.Message) error {
	return m.err
}

type mockSMSProvider struct {
	name string
	err  error
}

func (m *mockSMSProvider) Name() string                                       { return m.name }
func (m *mockSMSProvider) Send(_ context.Context, _ *smscommon.Message) error { return m.err }

// --- helpers ---

var testGIDOnce sync.Once
var testGID thirdcall.GIDService

func getTestGID(t *testing.T) thirdcall.GIDService {
	t.Helper()
	testGIDOnce.Do(func() {
		var err error
		testGID, err = thirdcall.NewGIDService(&config.GIDConfig{
			Mode: "module",
			Snowflake: config.SnowflakeConfig{
				MachineID: 1,
				StartTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			},
		})
		require.NoError(t, err)
	})
	return testGID
}

func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.MessageRecord{}), "auto-migrate should succeed")
	return db
}

func newTestEmailService(t *testing.T, providers []emailcommon.Provider) *Service {
	t.Helper()
	db := setupDB(t)
	accounts := make(map[string]*email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = &email.AccountProvider{
			Vendor: p.Name(), Account: fmt.Sprintf("p%d", i), Provider: p,
		}
	}
	return New(
		db,
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[string]map[string]*email.AccountProvider{"mock": accounts}),
		nil, nil,
	)
}

func newTestSMSServiceWithRouter(t *testing.T, providers []smscommon.Provider) *Service {
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
	return New(db, getTestGID(t), nil, nil, router)
}

func newTestRecord(t *testing.T, channel, status int32, target string, emailVendor, smsVendor int32) *models.MessageRecord {
	t.Helper()
	id, err := getTestGID(t).NextID(context.Background())
	require.NoError(t, err)
	return &models.MessageRecord{
		ID:          id,
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
```

然后**从原 `service_test.go` 把以下测试函数逐个搬过来**,改造点:
- 测试内 `svc, repo := newTestXxx(...)` 改为 `svc := newTestXxx(...)`(去 repo)
- `repo.FindByID(ctx, x)` → `dal.GetMessageRecord(ctx, svc.db, x)`
- `repo.List(ctx, dal.ListFilter{...})` → `dal.ListMessageRecords(ctx, svc.db, dal.ListFilter{...})`
- 等等(参考上次 skills-alignment 重构的迁移规则)

测试函数列表:
- `TestSendEmail_Success`
- `TestSendEmail_AllProvidersFail`
- `TestSendEmail_FallbackProvider`
- `TestSendSMS_Success`
- `TestSendSMS_Failed`
- `TestSendSMS_RouteByPhone`
- `TestSendSMS_NoRoutesConfigured`(构造 `&MessageService{...}` 改为 `&Service{ db: db, ... }`)
- `TestSendEmail_SelectAccount`
- `TestSendEmail_SelectAccount_UnknownVendor`
- `TestSendEmail_SelectAccount_PartialSpec`
- `TestQuery_Get_Success`
- `TestQuery_Get_NotFound`
- `TestQuery_List`
- `TestQuery_Stats`

- [ ] **Step 10.2: 删除 `internal/service/service_test.go`**

```bash
git rm internal/service/service_test.go
```

- [ ] **Step 10.3: 验证编译**

```bash
go vet ./...
```

Expected: 无错误。

- [ ] **Step 10.4: 跑非 testcontainer 测试**

```bash
go test -count=1 ./internal/provider/... ./pkg/xcodes/... ./pkg/handler/...
```

Expected: 全过。

- [ ] **Step 10.5: Commit**

```bash
git status
git add internal/service/message/message_test.go
git rm internal/service/service_test.go
git commit -m "test(service): migrate service_test.go to message subpackage"
```

---

## Task 11: memory 更新

**Files:**
- Modify: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/service-dal-package-level-functions.md`
- Create: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/handler-service-layering.md`
- Create: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/provider-vs-service-message.md`
- Modify: `/Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/MEMORY.md`

- [ ] **Step 11.1: 更新 `service-dal-package-level-functions.md`**

在文件末尾加分层说明(具体内容见 spec §3.9)。

- [ ] **Step 11.2: 创建 `handler-service-layering.md`**

记录 handler/service/<domain> 三层分层规则(具体内容见 spec §3.9)。

- [ ] **Step 11.3: 创建 `provider-vs-service-message.md`**

```markdown
---
name: provider-vs-service-message
description: "Distinguish internal/provider/ (vendor protocol layer) from internal/service/message/ (business logic)"
metadata:
  node_type: memory
  type: feedback
---

`internal/provider/` 和 `internal/service/message/` 是两个不同的层,不要混淆:

- **`internal/provider/{email,sms}/`** = vendor 协议封装层。直接调 SMTP/Aliyun SDK,提供 `AccountRegistry` / `Sender` / `SendResult` 抽象。原 `internal/message/`(2026-06-21 改名)。
- **`internal/service/message/`** = 业务逻辑层。调用 provider 完成发送、持久化发送记录、查询统计。

**Why:** 之前 message-service 把 vendor 协议封装放在 `internal/message/`,但 golang-service-development skill §1 的标准目录是 `internal/provider/`(辅助业务:mqtt/kafka/jobs 等)。改名后职责边界更清晰。

**How to apply:**
- 加新 vendor(如腾讯云短信)→ 加 `internal/provider/sms/tencent/`(实现 `sms.Provider` 接口),在 `sms/registry.go` 的 `buildProvider` 加 case
- 加新业务方法(如批量发送、定时发送)→ 加到 `internal/service/message/` 子包
- 不要在 provider 层加业务逻辑(不知道发送记录、不知道 stats)
- 不要在 service/message 层直接调 vendor SDK(必须通过 provider)

Related: [[handler-service-layering]], [[service-dal-package-level-functions]].
```

- [ ] **Step 11.4: 更新 `MEMORY.md` 索引**

```markdown
- [Service-dal: package-level functions](service-dal-package-level-functions.md) — service holds *gorm.DB; dal is package-level functions; handler/service/domain layering
- [Handler/service layering](handler-service-layering.md) — handler thin stub, service facade, domain subpackage business logic
- [Provider vs service/message](provider-vs-service-message.md) — internal/provider/ is vendor protocol layer; internal/service/message/ is business logic
- [Tests prefer real DB over mocks](tests-prefer-real-db-over-mocks.md) — dbx.SetupTestDB testcontainer
- [Avoid empty abstraction base classes](avoid-empty-abstraction-base-classes.md) — no BaseRepo{ db *gorm.DB }
```

- [ ] **Step 11.5: 验证**

```bash
ls /Users/moss/.claude/projects/-Users-moss-code-base-message-service/memory/
```

Expected: 5 个 .md 文件 + MEMORY.md。

(memory 不在 git 中,无需 commit。)

---

## Task 12: 全量验证

**Files:** 无修改,只跑验证。

- [ ] **Step 12.1: 全量验证**

```bash
gofmt -l . 2>&1 | grep -v "^gen/" | head -5
goimports -l . 2>&1 | grep -v "^gen/" | head -5
golangci-lint run ./... 2>&1 | tail -5
go build ./... 2>&1 | tail -3
go vet ./... 2>&1 | tail -3
buf lint 2>&1 | tail -3
```

Expected: 全部为空/无错误。

- [ ] **Step 12.2: 跑非 testcontainer 测试**

```bash
go test -count=1 ./internal/provider/... ./pkg/xcodes/... ./pkg/handler/...
```

Expected: 全过。

- [ ] **Step 12.3: 跑 testcontainer 测试(若有 Docker)**

```bash
go test -race -count=1 ./internal/service/... ./internal/store/dal/...
```

无 Docker 则跳过。

- [ ] **Step 12.4: grep 检查清理**

```bash
grep -rn "MessageService\b" --include="*.go" . | grep -v "_test.go\|/gen/\|Unimplemented\|MessageServiceServer\|MessageServiceClient"
```

Expected: 为空。

```bash
grep -rn "models.Database\|models.NewDatabase" --include="*.go" .
grep -rn "message-service/internal/message" --include="*.go" .
```

Expected: 都为空。

- [ ] **Step 12.5: 验收检查清单**

参照 golang-service-development skill §9:
- [ ] `go build ./...` 通过
- [ ] `golangci-lint run ./...` 无 error
- [ ] `make proto && git diff --exit-code`
- [ ] `make generate && git diff --exit-code`
- [ ] 每个 RPC 在 `service.go` 都有对应 facade 方法
- [ ] 每个领域在 `internal/service/<domain>/` 子包
- [ ] `internal/provider/` 含 vendor 协议封装
- [ ] `internal/jobs/` 含 Scheduler(空架子 OK)
- [ ] service.New 末尾调 `setupJobs()`
- [ ] grpcurl/curl 能跑通(若有 DB)

---

## Self-Review Notes

### Spec 覆盖率

- §3.1 目录结构 → Task 1 (provider 改名) + Task 2 (jobs) + Task 3 (service 拆分) + Task 4 (handler) + Task 8 (gateway)
- §3.2 pkg/handler → Task 4 ✓
- §3.3 service 分层 → Task 3 ✓
- §3.4 资源管理 → Task 6 ✓
- §3.4a jobs.Scheduler → Task 2 (创建包) + Task 7 (接入 service.New) ✓
- §3.5 server.go/module.go → Task 5 + Task 8 ✓
- §3.6 HTTP gateway → Task 8 ✓
- §3.7 DevOps → Task 9 ✓
- §3.8 测试 → Task 10 ✓
- §3.9 memory → Task 11 ✓
- §6 实施顺序 → Task 1-12 对应 ✓

### 类型一致性

- `*Service` 在 service.go (root) 和 message/ 子包都有定义,路径不同不冲突
- `*Handler` 在 pkg/handler/ 定义,在 pkg/module.go 通过 type alias 暴露
- `internal/provider/email.AccountRegistry` / `sms.AccountRegistry` / `sms.Router` 类型不变,只改 import 路径

### Placeholder 扫描

- Task 10 没列全部测试函数代码(参考现有 `service_test.go`),实施时 subagent 完整迁移。这个表述明确,subagent 可执行。
- 其他 task 都有完整代码块。

---

## 风险与回滚

- 每个 task 独立 commit,失败可 `git reset --hard HEAD~1`
- Task 1 改名后,所有 `internal/message` 引用要全局更新;sed 替换后跑 grep 验证无残留
- Task 3 后 service_test.go 暂时编译失败(Task 6/10 修复),期间非测试代码应可编译
- Task 6 删 `models.Database` 是 breaking,需检查所有引用
- Task 7 接入 jobs 后,service.New 失败路径必须 mgr.Stop() 回滚
- Task 8 proto annotation 是 wire-safe,不会破坏 gRPC 客户端
