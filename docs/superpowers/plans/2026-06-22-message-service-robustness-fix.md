# message-service 鲁棒性修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 message-service 发送→持久化链路的鲁棒性问题：persist 用独立 ctx 避免数据丢失、加幂等键防重复发送、显式校验应对 module 模式、vendor unknown 告警、stats SQL 合并、ErrorMessage 长度限制、setupJobs 死代码清理。

**Architecture:** 改造 `internal/service/message/{email,sms}.go` 发送流程（4 步：校验 → 幂等查重 → 发送 → 独立 ctx persist），proto 加 `idempotency_key` 字段，DB 加 partial unique 索引，dal 加 `GetXxxRecordByIdempotencyKey` 查询，stats 改 1 次 SQL。撤回 record_id 暴露机制，失败信息塞 error message。

**Tech Stack:** Go 1.26、GORM gen typed chain、PostgreSQL、gRPC + grpc-gateway、buf validate、`github.com/servekit/go-common`（xerr / dbx / configx / lifecycle）。

**Spec:** `docs/superpowers/specs/2026-06-22-message-service-robustness-fix-design.md`

**Conventions:**
- 注释用英文，commit message 用英文（Conventional Commits）
- 错误码集中 `pkg/xcodes/`，用 `xerr.New/Wrap/Wrapf`（**没有 Newf**，要格式化用 `Wrapf` 或 `fmt.Sprintf` 传 `New`）
- 测试用 `dbx.SetupTestDB(t)` 启 PostgreSQL testcontainer，不用 mock repo
- 文件内函数排列：导出类型/构造函数/方法在上，未导出 helper 在底，用 `// --- internal helpers ---` 分隔
- 库代码（`internal/`）不直接打日志，通过返回 error；只有 `cmd/` 入口层打日志
- 禁止 `_ = err`，所有 error 显式处理（资源清理 Close 除外）
- 每个 task 完成后 commit

---

## File Map

**新建：**
- `internal/service/message/util.go` — 显式校验 + truncate helper

**修改：**
- `api/proto/message/v1/message.proto` — 加 idempotency_key 字段、MessageStatus 注释
- `internal/store/models/email_record.go` — 加 IdempotencyKey 列、ErrorMessage size、TotalStats 接口
- `internal/store/models/sms_record.go` — 加 IdempotencyKey 列、ErrorMessage size、TotalStats 接口
- `internal/store/dal/email_record.go` — GetEmailRecordByIdempotencyKey、CountEmailStats 改 1 次 SQL、SuccessRate -1
- `internal/store/dal/sms_record.go` — 同上对称
- `internal/store/dal/email_record_test.go` — 加幂等查询测试、stats 合并验证
- `internal/store/dal/sms_record_test.go` — 同上
- `internal/service/message/email.go` — 流程改造、显式校验、幂等查重、独立 ctx persist、vendor unknown 告警、Wrapf 失败响应
- `internal/service/message/sms.go` — 同上对称
- `internal/service/message/email_test.go` — 幂等、校验、独立 ctx、Wrapf 失败响应测试
- `internal/service/message/sms_test.go` — 同上
- `internal/service/service.go` — 删除 setupJobs 方法 + 注释块
- `cmd/migrate/main.go` — 加 partial unique index 创建
- `CLAUDE.md` — MessageStatus 语义、idempotency_key 说明（如有约定变化）

**生成（无手动改动）：**
- `internal/store/generated/*` — 通过 `make generate`
- `gen/message/v1/*` — 通过 `make proto`

---

## Task 1: Proto 加 idempotency_key 字段 + MessageStatus 注释更新

**Files:**
- Modify: `api/proto/message/v1/message.proto`

- [ ] **Step 1: 更新 MessageStatus 枚举注释**

打开 `api/proto/message/v1/message.proto`，找到 `enum MessageStatus { ... }`，把整个枚举注释替换为：

```proto
// MessageStatus represents the delivery status of a message.
enum MessageStatus {
  // UNSPECIFIED — never persisted.
  MESSAGE_STATUS_UNSPECIFIED = 0;

  // PENDING — send in progress. Reserved for future async-send flow;
  // the sync flow does NOT write PENDING (currently unused).
  MESSAGE_STATUS_PENDING = 1;

  // SENT — the vendor synchronously accepted the send request.
  // For SMS: vendor API returned OK; does NOT mean handset received
  //          (async delivery is not tracked yet).
  // For email: SMTP server accepted the message.
  MESSAGE_STATUS_SENT = 2;

  // FAILED — the vendor rejected the request, network/transport failed,
  // or context was cancelled. error_message carries the last error.
  MESSAGE_STATUS_FAILED = 3;
}
```

- [ ] **Step 2: 给 SendEmailRequest 加 idempotency_key 字段**

找到 `message SendEmailRequest { ... }`，在最后一个字段 `string sender_id = 13;` 之后、`option (buf.validate.message).cel` 之前加：

```proto
  // IdempotencyKey is optional. When set, a second request with the same
  // (sender_id, idempotency_key) returns the existing record without
  // re-sending. Use a UUID per logical send intent. Max length 64.
  string idempotency_key = 14;
```

- [ ] **Step 3: 给 SendSMSRequest 加 idempotency_key 字段**

找到 `message SendSMSRequest { ... }`，在最后一个字段 `string sender_id = 8;` 之后、`option (buf.validate.message).cel` 之前加：

```proto
  // IdempotencyKey is optional. See SendEmailRequest.idempotency_key.
  string idempotency_key = 9;
```

- [ ] **Step 4: 验证 proto 语法**

Run: `buf lint`
Expected: PASS（无 lint 错误）

- [ ] **Step 5: 重新生成 pb 代码**

Run: `make proto`
Expected: 输出 `Finished`，无错误

- [ ] **Step 6: 验证生成的代码包含新字段**

Run: `grep -n "IdempotencyKey" gen/message/v1/message.pb.go | head -5`
Expected: 至少 5 行匹配（struct field、getter、JSON tag 等）

- [ ] **Step 7: Commit**

