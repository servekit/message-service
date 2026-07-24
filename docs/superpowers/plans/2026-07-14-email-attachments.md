# Email Attachments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OSS-URL-based attachment support to `SendEmail`, with two rendering modes (LINK = URL rendered into HTML body; MIME = bytes downloaded and attached as a real MIME part). Persist attachment metadata to a side table for audit.

**Architecture:**
- **API layer** (`proto`): one `EmailAttachment` message, one `AttachmentKind` enum. The same message appears on `SendEmailRequest` (input) and `EmailRecord` (query response).
- **Service layer** (`internal/service/message/`): branch on `kind`. LINK renders `<a href>`/`<img src>` into HTML body before calling provider. MIME does an HTTP GET against the URL with size-limit + timeout, then constructs a `provider/email.Attachment`.
- **Provider layer** (`internal/provider/email/`): adds an `Attachment` struct (bytes-only) + `Attachments` field on `Message`. `SMTPProvider.Send` calls `gomail.AttachReader` / `EmbedReader`. Provider does NOT know about URLs or kinds.
- **Storage layer** (`internal/store/`): new `message_email_record_attachments` side table keyed by `email_record_id`. No FK (per CLAUDE.md); integrity enforced at app layer. Schema mirrors proto one-to-one.
- **Bytes never enter the DB.** Only metadata (filename, url, kind, mime_type, size_bytes, inline) is persisted.

**Tech Stack:** Go 1.x, buf/protobuf, `github.com/wneessen/go-mail` v0.7.3, Postgres via testcontainers, testify, `net/http` for MIME-mode fetch.

---

## Execution Phasing

Tasks are split into two phases by DB dependency. Run Phase 1 end-to-end first; Phase 2 needs Docker running for testcontainer.

### Phase 1 — No DB required (Docker can be down)
- Task 1: proto types
- Task 2: provider Attachment + SMTP integration
- Task 5: xcodes
- Task 3 steps 1-4: model file + register in `AllModels` + `make generate` + `go build` (skip the migrate-test step)
- Task 6: service helpers (validate / render / fetch)
- Task 7: Service struct wiring + config
- Task 8 steps 1-5: SendEmail integration without the persistence-test step (Step 1 of Task 8 needs DB → move to Phase 2)

### Phase 2 — DB required (Docker must be up)
- Task 3 step 5: verify migrate via `go test ./cmd/migrate/`
- Task 4: DAL functions with testcontainer tests
- Task 8 step 6: persistence tests for SendEmail (TestSendEmail_LINKAttachment_RendersIntoBody, TestSendEmail_MIMEAttachment_FetchFailure)
- Task 9: EmailRecord attachments population (all tests need DB)
- Task 10: full `make test` + `make migrate`

**Subagent dispatch rule:** Phase 1 subagents run with Docker ignored; Phase 2 subagents require `docker info` to succeed first.

---

## File Structure

| File | Role | Operation |
|---|---|---|
| `api/proto/message/v1/message.proto` | Source of truth for proto types | Add `AttachmentKind` enum, `EmailAttachment` message; add `attachments` field on `SendEmailRequest` (16) and `EmailRecord` (21) |
| `gen/message/v1/*.pb.go` | Generated from proto | Regenerate via `make proto` |
| `internal/provider/email/message.go` | Provider-internal types | Add `Attachment` struct; add `Attachments []*Attachment` to `Message` |
| `internal/provider/email/smtp.go` | SMTP send path | Iterate `msg.Attachments`, call `m.AttachReader` / `m.EmbedReader` |
| `internal/provider/email/smtp_test.go` | SMTPProvider tests | Add tests for attachment + inline image paths |
| `internal/store/models/email_record_attachment.go` | New GORM model | New file |
| `internal/store/models/genconfig.go` | Model registry | Register `MessageEmailRecordAttachment` in `AllModels()` |
| `internal/store/generated/email_record_attachment.go` | gorm-generated query API | Regenerate via `make generate` |
| `internal/store/dal/email_record_attachment.go` | New DAL file | `CreateEmailRecordAttachments`, `ListEmailRecordAttachments` |
| `internal/store/dal/email_record_attachment_test.go` | DAL tests | New file |
| `pkg/xcodes/message.go` | Error codes | Add `ErrInvalidAttachment`, `ErrAttachmentFetchFailed`, `ErrAttachmentTooLarge` |
| `internal/service/message/email_attachment.go` | New service helpers | `processAttachments`, `renderLinkHTML`, `fetchAttachmentBytes`, `validateAttachments` |
| `internal/service/message/email_attachment_test.go` | Helper tests | New file |
| `internal/service/message/service.go` | `Service` struct + `New` | Add `httpClient *http.Client`, `attachmentMaxBytes int64` |
| `internal/service/message/email.go` | `SendEmail` flow | Validate attachments; process; pass to provider; persist |
| `internal/service/message/email.go` (`toProtoEmailRecord`) | Proto conversion | Load + populate `attachments` |
| `internal/service/message/email_test.go` | Service tests | Add LINK + MIME + failure-path tests |
| `internal/service/message/util.go` | `validateSendEmailRequest` | Add attachment-validation block (or call into `email_attachment.go`) |
| `pkg/config/config.go` | Config struct | Add `Attachment *AttachmentConfig` |
| `internal/service/service.go` | `service.New` | Construct `http.Client` + max bytes from config, inject into `message.New` |
| `internal/service/message/service.go` (`New` signature) | Constructor | Accept http client + max bytes |
| `config.yaml`, `config.docker.yaml` | Local + docker configs | Add `attachment.fetch_timeout` / `attachment.max_bytes` |

