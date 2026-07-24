# Proto Enum Unification — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove `internal/enum`, use proto-generated enums as single source of truth across all layers.

**Architecture:** Model stores raw int32 (proto enum numeric values). Service layer casts `int32 ↔ pb.XXX` at boundaries using proto native conversion. Provider string→enum mapping lives in email/sms packages respectively.

**Tech Stack:** Go, protobuf, GORM, gRPC

---

## File Map

| Action | File | Responsibility |
|--------|------|----------------|
| Create | `internal/email/provider.go` | Email provider name constants + string→pb.Provider mapping |
| Create | `internal/sms/provider.go` | SMS provider name constants + string→pb.Provider mapping |
| Modify | `internal/store/models/message_record.go` | Channel/Provider/Status: string → int32 |
| Delete | `internal/enum/enum.go` | Remove entire package |
| Modify | `internal/store/repository/message_repository.go` | Filter types → pb enums, UpdateStatus/UpdateError → int32, raw SQL → int constants |
| Modify | `internal/store/repository/message_repository_test.go` | Use pb enum constants |
| Modify | `internal/email/email_service.go` | SendResponse types → pb enums, hook uses ProviderToProto |
| Modify | `internal/email/email_service_test.go` | Mock repo + assertions use pb types |
| Modify | `internal/sms/sms_service.go` | Same pattern as email |
| Modify | `internal/sms/sms_service_test.go` | Same pattern as email |
| Modify | `internal/service/message_service.go` | Delete 6 switch functions, direct casts |
| Modify | `internal/query/query_service_test.go` | Use pb enum constants |
| Regenerate | `internal/store/generated/message_record.go` | `gorm gen` after model change |
| Modify | `CLAUDE.md` | Update enum conventions |

---

### Task 1: Create provider mapping files

Non-breaking. Additive only.

**Files:**
- Create: `internal/email/provider.go`
- Create: `internal/sms/provider.go`

- [ ] **Step 1: Create `internal/email/provider.go`**

```go
package email

import pb "message-service/gen/message/v1"

// Provider name constants matching go-common/message/email sender Name() values.
const (
	ProviderSMTP    = "smtp"
	ProviderMailgun = "mailgun"
)

var providerToProto = map[string]pb.Provider{
	ProviderSMTP:    pb.Provider_PROVIDER_SMTP,
	ProviderMailgun: pb.Provider_PROVIDER_MAILGUN,
}

// ProviderToProto converts a go-common provider name to a proto Provider enum.
func ProviderToProto(name string) pb.Provider {
	if p, ok := providerToProto[name]; ok {
		return p
	}
	return pb.Provider_PROVIDER_UNSPECIFIED
}
```

- [ ] **Step 2: Create `internal/sms/provider.go`**

```go
package sms

import pb "message-service/gen/message/v1"

// Provider name constants matching go-common/message/sms sender Name() values.
const (
	ProviderAliyun = "aliyun"
)

var providerToProto = map[string]pb.Provider{
	ProviderAliyun: pb.Provider_PROVIDER_ALIYUN,
}

// ProviderToProto converts a go-common provider name to a proto Provider enum.
func ProviderToProto(name string) pb.Provider {
	if p, ok := providerToProto[name]; ok {
		return p
	}
	return pb.Provider_PROVIDER_UNSPECIFIED
}
```

- [ ] **Step 3: Verify new files compile**

Run: `go build ./internal/email/ ./internal/sms/`
Expected: success (no existing code is affected)

- [ ] **Step 4: Commit**

```bash
git add internal/email/provider.go internal/sms/provider.go
git commit -m "feat: add provider name→proto enum mapping in email/sms packages"
```

---

### Task 2: Atomic refactor — model, enum deletion, all consumers

This is an atomic change. Nothing compiles until ALL sub-steps are done. Do not run `go build` until Step 13.

**Files:**
- Modify: `internal/store/models/message_record.go`
- Delete: `internal/enum/enum.go`
- Modify: `internal/email/email_service.go`
- Modify: `internal/email/email_service_test.go`
- Modify: `internal/sms/sms_service.go`
- Modify: `internal/sms/sms_service_test.go`
- Modify: `internal/store/repository/message_repository.go`
- Modify: `internal/store/repository/message_repository_test.go`
- Modify: `internal/service/message_service.go`
- Modify: `internal/query/query_service_test.go`