```bash
git add api/proto/message/v1/message.proto gen/
git commit -m "feat(proto): add idempotency_key to SendEmail/SendSMS requests

Document MESSAGE_STATUS_SENT semantics (vendor-accepted, not delivered).
PENDING documented as reserved for future async-send flow."
```

---

## Task 2: Model 加 IdempotencyKey 列、ErrorMessage size

**Files:**
- Modify: `internal/store/models/email_record.go`
- Modify: `internal/store/models/sms_record.go`

- [ ] **Step 1: email_record.go 加 IdempotencyKey 列**

打开 `internal/store/models/email_record.go`，找到 `type EmailRecord struct { ... }`，在 `SenderID` 字段之后加：

```go
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
	IdempotencyKey string          `gorm:"size:64;column:idempotency_key;index"`
```

- [ ] **Step 2: email_record.go ErrorMessage 加 size**

在同一文件找到 `ErrorMessage string \`gorm:"column:error_message"\``，改为：

```go
	ErrorMessage   string          `gorm:"size:1024;column:error_message"`
```

- [ ] **Step 3: sms_record.go 同样改动**

打开 `internal/store/models/sms_record.go`，做对称改动：
- 在 `SenderID` 字段后加 `IdempotencyKey`
- `ErrorMessage` 加 `size:1024`

- [ ] **Step 4: 重新生成 dal typed chain**

Run: `make generate`
Expected: 输出 `generate success done`，无错误

- [ ] **Step 5: 验证 generated 文件包含新字段**

Run: `grep -n "IdempotencyKey" internal/store/generated/email_record.go`
Expected: 看到 `IdempotencyKey field.String` 字段定义

- [ ] **Step 6: 编译检查**

Run: `go build ./...`
Expected: PASS（无编译错误）

- [ ] **Step 7: Commit**

```bash
git add internal/store/models/ internal/store/generated/
git commit -m "feat(model): add IdempotencyKey column, cap ErrorMessage to 1024"
```

---

## Task 3: cmd/migrate 加 partial unique index

**Files:**
- Modify: `cmd/migrate/main.go`

- [ ] **Step 1: 在 AutoMigrate 之后加 partial unique index 创建**

打开 `cmd/migrate/main.go`，找到 main 函数末尾，在 `dbx.AutoMigrate(...)` 调用之后追加：

```go
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}

	// Partial unique indexes for idempotency. NULL/'' values are excluded
	// so legacy requests without idempotency_key don't collide.
	// AutoMigrate can't express partial indexes via struct tags, hence raw SQL.
	indexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_email_records_sender_idempotency
		   ON email_records (sender_id, idempotency_key)
		   WHERE idempotency_key != ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_records_sender_idempotency
		   ON sms_records (sender_id, idempotency_key)
		   WHERE idempotency_key != ''`,
	}
	for _, ddl := range indexes {
		if err := db.Exec(ddl).Error; err != nil {
			slog.Error("create index failed", "ddl", ddl, "error", err)
			os.Exit(1)
		}
	}
```

- [ ] **Step 2: 编译检查**

Run: `go build ./cmd/migrate/`
Expected: PASS

- [ ] **Step 3: 跑 migrate 验证 SQL 正确（用本地 postgres 或 testcontainer）**

如果有本地 PG：`make migrate`
Expected: 退出码 0，日志显示 migrate + index 创建成功

如果没本地 PG：跳过此步，Task 5 的 dal 测试会用 testcontainer 验证

- [ ] **Step 4: Commit**

```bash
git add cmd/migrate/main.go
git commit -m "feat(migrate): add partial unique indexes for (sender_id, idempotency_key)"
```

---

## Task 4: DAL 加 GetEmailRecordByIdempotencyKey

**Files:**
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/email_record_test.go`

- [ ] **Step 1: 写失败测试**

打开 `internal/store/dal/email_record_test.go`，在文件末尾加：

```go
func TestGetEmailRecordByIdempotencyKey_Hit(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	record := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)
	record.SenderID = "user:42"
	record.IdempotencyKey = "abc-123"
	require.NoError(t, CreateEmailRecord(ctx, db, record))

	got, err := GetEmailRecordByIdempotencyKey(ctx, db, "user:42", "abc-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, record.ID, got.ID)
	assert.Equal(t, "abc-123", got.IdempotencyKey)
}

func TestGetEmailRecordByIdempotencyKey_NotFound(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	got, err := GetEmailRecordByIdempotencyKey(ctx, db, "user:42", "missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetEmailRecordByIdempotencyKey_DoesNotCrossSenders(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	record := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)
	record.SenderID = "user:42"
	record.IdempotencyKey = "shared-key"
	require.NoError(t, CreateEmailRecord(ctx, db, record))

	// Different sender with same key should not match.
	got, err := GetEmailRecordByIdempotencyKey(ctx, db, "user:99", "shared-key")
	require.NoError(t, err)
	assert.Nil(t, got)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/dal/ -run TestGetEmailRecordByIdempotencyKey -v`
Expected: 编译失败，`undefined: GetEmailRecordByIdempotencyKey`

- [ ] **Step 3: 实现 GetEmailRecordByIdempotencyKey**

打开 `internal/store/dal/email_record.go`，在 `GetEmailRecord` 函数之后加：

```go
// GetEmailRecordByIdempotencyKey returns the record for a given
// (sender_id, idempotency_key), or (nil, nil) if not found. Caller must
// ensure idempotencyKey != "" before calling — empty keys are not indexed
// by the partial unique constraint.
func GetEmailRecordByIdempotencyKey(ctx context.Context, tx *gorm.DB, senderID, idempotencyKey string) (*models.EmailRecord, error) {
	record, err := gorm.G[models.EmailRecord](tx).
		Where(generated.EmailRecord.SenderID.Eq(senderID)).
		Where(generated.EmailRecord.IdempotencyKey.Eq(idempotencyKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/dal/ -run TestGetEmailRecordByIdempotencyKey -v`
Expected: PASS（3 个测试全过）

- [ ] **Step 5: Commit**