---

## Task 1: Add proto types (`AttachmentKind`, `EmailAttachment`, request/response fields)

**Goal:** Define the wire-level shape. No logic, no validation — that's service-layer's job.

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Regenerate: `make proto`

- [ ] **Step 1: Add `AttachmentKind` enum and `EmailAttachment` message**

Insert after the existing `EmailAddress` message (around line 301):

```proto
// AttachmentKind selects how an attachment is delivered.
enum AttachmentKind {
  ATTACHMENT_KIND_UNSPECIFIED = 0;
  // LINK renders the URL into the HTML body (an <a href> download link or an
  // <img src> for inline images). message-service does NOT fetch the bytes —
  // the recipient's mail client loads the URL on open.
  ATTACHMENT_KIND_LINK = 1;
  // MIME downloads the bytes from URL at send time and attaches them as a
  // real MIME part. Required for compliance/archival/offline scenarios.
  ATTACHMENT_KIND_MIME = 2;
}

// EmailAttachment describes one attachment on an email. URL is the single
// source of bytes (OSS-only design — caller manages object storage). The same
// message type appears on SendEmailRequest (input) and EmailRecord (query).
message EmailAttachment {
  AttachmentKind kind = 1;
  // filename is the MIME filename (MIME mode) or display label (LINK mode).
  // Required.
  string filename = 2 [(buf.validate.field).string.min_len = 1];
  // url is the HTTPS URL of the attachment content. Required.
  string url = 3 [(buf.validate.field).string.min_len = 1];
  // inline selects rendering: true = <img>/CID-embedded; false = <a>/regular
  // attachment. For LINK mode, inline with a non-image MIME type renders as
  // <a> anyway (graceful fallback).
  bool inline = 4;
  // mime_type is optional. Empty = let MIME detection infer from filename.
  string mime_type = 5;
  // size_bytes is optional. Pre-validated against max_bytes in MIME mode when
  // non-zero. Zero = no pre-check (still enforced after download).
  int64 size_bytes = 6;
}
```

- [ ] **Step 2: Add `attachments` field on `SendEmailRequest`**

In `SendEmailRequest`, after `from` (field 15):

```proto
  // attachments is optional. LINK attachments augment html_body; MIME
  // attachments are downloaded and added as real MIME parts. Mixed lists
  // are allowed — each attachment carries its own kind.
  repeated EmailAttachment attachments = 16;
```

- [ ] **Step 3: Add `attachments` field on `EmailRecord`**

In `EmailRecord`, after `updated_at` (field 20):

```proto
  // attachments lists metadata for each attachment recorded at send time.
  // Empty when no attachments were sent or persistence is disabled.
  repeated EmailAttachment attachments = 21;
```

- [ ] **Step 4: Regenerate proto**

Run: `make proto`
Expected: `gen/message/v1/message.pb.go` updated with `AttachmentKind`, `EmailAttachment`, `SendEmailRequest.Attachments`, `EmailRecord.Attachments`.