- [ ] **Step 1: Change model — `internal/store/models/message_record.go`**

Change three field types from `string` to `int32`. Remove `default:pending`, add `default:0`. Remove import of enum (there is none — it never imported enum directly).

Replace the struct definition:

```go
type MessageRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Channel        int32           `gorm:"not null;index"`
	Provider       int32           `gorm:"not null;index"`
	Status         int32           `gorm:"not null;default:0;index"`
	Target         string          `gorm:"size:255;not null;index"`
	Subject        string          `gorm:"type:text"`
	Content        string          `gorm:"type:text"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`
	SenderID       string          `gorm:"size:64;column:sender_id"`
	ErrorMessage   string          `gorm:"column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time       `gorm:"not null;default:now()"`
}
```

Keep `MapStringString`, its `Scan`/`Value` methods, and all other imports unchanged.

- [ ] **Step 2: Delete `internal/enum/` directory**

```bash
rm -rf internal/enum/
```

- [ ] **Step 3: Update `internal/email/email_service.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**SendResponse struct:**

```go
type SendResponse struct {
	ID       int64
	Status   pb.MessageStatus
	Provider pb.Provider
}
```

**MessageRepo interface:**

```go
type MessageRepo interface {
	Create(ctx context.Context, record *models.MessageRecord) error
	UpdateStatus(ctx context.Context, id int64, status, provider int32, attempts int, sentAt time.Time) error
	UpdateError(ctx context.Context, id int64, provider int32, attempts int, errMsg string) error
}
```

**In `Send()` method** — change record creation and response:

```go
record := &models.MessageRecord{
	ID:             id,
	Channel:        int32(pb.Channel_CHANNEL_EMAIL),
	Provider:       0,
	Status:         int32(pb.MessageStatus_MESSAGE_STATUS_PENDING),
	// ... rest unchanged
}
```

Return statement at end of Send():

```go
return &SendResponse{
	ID:       id,
	Status:   pb.MessageStatus_MESSAGE_STATUS_SENT,
	Provider: ProviderToProto(holder.provider),
}, nil
```

**In `hook()` method:**

```go
func (s *Service) hook(ctx context.Context, result *email.SendResult) {
	recordID, ok := recordIDFromCtx(ctx)
	if !ok {
		return
	}

	if result.Success {
		now := time.Now()
		_ = s.repo.UpdateStatus(ctx, recordID, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), int32(ProviderToProto(result.Provider)), result.Attempts, now)

		if holder, ok := resultHolderFromCtx(ctx); ok {
			holder.provider = result.Provider
		}
	} else {
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		_ = s.repo.UpdateError(ctx, recordID, int32(ProviderToProto(result.Provider)), result.Attempts, errMsg)
	}
}
```

Note: `sendResultHolder.provider` stays `string` — it stores raw go-common name.

- [ ] **Step 4: Update `internal/email/email_service_test.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**mockRepo — change method signatures:**

```go
func (r *mockRepo) UpdateStatus(_ context.Context, id int64, status, provider int32, attempts int, sentAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok {
		return nil
	}
	rec.Status = status
	rec.Provider = provider
	rec.Attempts = attempts
	rec.SentAt = sql.NullTime{Time: sentAt, Valid: true}
	return nil
}