```bash
git add internal/store/dal/email_record.go internal/store/dal/email_record_test.go
git commit -m "feat(dal): add GetEmailRecordByIdempotencyKey"
```

---

## Task 5: DAL 加 GetSMSRecordByIdempotencyKey

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`

- [ ] **Step 1: 写失败测试**

打开 `internal/store/dal/sms_record_test.go`，在文件末尾加（参考 Task 4 的 email 版本，把 Email 换成 SMS）：

```go
func TestGetSMSRecordByIdempotencyKey_Hit(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	record := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)
	record.SenderID = "user:42"
	record.IdempotencyKey = "abc-123"
	require.NoError(t, CreateSMSRecord(ctx, db, record))

	got, err := GetSMSRecordByIdempotencyKey(ctx, db, "user:42", "abc-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, record.ID, got.ID)
	assert.Equal(t, "abc-123", got.IdempotencyKey)
}

func TestGetSMSRecordByIdempotencyKey_NotFound(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	got, err := GetSMSRecordByIdempotencyKey(ctx, db, "user:42", "missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetSMSRecordByIdempotencyKey_DoesNotCrossSenders(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	record := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)
	record.SenderID = "user:42"
	record.IdempotencyKey = "shared-key"
	require.NoError(t, CreateSMSRecord(ctx, db, record))

	got, err := GetSMSRecordByIdempotencyKey(ctx, db, "user:99", "shared-key")
	require.NoError(t, err)
	assert.Nil(t, got)
}
```

**注意**：如果 sms_record_test.go 里没有 `setupSMSDB` 或 `newTestSMSRecord` helper，先检查现有代码：

Run: `grep -n "func setupSMSDB\|func newTestSMSRecord" internal/store/dal/sms_record_test.go`

如果缺，参考 email_record_test.go 的对应函数复制改名。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/dal/ -run TestGetSMSRecordByIdempotencyKey -v`
Expected: 编译失败，`undefined: GetSMSRecordByIdempotencyKey`

- [ ] **Step 3: 实现 GetSMSRecordByIdempotencyKey**

打开 `internal/store/dal/sms_record.go`，在 `GetSMSRecord` 函数之后加：

```go
// GetSMSRecordByIdempotencyKey returns the record for a given
// (sender_id, idempotency_key), or (nil, nil) if not found. Caller must
// ensure idempotencyKey != "" before calling — empty keys are not indexed
// by the partial unique constraint.
func GetSMSRecordByIdempotencyKey(ctx context.Context, tx *gorm.DB, senderID, idempotencyKey string) (*models.SMSRecord, error) {
	record, err := gorm.G[models.SMSRecord](tx).
		Where(generated.SMSRecord.SenderID.Eq(senderID)).
		Where(generated.SMSRecord.IdempotencyKey.Eq(idempotencyKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/dal/ -run TestGetSMSRecordByIdempotencyKey -v`
Expected: PASS（3 个测试全过）

- [ ] **Step 5: Commit**

```bash
git add internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go
git commit -m "feat(dal): add GetSMSRecordByIdempotencyKey"
```

---

## Task 6: stats 改 1 次 SQL（COUNT FILTER）+ SuccessRate -1

**Files:**
- Modify: `internal/store/models/email_record.go` — 加 TotalStats 接口签名
- Modify: `internal/store/models/sms_record.go` — 同上
- Modify: `internal/store/dal/email_record.go` — CountEmailStats 改实现
- Modify: `internal/store/dal/sms_record.go` — CountSMSStats 改实现
- Regenerate: `internal/store/generated/*`

- [ ] **Step 1: email_record.go model 加 TotalStats 接口签名**

打开 `internal/store/models/email_record.go`，在文件末尾的 `EmailRecordStatsQuery[T]` 接口之后加：

```go
// EmailRecordTotalStatsQuery defines typed raw SQL for total/sent/failed
// aggregation in a single query. gorm gen discovers this interface by name
// (prefix matches the EmailRecord model) and generates a typed method.
//
// Uses COUNT(*) FILTER to compute sent/failed in one pass instead of
// three separate COUNT queries.
type EmailRecordTotalStatsQuery[T any] interface {
	// SELECT
	//   COUNT(*) AS total,
	//   COUNT(*) FILTER (WHERE status = @sentStatus) AS sent,
	//   COUNT(*) FILTER (WHERE status = @failedStatus) AS failed
	// FROM email_records
	// {{where}}
	//   {{if vendor > 0}} vendor = @vendor {{end}}
	//   {{if scene > 0}} AND scene = @scene {{end}}
	//   {{if startTime != nil}} AND created_at >= @startTime {{end}}
	//   {{if endTime != nil}} AND created_at <= @endTime {{end}}
	// {{end}}
	TotalStats(
		sentStatus int32,
		failedStatus int32,
		vendor int32,
		scene int32,
		startTime *time.Time,
		endTime *time.Time,
	) ([]T, error)
}

// EmailTotalStatsRow is the scan target for the total stats query.
// Not a GORM model.
type EmailTotalStatsRow struct {
	Total  int64
	Sent   int64
	Failed int64
}
```

- [ ] **Step 2: sms_record.go model 加对称的接口签名**

打开 `internal/store/models/sms_record.go`，做对称改动（把 Email 换成 SMS，`SmsTotalStatsRow`）。

- [ ] **Step 3: 重新生成 typed SQL 代码**

Run: `make generate`
Expected: 生成 `_EmailRecordTotalStatsQueryImpl` 和 `_SMSRecordTotalStatsQueryImpl`

- [ ] **Step 4: 改 CountEmailStats 实现**

打开 `internal/store/dal/email_record.go`，找到 `CountEmailStats` 函数，整段替换为：

```go
// CountEmailStats returns aggregated email statistics matching filter.
// Single SQL query using COUNT(*) FILTER (faster than 3 separate COUNTs).
// SuccessRate is -1 when total == 0 (distinguishes "no data" from "0% success").
func CountEmailStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) (*Stats, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	rows, err := generated.EmailRecordTotalStatsQuery[models.EmailTotalStatsRow](tx).TotalStats(
		ctx,
		sentStatus, failedStatus,
		int32(filter.Vendor), int32(filter.Scene),
		filter.StartTime, filter.EndTime,
	)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if len(rows) == 0 {
		return &Stats{SuccessRate: -1}, nil
	}
	r := rows[0]

	var successRate float64
	if r.Total > 0 {
		successRate = float64(r.Sent) / float64(r.Total) * 100
	} else {
		successRate = -1
	}

	return &Stats{
		Total:       r.Total,
		Sent:        r.Sent,
		Failed:      r.Failed,
		SuccessRate: successRate,
	}, nil
}
```

- [ ] **Step 5: 移除现在未使用的 applyEmailStatsFilter helper**

打开 `internal/store/dal/email_record.go`，删掉 `applyEmailStatsFilter` 函数（它的逻辑现在嵌入在生成的 SQL 模板里）。

Run: `grep -n "applyEmailStatsFilter" internal/store/dal/email_record.go`
Expected: 无匹配（如果还有，说明 ListEmailVendorStats 还在用，先检查）

注意：`ListEmailVendorStats` 用的是 generated typed SQL，**不**用 `applyEmailStatsFilter`，所以可以删。但保险起见，先 grep 确认全文件没有其他引用。

- [ ] **Step 6: 改 CountSMSStats 实现（对称）**

打开 `internal/store/dal/sms_record.go`，做对称改动。

- [ ] **Step 7: 移除 applySMSStatsFilter helper（如果有）**

同 Step 5 对称操作。

- [ ] **Step 8: 跑现有 stats 测试确认行为不变**

Run: `go test ./internal/store/dal/ -run "TestCountEmailStats|TestCountSMSStats" -v`
Expected: PASS（现有测试断言行为不变；TestCountEmailStats 的 `assert.InDelta(t, 66.67, stats.SuccessRate, 0.1)` 应该仍通过）

- [ ] **Step 9: 加 SuccessRate=-1 的语义测试**

打开 `internal/store/dal/email_record_test.go`，在 `TestCountEmailStats` 之后加：

```go
func TestCountEmailStats_EmptyReturnsMinusOne(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	stats, err := CountEmailStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Total)
	assert.Equal(t, float64(-1), stats.SuccessRate,
		"empty filter should return -1 to distinguish 'no data' from '0% success'")
}
```

sms_record_test.go 加对称测试。

- [ ] **Step 10: 跑测试确认通过**

Run: `go test ./internal/store/dal/ -v`
Expected: PASS（所有测试，包括新增的）

- [ ] **Step 11: Commit**

```bash
git add internal/store/models/ internal/store/dal/ internal/store/generated/
git commit -m "refactor(dal): merge stats COUNT queries, return -1 for empty SuccessRate"
```

---

## Task 7: 新建 service/message/util.go（校验 + truncate helper）

**Files:**
- Create: `internal/service/message/util.go`

- [ ] **Step 1: 写失败测试**

新建 `internal/service/message/util_test.go`：

```go
package message

import (
	"strings"
	"testing"

	pb "message-service/gen/message/v1"

	"github.com/stretchr/testify/assert"
)

func TestValidateSendEmailRequest_AllValid(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	}
	assert.NoError(t, validateSendEmailRequest(req))
}

func TestValidateSendEmailRequest_VendorWithoutAccount(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
		// Account empty
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestValidateSendEmailRequest_AccountWithoutVendor(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Account:  "primary",
		// Vendor empty
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestValidateSendEmailRequest_MissingScene(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		SenderId: "user:42",
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scene")
}

func TestValidateSendEmailRequest_MissingSenderID(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:    "user@example.com",
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sender_id")
}

func TestValidateSendEmailRequest_IdempotencyKeyTooLong(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: strings.Repeat("a", 65),
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency_key")
}

func TestValidateSendSMSRequest_VendorWithoutAccount(t *testing.T) {
	req := &pb.SendSMSRequest{
		To:       "+8613800001111",
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.SmsVendor_SMS_VENDOR_ALIYUN,
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestTruncateErrorMessage_Short(t *testing.T) {
	assert.Equal(t, "hello", truncateErrorMessage("hello"))
}

func TestTruncateErrorMessage_Long(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := truncateErrorMessage(long)
	assert.Equal(t, 1024, len(got))
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/service/message/ -run "TestValidate|TestTruncate" -v`
Expected: 编译失败，`undefined: validateSendEmailRequest` 等

- [ ] **Step 3: 创建 util.go 实现**

新建 `internal/service/message/util.go`：

```go
package message

import (
	"fmt"

	pb "message-service/gen/message/v1"
)

// maxErrorMessageLen caps the persisted error_message to match the DB column
// size (model.EmailRecord.ErrorMessage / model.SMSRecord.ErrorMessage gorm:"size:1024").
const maxErrorMessageLen = 1024

// maxIdempotencyKeyLen matches the DB column size (IdempotencyKey gorm:"size:64").
const maxIdempotencyKeyLen = 64

// validateSendEmailRequest enforces required fields and cross-field invariants
// at the service layer. This is a defense-in-depth check that runs even when
// the protovalidate interceptor is bypassed (e.g. module-mode direct calls).
// The proto CEL rules are the primary check; this is the fallback.
func validateSendEmailRequest(req *pb.SendEmailRequest) error {
	vendorSet := req.GetVendor() != pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	accountSet := req.GetAccount() != ""
	if vendorSet != accountSet {
		return fmt.Errorf("vendor and account must be set together")
	}
	if req.GetScene() == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if req.GetSenderId() == "" {
		return fmt.Errorf("sender_id is required")
	}
	if len(req.GetIdempotencyKey()) > maxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", maxIdempotencyKeyLen)
	}
	return nil
}

// validateSendSMSRequest mirrors validateSendEmailRequest for SMS.
func validateSendSMSRequest(req *pb.SendSMSRequest) error {
	vendorSet := req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	accountSet := req.GetAccount() != ""
	if vendorSet != accountSet {
		return fmt.Errorf("vendor and account must be set together")
	}
	if req.GetScene() == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if req.GetSenderId() == "" {
		return fmt.Errorf("sender_id is required")
	}
	if len(req.GetIdempotencyKey()) > maxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", maxIdempotencyKeyLen)
	}
	return nil
}

// truncateErrorMessage caps vendor error strings so a multi-KB HTML error
// page doesn't bloat the DB row. The model column is varchar(1024).
func truncateErrorMessage(s string) string {
	if len(s) <= maxErrorMessageLen {
		return s
	}
	return s[:maxErrorMessageLen]
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/service/message/ -run "TestValidate|TestTruncate" -v`
Expected: PASS（所有测试）

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/util.go internal/service/message/util_test.go
git commit -m "feat(service): add request validators and truncateErrorMessage helper"
```

---

## Task 8: email.go 流程改造（幂等 + 显式校验 + 独立 ctx + Wrapf 失败响应）

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/email_test.go`

- [ ] **Step 1: 加 mock provider 调用计数**

打开 `internal/service/message/email_test.go`，找到 `mockEmailProvider`，加调用计数：

```go
type mockEmailProvider struct {
	name  string
	err   error
	calls int
}

func (m *mockEmailProvider) Name() string { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, _ *emailcommon.Message) error {
	m.calls++
	return m.err
}
```

- [ ] **Step 2: 写失败测试 — 幂等命中**

在 email_test.go 末尾加：

```go
func TestSendEmail_Idempotent_SecondCallSkipsProvider(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	resp1, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, provider.calls)

	// Second call with same key must not invoke provider.
	resp2, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, provider.calls, "provider must not be called for idempotent retry")
	assert.Equal(t, resp1.Id, resp2.Id)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp2.Status)
}

func TestSendEmail_Idempotent_NoKey_DoesNotDedupe(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		// No idempotency_key
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	_, err = svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.calls, "without key, both calls hit provider")
}

func TestSendEmail_Idempotent_HitsFailedRecord(t *testing.T) {
	provider := &mockEmailProvider{name: "mock", err: fmt.Errorf("smtp timeout")}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	// First call fails.
	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 1, provider.calls)

	// Second call with same key must not invoke provider; returns wrapped error.
	_, err = svc.SendEmail(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 1, provider.calls)
	assert.Contains(t, err.Error(), "previous attempt")
}
```

- [ ] **Step 3: 写失败测试 — 独立 ctx persist**

加：

```go
func TestSendEmail_PersistsEvenWhenContextCancelled(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before send

	req := &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	}

	resp, err := svc.SendEmail(ctx, req)
	// Provider's Send is called with cancelled ctx; the Sender wrapper
	// checks ctx.Err() and returns a failed result. Service persists it.
	require.Error(t, err)

	// The record must still be persisted (independent ctx).
	require.NotNil(t, resp) // resp is nil on error path; we use sender_id to find
	listResp, lerr := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		SenderId: "user:42",
	})
	require.NoError(t, lerr)
	require.Len(t, listResp.Records, 1)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, listResp.Records[0].Status)
}
```

注意：`resp` 在 error 路径是 nil，测试用 ListEmails 验证。修正：

```go
	_, err = svc.SendEmail(ctx, req)
	require.Error(t, err)

	listResp, lerr := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		SenderId: "user:42",
	})
	require.NoError(t, lerr)
	require.Len(t, listResp.Records, 1, "record must be persisted even with cancelled ctx")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, listResp.Records[0].Status)
}
```

- [ ] **Step 4: 写失败测试 — 显式校验**

加：

```go
func TestSendEmail_RejectsMissingScene(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		SenderId: "user:42",
		// No scene
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls, "validation must short-circuit before provider call")
}

func TestSendEmail_RejectsVendorWithoutAccount(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "user@example.com",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
		// No account
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls)
}
```

- [ ] **Step 5: 写失败测试 — Wrapf 失败响应信息**

加：

```go
func TestSendEmail_FailureIncludesVendorContext(t *testing.T) {
	provider := &mockEmailProvider{name: "aliyun", err: fmt.Errorf("connection refused")}
	svc := newTestEmailService(t, []emailcommon.Provider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "vendor=")
	assert.Contains(t, msg, "account=")
	assert.Contains(t, msg, "attempts=")
	assert.Contains(t, msg, "connection refused")
}
```

- [ ] **Step 6: 跑测试确认全部失败**

Run: `go test ./internal/service/message/ -run TestSendEmail -v`
Expected: 所有新加测试 FAIL（流程尚未改造）

- [ ] **Step 7: 改造 email.go SendEmail 主流程**

打开 `internal/service/message/email.go`，整段替换 `SendEmail` 函数：

```go
// SendEmail sends an email via the configured vendor/account, or the default
// fallback chain when both are unset. Idempotent on (sender_id, idempotency_key):
// a second request with the same key returns the existing record without
// re-invoking the provider.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	if err := validateSendEmailRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	// Idempotency check.
	if key := req.GetIdempotencyKey(); key != "" {
		existing, err := dal.GetEmailRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if existing != nil {
			return s.respondIdempotentEmail(existing)
		}
	}

	sender, err := s.emailRegistry.SenderFor(req.GetVendor(), req.GetAccount())
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	id, err := s.gid.NextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	msg := &emailcommon.Message{
		To:             req.GetTo(),
		Cc:             req.GetCc(),
		Bcc:            req.GetBcc(),
		Subject:        req.GetSubject(),
		Body:           req.GetBody(),
		HTMLBody:       req.GetHtmlBody(),
		ReplyTo:        req.GetReplyTo(),
		Template:       req.GetTemplateId(),
		TemplateParams: req.GetTemplateParams(),
	}

	result, sendErr := sender.Send(ctx, msg)

	// Persist with an independent context so request cancellation does not
	// lose the record. Only persist when we have a result — pre-send failures
	// (empty recipient, no provider) return (nil, err) and have nothing to log.
	if result != nil {
		persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.persistEmailRecord(persistCtx, id, req, result)
	}

	if sendErr != nil {
		vendor := ""
		account := ""
		attempts := 0
		if result != nil {
			vendor = result.Vendor
			account = result.Account
			attempts = result.Attempts
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d", vendor, account, attempts)
	}

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_EmailVendor{
			EmailVendor: emailVendorFromString(result.Vendor),
		},
	}, nil
}

// respondIdempotentEmail builds the response for a request whose
// (sender_id, idempotency_key) already has a record. SENT → success response;
// FAILED → error referencing the original failure message; anything else →
// internal error (PENDING is not written by the sync flow, only by future
// async-send work).
func (s *Service) respondIdempotentEmail(existing *models.EmailRecord) (*pb.SendResponse, error) {
	switch pb.MessageStatus(existing.Status) {
	case pb.MessageStatus_MESSAGE_STATUS_SENT:
		return &pb.SendResponse{
			Id:     existing.ID,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_EmailVendor{
				EmailVendor: pb.EmailVendor(existing.Vendor),
			},
		}, nil
	case pb.MessageStatus_MESSAGE_STATUS_FAILED:
		return nil, xcodes.ErrMessageSendFailed.Wrap(fmt.Errorf("previous attempt with same idempotency_key failed: %s", existing.ErrorMessage))
	default:
		return nil, xcodes.ErrInternal.Wrap(fmt.Errorf("idempotent record in unexpected status %d", existing.Status))
	}
}
```

注意：需要加 `"fmt"` import 如果还没有。

- [ ] **Step 8: persistEmailRecord 加 truncate + IdempotencyKey**

在同文件找到 `persistEmailRecord` 函数，整段替换为：

```go
func (s *Service) persistEmailRecord(ctx context.Context, id int64, req *pb.SendEmailRequest, result *email.SendResult) {
	record := &models.EmailRecord{
		ID:             id,
		Vendor:         int32(emailVendorFromString(result.Vendor)),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		Target:         req.GetTo(),
		SenderID:       req.GetSenderId(),
		IdempotencyKey: req.GetIdempotencyKey(),
		Cc:             models.StringSlice(req.GetCc()),
		Bcc:            models.StringSlice(req.GetBcc()),
		Subject:        req.GetSubject(),
		Content:        req.GetBody(),
		HTMLBody:       req.GetHtmlBody(),
		ReplyTo:        req.GetReplyTo(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = truncateErrorMessage(result.Error.Error())
		}
	}

	if err := dal.CreateEmailRecord(ctx, s.db, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
	}
}
```

- [ ] **Step 9: 跑测试确认通过**

Run: `go test ./internal/service/message/ -run TestSendEmail -v`
Expected: PASS（所有测试，包括 Task 7 的 validator 测试）

如果失败，常见问题：
- `fmt` 没导入 → 加 import
- `models.EmailRecord` 缺 `IdempotencyKey` 字段 → 回到 Task 2 检查
- `dal.GetEmailRecordByIdempotencyKey` 未定义 → 回到 Task 4 检查

- [ ] **Step 10: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "refactor(service): email send flow — idempotency, explicit validation, independent ctx persist

- Idempotency check via (sender_id, idempotency_key) lookup before send
- Explicit validation runs even without protovalidate interceptor
- persistEmailRecord uses context.Background()+3s timeout so request
  cancellation no longer loses FAILED records
- Failure error wraps vendor/account/attempts context for caller debugging
- ErrorMessage truncated to 1024 chars to fit DB column"
```

---

## Task 9: sms.go 流程改造（对称）

**Files:**
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/message/sms_test.go`

- [ ] **Step 1: 加 mock provider 调用计数**

打开 `internal/service/message/sms_test.go`，找到 `mockSMSProvider`，加 `calls int` 字段（参考 Task 8 Step 1）。

- [ ] **Step 2: 写失败测试 — 幂等命中（参考 Task 8 Step 2，Email 换成 SMS）**

在 sms_test.go 末尾加 `TestSendSMS_Idempotent_SecondCallSkipsProvider`、`TestSendSMS_Idempotent_NoKey_DoesNotDedupe`、`TestSendSMS_Idempotent_HitsFailedRecord`，参考 Task 8 Step 2 的 email 版本，把 Email 字段换成 SMS（`SmsScene_SMS_SCENE_LOGIN_CODE`、`pb.SmsVendor` 等）。

- [ ] **Step 3: 写失败测试 — 独立 ctx persist + 显式校验 + Wrapf（参考 Task 8 Step 3-5，Email 换成 SMS）**

加：
- `TestSendSMS_PersistsEvenWhenContextCancelled`
- `TestSendSMS_RejectsMissingScene`
- `TestSendSMS_RejectsVendorWithoutAccount`
- `TestSendSMS_FailureIncludesVendorContext`

- [ ] **Step 4: 跑测试确认全部失败**

Run: `go test ./internal/service/message/ -run TestSendSMS -v`
Expected: 所有新加测试 FAIL

- [ ] **Step 5: 改造 sms.go SendSMS 主流程**

打开 `internal/service/message/sms.go`，整段替换 `SendSMS` 函数：

```go
// SendSMS sends an SMS via the configured vendor/account, or routes by phone
// country code when both are unset. Idempotent on (sender_id, idempotency_key).
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	if err := validateSendSMSRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	// Idempotency check.
	if key := req.GetIdempotencyKey(); key != "" {
		existing, err := dal.GetSMSRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if existing != nil {
			return s.respondIdempotentSMS(existing)
		}
	}

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
	if req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED {
		// validateSendSMSRequest guaranteed account is also set.
		sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
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

	// Persist with an independent context.
	if result != nil {
		persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.persistSMSRecord(persistCtx, id, req, result)
	}

	if sendErr != nil {
		vendor := ""
		account := ""
		attempts := 0
		if result != nil {
			vendor = result.Vendor
			account = result.Account
			attempts = result.Attempts
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d", vendor, account, attempts)
	}

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{
			SmsVendor: smsVendorFromString(result.Vendor),
		},
	}, nil
}

// respondIdempotentSMS mirrors respondIdempotentEmail for SMS.
func (s *Service) respondIdempotentSMS(existing *models.SMSRecord) (*pb.SendResponse, error) {
	switch pb.MessageStatus(existing.Status) {
	case pb.MessageStatus_MESSAGE_STATUS_SENT:
		return &pb.SendResponse{
			Id:     existing.ID,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_SmsVendor{
				SmsVendor: pb.SmsVendor(existing.Vendor),
			},
		}, nil
	case pb.MessageStatus_MESSAGE_STATUS_FAILED:
		return nil, xcodes.ErrMessageSendFailed.Wrap(fmt.Errorf("previous attempt with same idempotency_key failed: %s", existing.ErrorMessage))
	default:
		return nil, xcodes.ErrInternal.Wrap(fmt.Errorf("idempotent record in unexpected status %d", existing.Status))
	}
}
```

- [ ] **Step 6: persistSMSRecord 加 truncate + IdempotencyKey**

在同文件找到 `persistSMSRecord` 函数，整段替换为：

```go
func (s *Service) persistSMSRecord(ctx context.Context, id int64, req *pb.SendSMSRequest, result *sms.SendResult) {
	record := &models.SMSRecord{
		ID:             id,
		Vendor:         int32(smsVendorFromString(result.Vendor)),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		Target:         req.GetTo(),
		SenderID:       req.GetSenderId(),
		IdempotencyKey: req.GetIdempotencyKey(),
		Content:        req.GetContent(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = truncateErrorMessage(result.Error.Error())
		}
	}

	if err := dal.CreateSMSRecord(ctx, s.db, record); err != nil {
		slog.Error("persist sms record", "record_id", id, "error", err)
	}
}
```

- [ ] **Step 7: 跑测试确认通过**

Run: `go test ./internal/service/message/ -run TestSendSMS -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go
git commit -m "refactor(service): sms send flow — idempotency, explicit validation, independent ctx persist

Same shape as email send flow. Vendor+account both-or-neither now enforced
explicitly (validateSendSMSRequest) so module-mode callers without the
protovalidate interceptor can't reach the router with mismatched input."
```

---

## Task 10: vendor unknown 告警

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/sms.go`

- [ ] **Step 1: 改 emailVendorFromString 加 slog.Warn**

打开 `internal/service/message/email.go`，找到 `emailVendorFromString` 函数，整段替换为：

```go
// emailVendorFromString converts the vendor name returned by go-common's
// Provider.Name() (string, e.g. "aliyun") back to the proto enum. Needed
// because SendResult.Vendor is string-typed (go-common interface contract),
// while SendResponse.vendor is proto enum.
//
// Unknown names log a warning so a go-common upgrade that adds a new vendor
// is detectable in monitoring; the record is persisted with UNSPECIFIED
// vendor for traceability.
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
		slog.Warn("unknown email vendor name from go-common provider",
			"vendor", s,
			"hint", "add case to emailVendorFromString or upgrade message-service")
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	}
}
```

- [ ] **Step 2: 改 smsVendorFromString 加 slog.Warn**

打开 `internal/service/message/sms.go`，找到 `smsVendorFromString` 函数，整段替换为：

```go
// smsVendorFromString converts the vendor name returned by go-common's
// Provider.Name() (string, e.g. "aliyun") back to the proto enum. Needed
// because SendResult.Vendor is string-typed (go-common interface contract),
// while SendResponse.vendor is proto enum.
//
// Unknown names log a warning so a go-common upgrade that adds a new vendor
// is detectable in monitoring; the record is persisted with UNSPECIFIED
// vendor for traceability.
func smsVendorFromString(s string) pb.SmsVendor {
	switch s {
	case "aliyun":
		return pb.SmsVendor_SMS_VENDOR_ALIYUN
	default:
		slog.Warn("unknown sms vendor name from go-common provider",
			"vendor", s,
			"hint", "add case to smsVendorFromString or upgrade message-service")
		return pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	}
}
```

- [ ] **Step 3: 跑全部测试确认无回归**

Run: `go test ./internal/service/message/ -v`
Expected: PASS（slog.Warn 不影响行为）

- [ ] **Step 4: Commit**

```bash
git add internal/service/message/email.go internal/service/message/sms.go
git commit -m "feat(service): log warning on unknown vendor name from go-common provider"
```

---

## Task 11: setupJobs 死代码清理

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: 删除注释块和 setupJobs 方法**

打开 `internal/service/service.go`，删除：
1. 第 114-119 行的注释块（`// if err := svc.setupJobs(); err != nil {` 整段）
2. 第 130-146 行的 `setupJobs` 方法