- [ ] **Step 5: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add api/proto/message/v1/message.proto gen/message/v1/
git commit -m "feat(proto): add EmailAttachment + AttachmentKind"
```

---

## Task 2: Provider-layer `Attachment` type and SMTP integration

**Goal:** `provider/email.Message` gains an `Attachments` field; `SMTPProvider.Send` calls go-mail's `AttachReader` (regular) or `EmbedReader` (inline). Provider stays URL/kind-agnostic — it only knows bytes.

**Files:**
- Modify: `internal/provider/email/message.go`
- Modify: `internal/provider/email/smtp.go`
- Modify: `internal/provider/email/smtp_test.go`

- [ ] **Step 1: Write failing test for regular attachment**

Append to `internal/provider/email/smtp_test.go`:

```go
func TestSMTPProvider_Send_withAttachment(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:      []*Address{{Email: "user@test.com"}},
		Subject: "With attachment",
		Body:    "See attached",
		Attachments: []*Attachment{
			{
				Filename: "report.txt",
				Content:  []byte("hello world"),
				MimeType: "text/plain",
			},
		},
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_withInlineImage(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:       []*Address{{Email: "user@test.com"}},
		Subject:  "With inline image",
		HTMLBody: "<p>Hi</p><img src=\"cid:logo\">",
		Attachments: []*Attachment{
			{
				Filename:  "logo.png",
				Content:   []byte{0x89, 'P', 'N', 'G'},
				MimeType:  "image/png",
				Inline:    true,
				ContentID: "logo",
			},
		},
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/provider/email/ -run TestSMTPProvider_Send_with -v`
Expected: FAIL with compile errors (`Attachment undefined`, `Attachments field missing`).

- [ ] **Step 3: Add `Attachment` struct + `Attachments` field to `Message`**

In `internal/provider/email/message.go`, append:

```go
// Attachment is a MIME-mode attachment. Provider-internal — service layer
// downloads bytes from URL and constructs this. The provider never sees URLs
// or kinds.
type Attachment struct {
	Filename  string // MIME filename; required
	Content   []byte // raw bytes; required
	MimeType  string // optional; empty = let go-mail infer from filename
	Inline    bool   // true = CID-embedded (<img src="cid:...">), false = regular attachment
	ContentID string // required when Inline=true; the CID value used in HTML
}
```

Add to `Message` struct (after `TemplateParams`):

```go
	// Attachments are MIME-mode attachments. The provider embeds each as a
	// MIME part: regular attachment when Inline=false, CID-embedded when
	// Inline=true. Empty slice = no attachments. LINK-mode attachments
	// never reach here — they are rendered into HTMLBody by the service
	// layer before calling Send.
	Attachments []*Attachment
```

- [ ] **Step 4: Wire `Attachments` into `SMTPProvider.Send`**

In `internal/provider/email/smtp.go`, in `Send`, after the body setup block (after line 93, before `DialAndSendWithContext`):

```go
	for _, att := range msg.Attachments {
		if att == nil {
			continue
		}
		reader := bytes.NewReader(att.Content)
		var opts []gomail.FileOption
		if att.MimeType != "" {
			opts = append(opts, gomail.WithMimeType(att.MimeType))
		}
		if att.Inline {
			if att.ContentID == "" {
				return fmt.Errorf("smtp: inline attachment %q missing ContentID", att.Filename)
			}
			opts = append(opts, gomail.WithContentID(att.ContentID))
			if err := m.EmbedReader(att.Filename, reader, opts...); err != nil {
				return fmt.Errorf("smtp: embed %q: %w", att.Filename, err)
			}
		} else {
			if err := m.AttachReader(att.Filename, reader, opts...); err != nil {
				return fmt.Errorf("smtp: attach %q: %w", att.Filename, err)
			}
		}
	}
```

Add `"bytes"` to the import block.

- [ ] **Step 5: Run tests to verify pass**

Run: `go test ./internal/provider/email/ -run TestSMTPProvider_Send_with -v`
Expected: PASS.

- [ ] **Step 6: Run full provider test suite**

Run: `go test ./internal/provider/email/ -v`
Expected: PASS (no regressions).

- [ ] **Step 7: Commit**

```bash
git add internal/provider/email/message.go internal/provider/email/smtp.go internal/provider/email/smtp_test.go
git commit -m "feat(provider/email): add Attachment type + SMTP send path"
```

---

## Task 3: DB model `MessageEmailRecordAttachment`

**Goal:** New side table for attachment metadata. Registered for AutoMigrate.

**Files:**
- Create: `internal/store/models/email_record_attachment.go`
- Modify: `internal/store/models/genconfig.go`

- [ ] **Step 1: Create the model file**

Create `internal/store/models/email_record_attachment.go`:

```go
package models

import "time"

// MessageEmailRecordAttachment stores metadata for one attachment on a sent
// email. Bytes are NOT persisted — the URL column points to the OSS source.
// The "Message" prefix lets GORM's NamingStrategy auto-derive the table name
// "message_email_record_attachments" — same convention as MessageEmailRecord.
//
// No foreign key to MessageEmailRecord (per CLAUDE.md — relation integrity is
// enforced at the application layer). Cascade deletes are NOT used; the
// service layer deletes attachment rows in the same transaction as the
// parent record when needed.
type MessageEmailRecordAttachment struct {
	ID            int64     `gorm:"primaryKey"`
	EmailRecordID int64     `gorm:"column:email_record_id;not null;index"`
	// Kind stores the proto AttachmentKind int value (1=LINK, 2=MIME).
	Kind          int32     `gorm:"not null;default:0"`
	Filename      string    `gorm:"size:255;not null"`
	URL           string    `gorm:"size:2048;column:url;not null"`
	Inline        bool      `gorm:"not null;default:false"`
	MimeType      string    `gorm:"size:127;column:mime_type"`
	SizeBytes     int64     `gorm:"column:size_bytes"`
	CreatedAt     time.Time `gorm:"not null;default:now()"`
}
```

- [ ] **Step 2: Register in `AllModels()`**

In `internal/store/models/genconfig.go`, modify `AllModels`:

```go
func AllModels() []any {
	return []any{
		&MessageEmailRecord{},
		&MessageSMSRecord{},
		&MessageEmailRecordAttachment{},
	}
}
```

- [ ] **Step 3: Regenerate gorm gen code**

Run: `make generate`
Expected: `internal/store/generated/email_record_attachment.go` created.

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 5: Verify migration runs**

Run: `go test ./cmd/migrate/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/models/email_record_attachment.go internal/store/models/genconfig.go internal/store/generated/
git commit -m "feat(store/models): add MessageEmailRecordAttachment"
```

---

## Task 4: DAL functions for attachments

**Goal:** Typed DAL for the new table. Mirrors the email_record DAL pattern: package-level functions taking `*gorm.DB`.

**Files:**
- Create: `internal/store/dal/email_record_attachment.go`
- Create: `internal/store/dal/email_record_attachment_test.go`

- [ ] **Step 1: Write failing test for `CreateEmailRecordAttachments`**

Create `internal/store/dal/email_record_attachment_test.go`:

```go
package dal

import (
	"context"
	"testing"

	"github.com/servekit/message-service/internal/store/models"

	pb "github.com/servekit/message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupEmailAttachmentDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.MessageEmailRecord{}, &models.MessageEmailRecordAttachment{}))
	return db
}

func TestCreateEmailRecordAttachments(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	atts := []*models.MessageEmailRecordAttachment{
		{
			EmailRecordID: 100,
			Kind:          int32(pb.AttachmentKind_ATTACHMENT_KIND_LINK),
			Filename:      "report.pdf",
			URL:           "https://oss.example.com/report.pdf",
			Inline:        false,
			MimeType:      "application/pdf",
			SizeBytes:     1024,
		},
		{
			EmailRecordID: 100,
			Kind:          int32(pb.AttachmentKind_ATTACHMENT_KIND_MIME),
			Filename:      "logo.png",
			URL:           "https://oss.example.com/logo.png",
			Inline:        true,
			MimeType:      "image/png",
		},
	}
	require.NoError(t, CreateEmailRecordAttachments(ctx, db, atts))

	found, err := ListEmailRecordAttachments(ctx, db, 100)
	require.NoError(t, err)
	assert.Len(t, found, 2)
	assert.Equal(t, "report.pdf", found[0].Filename)
	assert.Equal(t, "logo.png", found[1].Filename)
}

func TestListEmailRecordAttachments_empty(t *testing.T) {
	db := setupEmailAttachmentDB(t)
	ctx := context.Background()

	found, err := ListEmailRecordAttachments(ctx, db, 999)
	require.NoError(t, err)
	assert.Empty(t, found)
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/store/dal/ -run EmailRecordAttachment -v`
Expected: FAIL with compile errors (`CreateEmailRecordAttachments undefined`).

- [ ] **Step 3: Implement the DAL functions**

Create `internal/store/dal/email_record_attachment.go`:

```go
package dal

import (
	"context"

	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"gorm.io/gorm"
)

// CreateEmailRecordAttachments inserts all attachment rows in one call.
// Caller is responsible for ordering. Empty slice is a no-op.
func CreateEmailRecordAttachments(ctx context.Context, tx *gorm.DB, rows []*models.MessageEmailRecordAttachment) error {
	if len(rows) == 0 {
		return nil
	}
	if err := gorm.G[models.MessageEmailRecordAttachment](tx).Create(ctx, rows...); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// ListEmailRecordAttachments returns all attachments for a given email record
// ordered by id ascending (preserves send-time ordering). Returns an empty
// (non-nil) slice when none exist.
func ListEmailRecordAttachments(ctx context.Context, tx *gorm.DB, emailRecordID int64) ([]*models.MessageEmailRecordAttachment, error) {
	rows, err := gorm.G[models.MessageEmailRecordAttachment](tx).
		Where(generated.MessageEmailRecordAttachment.EmailRecordID.Eq(emailRecordID)).
		Order(generated.MessageEmailRecordAttachment.ID.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*models.MessageEmailRecordAttachment, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/store/dal/ -run EmailRecordAttachment -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/dal/email_record_attachment.go internal/store/dal/email_record_attachment_test.go
git commit -m "feat(store/dal): add email record attachment CRUD"
```

---

## Task 5: Add xcodes

**Goal:** Error codes for attachment validation / fetch / size failures.

**Files:**
- Modify: `pkg/xcodes/message.go`

- [ ] **Step 1: Add error codes**

Append to `pkg/xcodes/message.go`:

```go
// ErrInvalidAttachment indicates an attachment in SendEmailRequest failed
// validation (kind UNSPECIFIED, empty url, empty filename, or conflicting
// inline settings).
var ErrInvalidAttachment = xerr.New(
	"INVALID_ATTACHMENT",
	xerr.CategoryBadRequest,
	400,
	"invalid attachment",
)

// ErrAttachmentTooLarge indicates a MIME-mode attachment exceeded the
// configured max_bytes after download (or pre-download when size_bytes was
// provided and exceeded).
var ErrAttachmentTooLarge = xerr.New(
	"ATTACHMENT_TOO_LARGE",
	xerr.CategoryBadRequest,
	413,
	"attachment exceeds size limit",
)

// ErrAttachmentFetchFailed indicates the HTTP GET against the attachment URL
// failed (non-2xx status, timeout, transport error). Network-side failure;
// caller may retry.
var ErrAttachmentFetchFailed = xerr.New(
	"ATTACHMENT_FETCH_FAILED",
	xerr.CategoryInternal,
	502,
	"failed to fetch attachment",
)
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add pkg/xcodes/message.go
git commit -m "feat(xcodes): add attachment error codes"
```

---

## Task 6: Service-layer attachment processing helpers

**Goal:** Pure functions: validate, render LINK HTML, fetch MIME bytes. Testable in isolation.

**Files:**
- Create: `internal/service/message/email_attachment.go`
- Create: `internal/service/message/email_attachment_test.go`

- [ ] **Step 1: Write failing test for `validateAttachments`**

Create `internal/service/message/email_attachment_test.go`:

```go
package message

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAttachments_emptyOK(t *testing.T) {
	require.NoError(t, validateAttachments(nil))
}

func TestValidateAttachments_kindRequired(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f.txt", Url: "https://x.com/f.txt"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind")
}

func TestValidateAttachments_urlRequired(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "f.txt"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url")
}

func TestValidateAttachments_filenameRequired(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Url: "https://x.com/f.txt"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "filename")
}

func TestValidateAttachments_validPasses(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "f.txt", Url: "https://x.com/f.txt"},
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_MIME, Filename: "g.txt", Url: "https://x.com/g.txt", Inline: true},
	})
	require.NoError(t, err)
}

func TestRenderLinkAttachmentAnchor(t *testing.T) {
	html := renderLinkHTML("", []*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "report.pdf", Url: "https://x.com/r.pdf", MimeType: "application/pdf", SizeBytes: 1024},
	})
	assert.Contains(t, html, "<a href=\"https://x.com/r.pdf\"")
	assert.Contains(t, html, "download=\"report.pdf\"")
}