func (r *mockRepo) UpdateError(_ context.Context, id int64, provider int32, attempts int, errMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok {
		return nil
	}
	rec.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
	rec.Provider = provider
	rec.Attempts = attempts
	rec.ErrorMessage = errMsg
	return nil
}
```

**Test assertions — replace all `enum.XXX` with pb constants:**

Replace map:

| Before | After |
|--------|-------|
| `enum.StatusSent` | `pb.MessageStatus_MESSAGE_STATUS_SENT` |
| `enum.StatusPending` | `pb.MessageStatus_MESSAGE_STATUS_PENDING` |
| `enum.StatusFailed` | `pb.MessageStatus_MESSAGE_STATUS_FAILED` |
| `enum.ChannelEmail` | `int32(pb.Channel_CHANNEL_EMAIL)` |
| `enum.ProviderSMTP` | `int32(pb.Provider_PROVIDER_SMTP)` |

Note: `rec.Status` is now `int32`, so assertions compare against `int32(pb.XXX)`.
Note: `rec.Channel` is now `int32`, so assertions compare against `int32(pb.Channel_CHANNEL_EMAIL)`.
Note: `rec.Provider` is now `int32`, so compare against `int32(pb.Provider_PROVIDER_SMTP)` or against `"mock"` via `ProviderToProto`.

Special case for mock provider: `"mock"` is not in the provider mapping, so `ProviderToProto("mock")` returns `pb.Provider_PROVIDER_UNSPECIFIED`. In tests that check mock provider name, compare the raw record provider field differently — but since `UpdateStatus` receives `int32(ProviderToProto(result.Provider))`, the mock provider will be stored as `int32(pb.Provider_PROVIDER_UNSPECIFIED)` (0). Update assertions accordingly.

For `resp.Status` assertions (SendResponse.Status is now `pb.MessageStatus`):

```go
assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
```

For `resp.Provider` assertions (SendResponse.Provider is now `pb.Provider`):

```go
// mock provider → PROVIDER_UNSPECIFIED
assert.Equal(t, pb.Provider_PROVIDER_UNSPECIFIED, resp.Provider)

// For backup/mockNamedProvider named "backup" → also UNSPECIFIED
assert.Equal(t, pb.Provider_PROVIDER_UNSPECIFIED, resp.Provider)
```

For `rec.Channel` assertions (model field is int32):

```go
assert.Equal(t, int32(pb.Channel_CHANNEL_EMAIL), rec.Channel)
```

For `rec.Status` assertions (model field is int32):

```go
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), rec.Status)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), rec.Status)
```

For `rec.Provider` assertions — since mock providers aren't in the mapping, the hook converts them to `PROVIDER_UNSPECIFIED` (0):

```go
assert.Equal(t, int32(pb.Provider_PROVIDER_UNSPECIFIED), rec.Provider)
```

Wait — but there are named mock providers like `"backup"`, `"smtp"`, `"mailgun"`. `"smtp"` and `"mailgun"` ARE in the mapping. So for `mockNamedProvider{name: "backup"}`, the provider would be `PROVIDER_UNSPECIFIED`. For `mockNamedProvider{name: "smtp"}`, it would be `PROVIDER_SMTP`.

Test `TestService_Send_AllProvidersFail` uses providers named `"smtp"` and `"mailgun"`:

```go
rec := repo.getRecord(4004)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), rec.Status)
assert.Equal(t, int32(pb.Provider_PROVIDER_MAILGUN), rec.Provider) // last provider tried
```

Test `TestService_Send_MultipleProviders_Success` uses `mockNamedProvider{name: "backup"}`:

```go
rec := repo.getRecord(3003)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), rec.Status)
assert.Equal(t, int32(pb.Provider_PROVIDER_UNSPECIFIED), rec.Provider) // "backup" not in mapping
```

Test `TestService_Send_Success` uses `mockProvider` (Name() returns `"mock"`):

```go
assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
assert.Equal(t, pb.Provider_PROVIDER_UNSPECIFIED, resp.Provider)
rec := repo.getRecord(1001)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), rec.Status)
assert.Equal(t, int32(pb.Provider_PROVIDER_UNSPECIFIED), rec.Provider)
```

Test `TestService_Send_Failure`:

```go
rec := repo.getRecord(2002)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), rec.Status)
assert.Equal(t, int32(pb.Provider_PROVIDER_UNSPECIFIED), rec.Provider)
```

- [ ] **Step 5: Update `internal/sms/sms_service.go`**

Same pattern as email. Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**SendResponse:**

```go
type SendResponse struct {
	ID       int64
	Status   pb.MessageStatus
	Provider pb.Provider
}
```

**MessageRepo interface:**

```go
type MessageRepo interface {
	Create(ctx context.Context, record *models.MessageRecord) error
	FindByID(ctx context.Context, id int64) (*models.MessageRecord, error)
	UpdateStatus(ctx context.Context, id int64, status, provider int32, attempts int, sentAt time.Time) error
	UpdateError(ctx context.Context, id int64, provider int32, attempts int, errMsg string) error
}
```

**In `Send()` — record creation:**

```go
record := &models.MessageRecord{
	ID:             id,
	Channel:        int32(pb.Channel_CHANNEL_SMS),
	Provider:       0,
	Status:         int32(pb.MessageStatus_MESSAGE_STATUS_PENDING),
	// ... rest unchanged
}
```

**In `Send()` — fallback response:**

```go
return &SendResponse{ID: id, Status: pb.MessageStatus_MESSAGE_STATUS_SENT, Provider: 0}, nil
```

**In `Send()` — final return:**

```go
return &SendResponse{
	ID:       id,
	Status:   pb.MessageStatus(updated.Status),
	Provider: pb.Provider(updated.Provider),
}, nil
```

**In `hook()`:**

```go
func (s *Service) hook(ctx context.Context, result *sms.SendResult) {
	recordID, ok := recordIDFromCtx(ctx)
	if !ok {
		return
	}

	if result.Success {
		now := time.Now()
		_ = s.repo.UpdateStatus(ctx, recordID, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), int32(ProviderToProto(result.Provider)), result.Attempts, now)
	} else {
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		_ = s.repo.UpdateError(ctx, recordID, int32(ProviderToProto(result.Provider)), result.Attempts, errMsg)
	}
}
```

Remove `"fmt"` from imports if it becomes unused (the `var _ = fmt.Sprintf` line at bottom). Actually check if fmt is still used — the `fmt.Errorf` in FindByID mock and `"not found"` usage. In the actual sms_service.go, fmt is imported but only the `var _ = fmt.Sprintf` line uses it. Remove that line and the fmt import.

- [ ] **Step 6: Update `internal/sms/sms_service_test.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**mockRepo — change method signatures and body:**