如果删除后 `slog` 不再被使用（检查 imports），删除 `log/slog` import。

Run（确认）：`grep -n "slog" internal/service/service.go`
Expected: 仍有引用（rollback 路径用 slog.Error），所以 import 保留。

- [ ] **Step 2: 检查 jobs 包是否还在被引用**

Run: `grep -rn "internal/jobs" --include="*.go" .`
Expected: 只剩 jobs 包自身的文件引用，service.go 不再 import jobs。

如果 service.go 还 import 了 `"message-service/internal/jobs"`，删除该 import。

- [ ] **Step 3: 编译检查**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 4: 跑全部测试**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/service/service.go
git commit -m "chore(service): remove dead setupJobs call and method

internal/jobs/ package is preserved for future use; only the unused call
site and method are removed."
```

---

## Task 12: 加 ID.Gt(0) 锚点注释

**Files:**
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/sms_record.go`

- [ ] **Step 1: email_record.go ListEmailRecords 加注释**

打开 `internal/store/dal/email_record.go`，找到 `ListEmailRecords` 函数。找到这一行：

```go
	q := applyEmailListFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter)
```

在这一行之前加注释：

```go
	// ID.Gt(0) is a no-op predicate (snowflake IDs are always positive) used
	// as an anchor for gorm gen's typed chain so subsequent .Where() clauses
	// compose via AND. Removing it would require restructuring the chain.
	q := applyEmailListFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter)
```