func TestRenderLinkAttachmentInlineImage(t *testing.T) {
	html := renderLinkHTML("", []*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "logo.png", Url: "https://x.com/logo.png", Inline: true, MimeType: "image/png"},
	})
	assert.Contains(t, html, "<img src=\"https://x.com/logo.png\"")
}

func TestRenderLinkAttachmentAppendsToExistingBody(t *testing.T) {
	html := renderLinkHTML("<p>existing</p>", []*pb.EmailAttachment{
		{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "r.pdf", Url: "https://x.com/r.pdf"},
	})
	assert.True(t, strings.HasPrefix(html, "<p>existing</p>"))
	assert.Contains(t, html, "<a href=\"https://x.com/r.pdf\"")
}

func TestFetchAttachmentBytes_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello bytes")
	}))
	defer srv.Close()

	svc := &Service{httpClient: http.DefaultClient, attachmentMaxBytes: 1024}
	bytes, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello bytes"), bytes)
}

func TestFetchAttachmentBytes_exceedsMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()

	svc := &Service{httpClient: http.DefaultClient, attachmentMaxBytes: 10}
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size limit")
}

func TestFetchAttachmentBytes_httpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	svc := &Service{httpClient: http.DefaultClient, attachmentMaxBytes: 1024}
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/service/message/ -run 'TestValidateAttachments|TestRenderLink|TestFetchAttachment' -v`
Expected: FAIL with compile errors.

- [ ] **Step 3: Implement the helpers**

Create `internal/service/message/email_attachment.go`:

```go
package message

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/pkg/xcodes"
)