```go
func (m *mockRepo) UpdateStatus(_ context.Context, id int64, status, provider int32, attempts int, sentAt time.Time) error {
	r, ok := m.records[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	r.Status = status
	r.Provider = provider
	r.Attempts = attempts
	r.SentAt = sql.NullTime{Time: sentAt, Valid: true}
	return nil
}

func (m *mockRepo) UpdateError(_ context.Context, id int64, provider int32, attempts int, errMsg string) error {
	r, ok := m.records[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	r.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
	r.Provider = provider
	r.Attempts = attempts
	r.ErrorMessage = errMsg
	return nil
}
```

**Test assertions:**

```go
// TestSMSService_Send_Success
assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
assert.Equal(t, pb.Provider_PROVIDER_UNSPECIFIED, resp.Provider) // "mock" not in mapping
assert.Equal(t, int64(123456), resp.ID)

record := repo.records[123456]
assert.NotNil(t, record)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), record.Status)
assert.Equal(t, int32(pb.Provider_PROVIDER_UNSPECIFIED), record.Provider)
```

```go
// TestSMSService_Send_Failed
// resp is nil, just check record
record := repo.records[123456]
assert.NotNil(t, record)
assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_FAILED), record.Status)
```

- [ ] **Step 7: Update `internal/store/repository/message_repository.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**ListFilter:**

```go
type ListFilter struct {
	Channel   pb.Channel
	Status    pb.MessageStatus
	Target    string
	Provider  pb.Provider
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}
```

**StatsFilter:**

```go
type StatsFilter struct {
	Channel   pb.Channel
	Provider  pb.Provider
	StartTime *time.Time
	EndTime   *time.Time
}
```

**ProviderStats:**

```go
type ProviderStats struct {
	Provider int32
	Total    int64
	Sent     int64
	Failed   int64
}
```

**UpdateStatus signature:**

```go
func (r *MessageRepository) UpdateStatus(ctx context.Context, id int64, status, provider int32, attempts int, sentAt time.Time) error {
```

Body unchanged (generated code `.Set()` accepts int32 after regeneration).

**UpdateError signature:**

```go
func (r *MessageRepository) UpdateError(ctx context.Context, id int64, provider int32, attempts int, errMsg string) error {
```

Body: replace `enum.StatusFailed` with `int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)`.

**Stats method — raw SQL sent/failed counts:**

Replace:
```go
sent, err := q.
    Where(generated.MessageRecord.Status.Eq(enum.StatusSent)).
    Count(ctx, "id")
```
With:
```go
sent, err := q.
    Where(generated.MessageRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_SENT))).
    Count(ctx, "id")