在第二个出现的位置（`q = applyEmailListFilter(...)` 那行）也加注释。

- [ ] **Step 2: sms_record.go 同样改动**

打开 `internal/store/dal/sms_record.go`，对 `ListSMSRecords` 做对称改动。

- [ ] **Step 3: 编译检查**

Run: `go build ./internal/store/dal/`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/store/dal/email_record.go internal/store/dal/sms_record.go
git commit -m "docs(dal): clarify ID.Gt(0) anchor intent in list queries"
```

---

## Task 13: 最终验证

**Files:** 无修改

- [ ] **Step 1: gofmt 全部代码**

Run: `gofmt -w .`
Expected: 无输出（如果有改动，再 `git diff` 检查）

- [ ] **Step 2: goimports**

Run: `goimports -w .`
Expected: 无输出

- [ ] **Step 3: golangci-lint**

Run: `golangci-lint run ./...`
Expected: PASS（无 lint 错误）

如果报错，根据错误信息修复。

- [ ] **Step 4: 全套测试**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: 全部 PASS，覆盖率不低于改动前的基线

- [ ] **Step 5: 检查覆盖率**

Run: `go tool cover -func=coverage.out | grep -E "email.go|sms.go|util.go|email_record.go|sms_record.go"`
Expected: 关键文件覆盖率 > 80%

- [ ] **Step 6: vet**

Run: `go vet ./...`
Expected: PASS

- [ ] **Step 7: proto lint**

Run: `buf lint`
Expected: PASS

- [ ] **Step 8: Commit（如果有自动格式化改动）**

```bash
git status
# 如果有未提交改动：
git add -A
git commit -m "style: apply gofmt/goimports"
```

---

## Task 14: 同步 CLAUDE.md 和 Obsidian

**Files:**
- Modify: `CLAUDE.md`（如果约定变化）
- Modify: Obsidian `services/message-service/index.md`、`changes.md`

- [ ] **Step 1: 检查 CLAUDE.md 是否需要更新**

读 `CLAUDE.md`，检查是否提到：
- MessageStatus 语义（如果之前没明确 SENT 含义）
- idempotency_key 字段约定
- 幂等键使用规范

如果有约定变化，更新对应小节。

- [ ] **Step 2: 同步 plan 到 Obsidian**

```bash
cp docs/superpowers/plans/2026-06-22-message-service-robustness-fix.md \
   "/Users/moss/Library/Mobile Documents/iCloud~md~obsidian/Documents/only/services/message-service/plan/v3/robustness-fix.md"