// validateAttachments checks kind/filename/url are set on every attachment.
// Returns the first error encountered (fail-fast). Mime-type / size are
// hints and not validated here.
func validateAttachments(atts []*pb.EmailAttachment) error {
	for i, a := range atts {
		if a == nil {
			return fmt.Errorf("attachment[%d]: nil", i)
		}
		if a.GetKind() == pb.AttachmentKind_ATTACHMENT_KIND_UNSPECIFIED {
			return fmt.Errorf("attachment[%d]: kind is required", i)
		}
		if a.GetFilename() == "" {
			return fmt.Errorf("attachment[%d]: filename is required", i)
		}
		if a.GetUrl() == "" {
			return fmt.Errorf("attachment[%d]: url is required", i)
		}
	}
	return nil
}

// linkAttachments filters the input down to LINK-kind attachments only.
func linkAttachments(atts []*pb.EmailAttachment) []*pb.EmailAttachment {
	out := make([]*pb.EmailAttachment, 0, len(atts))
	for _, a := range atts {
		if a.GetKind() == pb.AttachmentKind_ATTACHMENT_KIND_LINK {
			out = append(out, a)
		}
	}
	return out
}

// renderLinkHTML appends LINK-mode attachments into htmlBody. Inline
// attachments with an image MIME type become <img>; everything else becomes
// <a download>. Empty htmlBody is OK — the function synthesizes a wrapping
// <div>.
func renderLinkHTML(htmlBody string, linkAtts []*pb.EmailAttachment) string {
	if len(linkAtts) == 0 {
		return htmlBody
	}
	var b strings.Builder
	b.WriteString(htmlBody)
	b.WriteString(`<div class="attachments">`)
	for _, a := range linkAtts {
		if a.GetInline() && strings.HasPrefix(a.GetMimeType(), "image/") {
			fmt.Fprintf(&b, `<img src="%s" alt="%s">`, a.GetUrl(), a.GetFilename())
			continue
		}
		fmt.Fprintf(&b, `<a href="%s" download="%s">%s</a>`, a.GetUrl(), a.GetFilename(), a.GetFilename())
	}
	b.WriteString(`</div>`)
	return b.String()
}

// fetchAttachmentBytes downloads the URL with a hard size cap. maxBytes == 0
// uses s.attachmentMaxBytes. sizeHint (from request.size_bytes) is checked
// against Content-Length first as an optimization.
func (s *Service) fetchAttachmentBytes(ctx context.Context, url string, sizeHint int64) ([]byte, error) {
	maxBytes := s.attachmentMaxBytes
	if maxBytes <= 0 {
		return nil, fmt.Errorf("attachment size limit not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, xcodes.ErrAttachmentFetchFailed.Wrap(err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, xcodes.ErrAttachmentFetchFailed.Wrap(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, xcodes.ErrAttachmentFetchFailed.Wrapf(nil, "http status %d", resp.StatusCode)
	}
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return nil, xcodes.ErrAttachmentTooLarge.Wrapf(nil, "content-length %d > max %d", resp.ContentLength, maxBytes)
	}
	if sizeHint > 0 && sizeHint > maxBytes {
		return nil, xcodes.ErrAttachmentTooLarge.Wrapf(nil, "size_hint %d > max %d", sizeHint, maxBytes)
	}

	limited := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, xcodes.ErrAttachmentFetchFailed.Wrap(err)
	}
	if int64(len(body)) > maxBytes {
		return nil, xcodes.ErrAttachmentTooLarge.New(fmt.Sprintf("body exceeded max %d", maxBytes))
	}
	return body, nil
}