```

Similarly for failed:
```go
failed, err := q.
    Where(generated.MessageRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_FAILED))).
    Count(ctx, "id")
```

**ProviderStats method — raw SQL:**

Replace the Select string:
```go
"provider, COUNT(*) as total, COUNT(*) FILTER (WHERE status = 'sent') as sent, COUNT(*) FILTER (WHERE status = 'failed') as failed"
```
With:
```go
"provider, COUNT(*) as total, COUNT(*) FILTER (WHERE status = 2) as sent, COUNT(*) FILTER (WHERE status = 3) as failed"
```

(2 = MESSAGE_STATUS_SENT, 3 = MESSAGE_STATUS_FAILED)

**applyListFilter — change type checks:**

```go
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
	if f.Provider != 0 {
		q = q.Where(generated.MessageRecord.Provider.Eq(int32(f.Provider)))
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

**applyStatsFilter — same pattern:**

```go
func applyStatsFilter(q gorm.ChainInterface[models.MessageRecord], f StatsFilter) gorm.ChainInterface[models.MessageRecord] {
	if f.Channel != 0 {
		q = q.Where(generated.MessageRecord.Channel.Eq(int32(f.Channel)))
	}
	if f.Provider != 0 {
		q = q.Where(generated.MessageRecord.Provider.Eq(int32(f.Provider)))
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

- [ ] **Step 8: Update `internal/store/repository/message_repository_test.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**newTestRecord — change params and values:**

```go
func newTestRecord(channel int32, status int32, target string, provider int32) *models.MessageRecord {
	return &models.MessageRecord{
		ID:       time.Now().UnixNano(),
		Channel:  channel,
		Provider: provider,
		Status:   status,
		Target:   target,
		Subject:  "Test Subject",
		Content:  "Test content body",
		Attempts: 1,
	}
}
```

**Replace all `enum.XXX` references:**

| Before | After |
|--------|-------|
| `enum.ChannelEmail` | `int32(pb.Channel_CHANNEL_EMAIL)` |
| `enum.ChannelSMS` | `int32(pb.Channel_CHANNEL_SMS)` |
| `enum.StatusPending` | `int32(pb.MessageStatus_MESSAGE_STATUS_PENDING)` |
| `enum.StatusSent` | `int32(pb.MessageStatus_MESSAGE_STATUS_SENT)` |
| `enum.StatusFailed` | `int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)` |
| `enum.ProviderSMTP` | `int32(pb.Provider_PROVIDER_SMTP)` |
| `enum.ProviderMailgun` | `int32(pb.Provider_PROVIDER_MAILGUN)` |
| `enum.ProviderAliyun` | `int32(pb.Provider_PROVIDER_ALIYUN)` |

**TestMessageRepository_UpdateStatus — UpdateStatus call:**

```go
err := repo.UpdateStatus(ctx, record.ID, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), int32(pb.Provider_PROVIDER_MAILGUN), 2, sentAt)
```

**TestMessageRepository_UpdateError — UpdateError call:**

```go
err := repo.UpdateError(ctx, record.ID, int32(pb.Provider_PROVIDER_ALIYUN), 3, "connection timeout")
```

**TestMessageRepository_List_WithFilter — filter construction:**

```go
records, total, err := repo.List(ctx, ListFilter{
	Channel:  pb.Channel_CHANNEL_EMAIL,
	Page:     1,
	PageSize: 10,
})
```

**TestMessageRepository_Stats_WithChannelFilter:**

```go
stats, err := repo.Stats(ctx, StatsFilter{Channel: pb.Channel_CHANNEL_EMAIL})
```

**TestMessageRepository_ProviderStats — map key type changes:**

ProviderStats.Provider is now `int32`, so the map key changes:

```go
m := make(map[int32]ProviderStats)
for _, s := range stats {
	m[s.Provider] = s
}

smtpStat, ok := m[int32(pb.Provider_PROVIDER_SMTP)]
require.True(t, ok, "smtp provider should exist")
assert.Equal(t, int64(3), smtpStat.Total)
assert.Equal(t, int64(2), smtpStat.Sent)
assert.Equal(t, int64(1), smtpStat.Failed)

aliyunStat, ok := m[int32(pb.Provider_PROVIDER_ALIYUN)]
require.True(t, ok, "aliyun provider should exist")
assert.Equal(t, int64(2), aliyunStat.Total)
assert.Equal(t, int64(1), aliyunStat.Sent)
assert.Equal(t, int64(1), aliyunStat.Failed)
```

- [ ] **Step 9: Update `internal/service/message_service.go`**

Imports: remove `"message-service/internal/enum"`, keep the email import (already exists). The import path changes from referencing enum to just email.

**Delete these 6 functions entirely (lines 315-389):**
- `statusToProto`
- `statusFromProto`
- `channelToProto`
- `channelFromProto`
- `providerToProto`
- `providerFromProto`

**In `SendEmail()` — response construction:**

```go
return &pb.SendResponse{
	Id:       resp.ID,
	Status:   resp.Status,
	Provider: resp.Provider,
}, nil
```

(No conversion needed — resp.Status is already `pb.MessageStatus`, resp.Provider is already `pb.Provider`.)

**In `SendSMS()` — response construction:**

```go
return &pb.SendResponse{
	Id:       resp.ID,
	Status:   resp.Status,
	Provider: resp.Provider,
}, nil
```

**In `GetMessage()` — no change needed** (toProtoRecord handles it).

**In `ListMessages()` — filter construction uses pb types directly:**

```go
f := repository.ListFilter{
	Channel:  req.GetChannel(),
	Status:   req.GetStatus(),
	Target:   req.GetTarget(),
	Provider: req.GetProvider(),
	Page:     req.GetPage(),
	PageSize: req.GetPageSize(),
}
```

(No `xxxFromProto` calls needed — req.GetChannel() returns `pb.Channel` which is the filter type.)

**In `GetMessageStats()` — same pattern:**

```go
f := repository.StatsFilter{
	Channel:  req.GetChannel(),
	Provider: req.GetProvider(),
}
```

**In `toProtoRecord()` — direct casts:**

```go
func toProtoRecord(r *models.MessageRecord) *pb.MessageRecord {
	rec := &pb.MessageRecord{
		Id:             r.ID,
		Channel:        pb.Channel(r.Channel),
		Provider:       pb.Provider(r.Provider),
		Status:         pb.MessageStatus(r.Status),
		Target:         r.Target,
		Subject:        r.Subject,
		Content:        r.Content,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		SenderId:       r.SenderID,
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}
```

Note: rename local variable from `pb` to `rec` to avoid shadowing the import.

**In `buildEmailProviders()` — replace enum constants:**

```go
switch pc.Type {
case email.ProviderSMTP:
    // ...
case email.ProviderMailgun:
    // ...
```

**In provider stats construction:**

```go
pbProvStats[i] = &pb.ProviderStats{
    Provider: pb.Provider(ps.Provider),
    Total:    ps.Total,
    Sent:     ps.Sent,
    Failed:   ps.Failed,
}
```

- [ ] **Step 10: Update `internal/query/query_service_test.go`**

Imports: remove `"message-service/internal/enum"`, add `pb "message-service/gen/message/v1"`.

**newTestRecord — same change as repository test:**

```go
func newTestRecord(channel, status int32, target string, provider int32) *models.MessageRecord {
	return &models.MessageRecord{
		ID:       time.Now().UnixNano(),
		Channel:  channel,
		Provider: provider,
		Status:   status,
		Target:   target,
		Subject:  "Test Subject",
		Content:  "Test content body",
		Attempts: 1,
	}
}
```

**Replace all `enum.XXX` references** with pb constants (same mapping table as Step 8).

**TestQueryService_Stats — provider stats map:**

```go
m := make(map[int32]repository.ProviderStats)
for _, s := range provStats {
	m[s.Provider] = s
}

smtpStat, ok := m[int32(pb.Provider_PROVIDER_SMTP)]
// ... etc
```

- [ ] **Step 11: Verify compilation**

Run: `go build ./...`
Expected: compiles without errors

- [ ] **Step 12: Commit**

```bash
git add -A
git commit -m "refactor: replace internal/enum with proto-generated enums across all layers"
```

---

### Task 3: Regenerate GORM code

After the model field types changed from `string` to `int32`, the generated code must be regenerated.

**Files:**
- Regenerate: `internal/store/generated/message_record.go`

- [ ] **Step 1: Run GORM code generation**

Run: `gorm gen`
Expected: `internal/store/generated/message_record.go` regenerated with `field.Number[int32]` for Channel, Provider, Status fields (instead of `field.String`).

- [ ] **Step 2: Verify generated code looks correct**

Spot-check that `generated.MessageRecord.Status` is `field.Number[int32]` (not `field.String`).

- [ ] **Step 3: Commit**

```bash
git add internal/store/generated/
git commit -m "chore: regenerate GORM code for int32 enum fields"
```

---

### Task 4: Run all tests and verify

- [ ] **Step 1: Drop and recreate test database tables**

If test containers persist between runs, the column type change (string→int32) may cause issues. Run:

```bash
go test -race -count=1 ./...
```

AutoMigrate in test setup (`setupRepo`/`setupService`) will recreate tables. If tables already exist with string columns, AutoMigrate may not alter them. If tests fail with schema errors, manually drop the test tables or use `db.Migrator().DropTable(&models.MessageRecord{})` before AutoMigrate.

- [ ] **Step 2: Run full test suite**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: all tests pass

- [ ] **Step 3: Run linter**

Run: `golangci-lint run ./...`
Expected: no errors

---

### Task 5: Update CLAUDE.md and final commit

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update enum conventions in CLAUDE.md**

Replace the current enum section. Find the section that says:

> - **有限集合的字段必须使用 proto enum，不用 string**。当前已定义的枚举：
>   - `MessageStatus`（PENDING / SENT / FAILED）
>   - `Channel`（EMAIL / SMS）
>   - `Provider`（SMTP / MAILGUN / ALIYUN）
> - 新增枚举值时同步更新 `internal/enum/enum.go` 中的字符串常量和 `internal/service/message_service.go` 中的转换函数（`xxxToProto` / `xxxFromProto`）
> - Proto enum 用于 API 层，DB 层存储字符串（通过 `internal/enum` 常量），在 service 层做双向转换

Replace with:

> - **有限集合的字段必须使用 proto enum，不用 string**。当前已定义的枚举：
>   - `MessageStatus`（PENDING / SENT / FAILED）
>   - `Channel`（EMAIL / SMS）
>   - `Provider`（SMTP / MAILGUN / ALIYUN）
> - **DB 层直接存 proto enum 的 int32 数字值**，GORM 原生支持，无需自定义 Scan/Value
> - Model 字段用 `int32`，service 层用 `pb.MessageStatus(v)` / `int32(v)` 做双向转换（proto 原生能力）
> - **不使用 `internal/enum` 包**（已删除）
> - go-common/message 返回的 Provider 是字符串（如 `"smtp"`），映射函数在各自包里：
>   - `internal/email/provider.go`：`email.ProviderToProto(name string) pb.Provider`
>   - `internal/sms/provider.go`：`sms.ProviderToProto(name string) pb.Provider`
> - YAML 配置的 `type` 字段保持 string，service 层用 `email.ProviderSMTP` 等常量匹配

- [ ] **Step 2: Update directory structure in CLAUDE.md**

Remove `internal/enum/` from the directory structure listing.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md enum conventions to proto-only approach"
```