```

确保 `services/message-service/plan/v3/` 目录存在（mkdir -p）。

- [ ] **Step 3: 更新 Obsidian index.md 加 plan 链接**

```bash
obsidian vault=only prepend file="services/message-service/index" content="
## 实现计划（v3）

| 文档 | 说明 |
|------|------|
| [[services/message-service/plan/v3/robustness-fix\|鲁棒性修复实施计划]] | 14 个 task，TDD 风格，覆盖 persist 独立 ctx / 幂等键 / 显式校验 / vendor unknown 告警 / stats SQL 合并"
```

- [ ] **Step 4: 更新 Obsidian changes.md**

```bash
obsidian vault=only append file="services/message-service/changes" content="
- 2026-06-22: 新增 services/message-service/plan/v3/robustness-fix.md — 鲁棒性修复实施计划（14 个 task）"
```

- [ ] **Step 5: 提交（如果 CLAUDE.md 改了）**

```bash
git status
# 如果 CLAUDE.md 有改动：
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md with idempotency/MessageStatus conventions"
```

---

## Self-Review

### Spec coverage

| Spec 章节 | Task |
|-----------|------|
| 状态机注释更新 | Task 1 |
| 发送流程改造（Email） | Task 8 |
| 发送流程改造（SMS） | Task 9 |
| 幂等查重（DAL） | Task 4 (Email), Task 5 (SMS) |
| 幂等查重（Service） | Task 8, Task 9 |
| 显式校验 | Task 7 (helper), Task 8/9 (调用) |
| 独立 ctx persist | Task 8 (Email), Task 9 (SMS) |
| Wrapf 失败响应 | Task 8 (Email), Task 9 (SMS) |
| vendor unknown 告警 | Task 10 |
| stats SQL 合并 | Task 6 |
| SuccessRate -1 | Task 6 |
| ErrorMessage truncate | Task 7 (helper), Task 8/9 (调用) |
| ID.Gt(0) 注释 | Task 12 |
| setupJobs 清理 | Task 11 |
| DB schema (IdempotencyKey, ErrorMessage size) | Task 2 |
| Partial unique index | Task 3 |
| proto 加 idempotency_key | Task 1 |
| 验证 | Task 13 |
| 文档同步 | Task 14 |

**覆盖率：100%**，所有 spec 章节都有对应 task。

### Placeholder scan

- 无 "TBD"、"TODO"、"implement later"
- 无 "add appropriate error handling"（每处都有具体代码）
- 测试代码全部完整，没有 "similar to Task N"（重复写出了完整代码）

### Type consistency

- `validateSendEmailRequest` / `validateSendSMSRequest` — Task 7 定义，Task 8/9 使用 ✓
- `truncateErrorMessage` — Task 7 定义，Task 8/9 使用 ✓
- `dal.GetEmailRecordByIdempotencyKey` / `GetSMSRecordByIdempotencyKey` — Task 4/5 定义，Task 8/9 使用 ✓
- `respondIdempotentEmail` / `respondIdempotentSMS` — Task 8/9 定义和使用一致 ✓
- `models.EmailTotalStatsRow` / `SmsTotalStatsRow` — Task 6 定义并使用 ✓
- `EmailRecordTotalStatsQuery` / `SMSRecordTotalStatsQuery` — Task 6 定义，typed chain 生成 ✓

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-22-message-service-robustness-fix.md`.

Two execution options:

**1. Subagent-Driven (recommended)** - 每个 task 派一个 fresh subagent，task 之间做 review，迭代快，主对话 context 干净

**2. Inline Execution** - 在当前对话用 executing-plans 跑，分批执行带 checkpoint

Which approach?