// processAttachments splits request attachments into (rendered HTML, MIME
// attachments). MIME attachments are downloaded eagerly — any fetch failure
// aborts the whole send (no partial-send semantics).
func (s *Service) processAttachments(ctx context.Context, htmlBody string, atts []*pb.EmailAttachment) (string, []*email.Attachment, error) {
	if len(atts) == 0 {
		return htmlBody, nil, nil
	}
	rendered := renderLinkHTML(htmlBody, linkAttachments(atts))

	var mimeAtts []*email.Attachment
	for i, a := range atts {
		if a.GetKind() != pb.AttachmentKind_ATTACHMENT_KIND_MIME {
			continue
		}
		bytes, err := s.fetchAttachmentBytes(ctx, a.GetUrl(), a.GetSizeBytes())
		if err != nil {
			return "", nil, xcodes.ErrAttachmentFetchFailed.Wrapf(err, "attachment[%d] %s", i, a.GetFilename())
		}
		mimeAtts = append(mimeAtts, &email.Attachment{
			Filename:  a.GetFilename(),
			Content:   bytes,
			MimeType:  a.GetMimeType(),
			Inline:    a.GetInline(),
			ContentID: a.GetFilename(), // simple CID = filename; service layer owns naming
		})
	}
	return rendered, mimeAtts, nil
}
```

Note: the test for `processAttachments` is exercised via the integration test in Task 8; here we only test the pure helpers.

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/service/message/ -run 'TestValidateAttachments|TestRenderLink|TestFetchAttachment' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/email_attachment.go internal/service/message/email_attachment_test.go
git commit -m "feat(service/message): add attachment processing helpers"
```

---

## Task 7: Wire http client + size limit into Service

**Goal:** `message.Service` gains an `httpClient` and `attachmentMaxBytes`. Constructed in `service.New` from config.

**Files:**
- Modify: `internal/service/message/service.go`
- Modify: `internal/service/service.go`
- Modify: `pkg/config/config.go`

- [ ] **Step 1: Add `AttachmentConfig`**

In `pkg/config/config.go`, add (place near `PersistenceConfig`):

```go
// AttachmentConfig controls MIME-mode attachment fetching. Defaults apply
// when nil — see service.New.
type AttachmentConfig struct {
	FetchTimeoutSeconds int   `mapstructure:"fetch_timeout_seconds"`
	MaxBytes            int64 `mapstructure:"max_bytes"`
}

// FetchTimeout returns the configured fetch timeout, defaulting to 30s.
func (a *AttachmentConfig) FetchTimeout() time.Duration {
	if a == nil || a.FetchTimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(a.FetchTimeoutSeconds) * time.Second
}

// MaxBytesOr returns the configured byte cap, defaulting to 10MB.
func (a *AttachmentConfig) MaxBytesOr() int64 {
	if a == nil || a.MaxBytes <= 0 {
		return 10 * 1024 * 1024
	}
	return a.MaxBytes
}
```

Make sure `time` is imported.

Add field to the top-level `Config` struct (around line 35-45):

```go
	Attachment *AttachmentConfig
```

- [ ] **Step 2: Add fields to `message.Service` + `New` signature**

In `internal/service/message/service.go`:

```go
import (
	"net/http"
	// ... existing imports
)

type Service struct {
	db            *gorm.DB
	idem          idempotency.Checker
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router

	persistEmailEnabled bool
	persistSMSEnabled   bool

	httpClient          *http.Client
	attachmentMaxBytes  int64
}

func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid thirdcall.GIDService,
	emailRegistry *email.AccountRegistry,
	smsRegistry *sms.AccountRegistry,
	smsRouter *sms.Router,
	cfg ServiceConfig,
	httpClient *http.Client,
	attachmentMaxBytes int64,
) *Service {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if attachmentMaxBytes <= 0 {
		attachmentMaxBytes = 10 * 1024 * 1024
	}
	return &Service{
		db:                  db,
		idem:                idem,
		gid:                 gid,
		emailRegistry:       emailRegistry,
		smsRegistry:         smsRegistry,
		smsRouter:           smsRouter,
		persistEmailEnabled: cfg.PersistEmail,
		persistSMSEnabled:   cfg.PersistSMS,
		httpClient:          httpClient,
		attachmentMaxBytes:  attachmentMaxBytes,
	}
}
```

Make sure `time` is imported.

- [ ] **Step 3: Update callers of `message.New`**

In `internal/service/service.go`:

```go
	httpClient := &http.Client{Timeout: cfg.Attachment.FetchTimeout()}
	attachmentMaxBytes := cfg.Attachment.MaxBytesOr()

	svc := &Service{
		// ... existing fields
		message: message.New(db, idemChecker, gid, emailRegistry, smsRegistry, smsRouter, message.ServiceConfig{
			PersistEmail: emailEnabled,
			PersistSMS:   smsEnabled,
		}, httpClient, attachmentMaxBytes),
	}
```

- [ ] **Step 4: Update test helpers in `email_test.go` and `sms_test.go`**

Find all calls to `message.New(...)` in test files under `internal/service/message/` and add the two new args (`&http.Client{Timeout: time.Second}, 1024*1024`).

```go
// example - update all message.New call sites in tests
svc := New(db, idem, gid, emailReg, smsReg, nil, ServiceConfig{PersistEmail: true, PersistSMS: true}, &http.Client{Timeout: time.Second}, 1024*1024)
```

- [ ] **Step 5: Verify build + tests pass**

Run: `go build ./... && go test ./internal/service/... -count=1`
Expected: build OK; existing tests still PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/config/config.go internal/service/message/service.go internal/service/service.go internal/service/message/email_test.go internal/service/message/sms_test.go
git commit -m "feat(service): wire http client + attachment size limit"
```

---

## Task 8: Integrate attachments into `SendEmail` flow

**Goal:** Validate → process (LINK render + MIME fetch) → send → persist record + attachments.

**Files:**
- Modify: `internal/service/message/util.go` (`validateSendEmailRequest`)
- Modify: `internal/service/message/email.go` (`SendEmail`, `persistEmailRecord`)

- [ ] **Step 1: Write failing test for LINK-mode send**

In `internal/service/message/email_test.go`, add:

```go
func TestSendEmail_LINKAttachment_RendersIntoBody(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := idempotency.NewRedisChecker(redisx.NewTestClient(t), &idempotency.Config{})
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, nil, nil, ServiceConfig{PersistEmail: true},
		&http.Client{Timeout: time.Second}, 1024*1024)

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "with link",
		Body:     "see link",
		HtmlBody: "<p>base</p>",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "test-svc",
		Attachments: []*pb.EmailAttachment{
			{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "r.pdf", Url: "https://oss.example.com/r.pdf", MimeType: "application/pdf"},
		},
	}
	resp, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Persistence: verify the side-table row was written.
	atts, err := dal.ListEmailRecordAttachments(context.Background(), db, resp.Id)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, "r.pdf", atts[0].Filename)
	assert.Equal(t, "https://oss.example.com/r.pdf", atts[0].URL)
	assert.Equal(t, int32(pb.AttachmentKind_ATTACHMENT_KIND_LINK), atts[0].Kind)
}

func TestSendEmail_MIMEAttachment_FetchFailure(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := idempotency.NewRedisChecker(redisx.NewTestClient(t), &idempotency.Config{})
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, nil, nil, ServiceConfig{PersistEmail: true},
		&http.Client{Timeout: time.Second}, 1024*1024)

	// point at a closed port to force fetch failure
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "with mime",
		Body:     "see attached",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "test-svc",
		Attachments: []*pb.EmailAttachment{
			{Kind: pb.AttachmentKind_ATTACHMENT_KIND_MIME, Filename: "r.pdf", Url: "http://127.0.0.1:1/r.pdf"},
		},
	}
	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentFetchFailed) || errors.Is(err, xcodes.ErrMessageSendFailed))
}
```

Add `"github.com/servekit/message-service/internal/store/dal"` to the imports if not present.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/service/message/ -run TestSendEmail_LINKAttachment_RendersIntoBody -v`
Expected: FAIL (the request currently ignores attachments entirely).

- [ ] **Step 3: Add attachment validation to `validateSendEmailRequest`**

In `internal/service/message/util.go`, append to `validateSendEmailRequest` (before the final `return nil`):

```go
	if err := validateAttachments(req.GetAttachments()); err != nil {
		return fmt.Errorf("attachments: %w", err)
	}
```

- [ ] **Step 4: Wire `processAttachments` into `SendEmail`**

In `internal/service/message/email.go`, in `SendEmail`, BEFORE constructing `msg := &email.Message{...}` (around line 75), insert:

```go
	htmlBody, mimeAtts, err := s.processAttachments(ctx, req.GetHtmlBody(), req.GetAttachments())
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after attachment processing", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, err
	}
```

Replace the `HTMLBody` line in the `msg := &email.Message{...}` block:

```go
		HTMLBody:       htmlBody,
```

Add `Attachments: mimeAtts,` to the same struct literal (after `TemplateParams`):

```go
		TemplateParams: req.GetTemplateParams(),
		Attachments:    mimeAtts,
```

- [ ] **Step 5: Persist attachments in `persistEmailRecord`**

In `internal/service/message/email.go`, in `persistEmailRecord`, replace the trailing `dal.CreateEmailRecord` block with:

```go
	if err := dal.CreateEmailRecord(ctx, s.db, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
		return
	}
	if len(req.GetAttachments()) == 0 {
		return
	}
	attRows := make([]*models.MessageEmailRecordAttachment, 0, len(req.GetAttachments()))
	for _, a := range req.GetAttachments() {
		attRows = append(attRows, &models.MessageEmailRecordAttachment{
			EmailRecordID: id,
			Kind:          int32(a.GetKind()),
			Filename:      a.GetFilename(),
			URL:           a.GetUrl(),
			Inline:        a.GetInline(),
			MimeType:      a.GetMimeType(),
			SizeBytes:     a.GetSizeBytes(),
		})
	}
	if err := dal.CreateEmailRecordAttachments(ctx, s.db, attRows); err != nil {
		slog.Error("persist email attachments", "record_id", id, "error", err)
	}
```

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./internal/service/message/ -run 'TestSendEmail_LINKAttachment|TestSendEmail_MIMEAttachment' -v`
Expected: PASS.

- [ ] **Step 7: Run full service test suite**

Run: `go test ./internal/service/... -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/service/message/util.go internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(service/message): wire attachments into SendEmail"
```

---

## Task 9: Populate attachments in `EmailRecord` query responses

**Goal:** `GetEmail` / `ListEmails` / `ListEmailsByCursor` return attachment metadata.

**Files:**
- Modify: `internal/service/message/email.go` (`toProtoEmailRecord`)

- [ ] **Step 1: Write failing test**

Append to `internal/service/message/email_test.go`:

```go
func TestGetEmail_returnsAttachments(t *testing.T) {
	db := setupEmailTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.MessageEmailRecordAttachment{}))
	idem := idempotency.NewRedisChecker(redisx.NewTestClient(t), &idempotency.Config{})
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, nil, nil, ServiceConfig{PersistEmail: true},
		&http.Client{Timeout: time.Second}, 1024*1024)

	sendReq := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Kind: pb.AttachmentKind_ATTACHMENT_KIND_LINK, Filename: "a.txt", Url: "https://x.com/a.txt"},
			{Kind: pb.AttachmentKind_ATTACHMENT_KIND_MIME, Filename: "b.txt", Url: "https://x.com/b.txt"},
		},
	}
	resp, err := svc.SendEmail(context.Background(), sendReq)
	require.NoError(t, err)

	got, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.NoError(t, err)
	require.Len(t, got.GetAttachments(), 2)
	assert.Equal(t, "a.txt", got.GetAttachments()[0].GetFilename())
	assert.Equal(t, "b.txt", got.GetAttachments()[1].GetFilename())
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/service/message/ -run TestGetEmail_returnsAttachments -v`
Expected: FAIL (attachments empty in response).

- [ ] **Step 3: Update `toProtoEmailRecord` to load attachments**

In `internal/service/message/email.go`, change `toProtoEmailRecord` from a pure function to a method on `*Service`:

```go
func (s *Service) toProtoEmailRecord(ctx context.Context, r *models.MessageEmailRecord) *pb.EmailRecord {
	rec := &pb.EmailRecord{
		// ... existing fields (unchanged)
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	// Attachments: best-effort load. Errors are logged but do not fail the
	// record lookup — an attachment-table miss should not 500 the whole query.
	atts, err := dal.ListEmailRecordAttachments(ctx, s.db, r.ID)
	if err != nil {
		slog.Error("load email attachments", "record_id", r.ID, "error", err)
	} else {
		rec.Attachments = make([]*pb.EmailAttachment, 0, len(atts))
		for _, a := range atts {
			rec.Attachments = append(rec.Attachments, &pb.EmailAttachment{
				Kind:      pb.AttachmentKind(a.Kind),
				Filename:  a.Filename,
				Url:       a.URL,
				Inline:    a.Inline,
				MimeType:  a.MimeType,
				SizeBytes: a.SizeBytes,
			})
		}
	}
	return rec
}
```

- [ ] **Step 4: Update all callers of `toProtoEmailRecord`**

Find every `toProtoEmailRecord(...)` call site in `internal/service/message/email.go` (there are 4: GetEmail, ListEmails, ListEmailsByCursor — and update each to use `s.toProtoEmailRecord(ctx, ...)`).

In `GetEmail`:
```go
return s.toProtoEmailRecord(ctx, record), nil
```

In `ListEmails`:
```go
protoRecords[i] = s.toProtoEmailRecord(ctx, r)
```

In `ListEmailsByCursor`:
```go
protoRecords[i] = s.toProtoEmailRecord(ctx, r)
```

- [ ] **Step 5: Run tests to verify pass**

Run: `go test ./internal/service/message/ -run TestGetEmail_returnsAttachments -v`
Expected: PASS.

- [ ] **Step 6: Run full service test suite**

Run: `go test ./internal/service/... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(service/message): populate attachments in EmailRecord"
```

---

## Task 10: Update local + docker configs, end-to-end smoke test

**Goal:** Make the new config knobs available in `config.yaml` / `config.docker.yaml`. Run a full build + test + lint pass.

**Files:**
- Modify: `config.yaml`
- Modify: `config.docker.yaml` (if present)

- [ ] **Step 1: Add attachment config to `config.yaml`**

Append (top-level, alongside `persistence`):

```yaml
attachment:
  fetch_timeout_seconds: 30
  max_bytes: 10485760  # 10 MB
```

- [ ] **Step 2: Same for `config.docker.yaml`** (if it exists)

```yaml
attachment:
  fetch_timeout_seconds: 30
  max_bytes: 10485760
```

- [ ] **Step 3: Run fmt + lint + vet**

Run: `make fmt vet lint`
Expected: no errors.

- [ ] **Step 4: Run full test suite**

Run: `make test`
Expected: all PASS.

- [ ] **Step 5: Run migration locally to verify table created**

Run: `make migrate`
Expected: `message_email_record_attachments` table created (check via `\dt` in psql or a quick SELECT).

- [ ] **Step 6: Commit**

```bash
git add config.yaml config.docker.yaml
git commit -m "chore(config): add attachment fetch + size limit"
```

---

## Self-Review Checklist

- **Spec coverage:**
  - LINK mode renders `<a>`/`<img>` → Task 6 `renderLinkHTML` + Task 8 wiring ✓
  - MIME mode downloads + attaches → Task 6 `fetchAttachmentBytes` + Task 8 wiring ✓
  - `AttachmentKind` enum on proto → Task 1 ✓
  - Side table stores metadata, no bytes → Task 3 ✓
  - URL always persisted for audit → Task 3 model + Task 8 persistence ✓
  - EmailRecord response includes attachments → Task 9 ✓
  - Validation (kind/filename/url required) → Task 6 `validateAttachments` + Task 8 wiring ✓
  - Fetch failures return typed xcodes → Task 5 + Task 6 ✓
  - Size limit enforced → Task 6 `fetchAttachmentBytes` ✓

- **Type consistency:**
  - `provider/email.Attachment` field names match Task 2 + Task 6 ✓
  - `MessageEmailRecordAttachment` field names match Task 3 + Task 4 + Task 8 ✓
  - `Service` new fields match Task 6 (httpClient, attachmentMaxBytes) + Task 7 wiring ✓
  - `processAttachments` signature matches call site in Task 8 ✓
  - `toProtoEmailRecord` change from function to method propagated to all call sites ✓

- **Placeholder scan:** none found.
