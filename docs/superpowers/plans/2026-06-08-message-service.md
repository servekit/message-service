# Message Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a message sending microservice (SMS + email) backed by `go-common/message`, with send record persistence, multi-dimensional queries, and stats.

**Architecture:** gRPC service with three consumption modes (Server/Module/Client). Service layer delegates to per-channel services (EmailService, SMSService) backed by `go-common/message` senders, and a QueryService for record retrieval and stats. All records stored in a single `message_records` table. Hook mechanism updates records after each send.

**Tech Stack:** Go 1.26, gRPC + grpc-gateway, GORM + PostgreSQL, go-common/message, protovalidate, snowflake IDs

---

## File Structure

| File | Purpose |
|------|---------|
| `go.mod` | Module definition with go-common replace directive |
| `config.yaml` | Default config template |
| `Makefile` | Build/test/lint/generate targets |
| `api/proto/message/v1/message.proto` | Proto service + message definitions |
| `buf.yaml` | Buf config at repo root |
| `buf.gen.yaml` | Buf generation config |
| `internal/config/config.go` | Config struct + Viper loading |
| `internal/xcodes/xcodes.go` | Common error codes re-export |
| `internal/xcodes/message.go` | Message domain error codes |
| `internal/store/models/base.go` | GORM base model structs |
| `internal/store/models/message_record.go` | MessageRecord model |
| `internal/store/models/genconfig.go` | gorm.io/cli generation config |
| `internal/store/generated/*.go` | Generated field accessors (by `gorm gen`) |
| `internal/store/repository/base.go` | BaseRepo |
| `internal/store/repository/message_repository.go` | CRUD + queries + stats |
| `internal/email/email_service.go` | Email sending via go-common/message |
| `internal/sms/sms_service.go` | SMS sending via go-common/message |
| `internal/query/query_service.go` | Record queries and stats |
| `internal/service/message_service.go` | Thin gRPC dispatcher |
| `internal/middleware/interceptors.go` | Error interceptor |
| `pkg/server.go` | NewServer — standalone microservice |
| `pkg/module.go` | NewModule — in-process use |
| `pkg/client.go` | NewClient — gRPC client |
| `pkg/ptr/ptr.go` | Generic pointer utilities |
| `cmd/server/main.go` | Entry point |
| `migrations/000001_init.up.sql` | Create message_records table |
| `migrations/000001_init.down.sql` | Drop message_records table |

---

### Task 1: Project Scaffolding

**Files:**
- Create: `go.mod`
- Create: `config.yaml`
- Create: `Makefile`
- Create: `buf.yaml`
- Create: `buf.gen.yaml`

- [ ] **Step 1: Create go.mod**

```go
module message-service

go 1.26.1

// No dependencies yet — will be added as we implement
```

Also add the go-common replace directive:

```
replace github.com/servekit/go-common => ../go-common
```

- [ ] **Step 2: Create config.yaml template**

```yaml
server:
  grpc:
    addr: ":9000"
  gateway:
    addr: ":8080"

database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: message_service
  sslmode: disable

log:
  level: info
  format: json

email:
  providers:
    - type: smtp
      host: smtp.example.com
      port: 587
      username: ""
      password: ""
      from: noreply@example.com

sms:
  default_country: CN
  providers:
    - type: aliyun
      access_key_id: ""
      access_key_secret: ""
      sign_name: ""
      region_id: cn-hangzhou
```

- [ ] **Step 3: Create Makefile**

```makefile
.PHONY: all build test lint generate fmt vet tidy run proto

## build: Build the server binary
build:
	go build -o bin/server ./cmd/server/

## run: Run the server locally
run:
	go run ./cmd/server/

## test: Run tests with race detector
test:
	go test -race -coverprofile=coverage.out ./...

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## fmt: Format code
fmt:
	gofmt -w .
	goimports -w .

## vet: Run go vet
vet:
	go vet ./...

## generate: Run gorm.io/cli code generation
generate:
	gorm gen

## proto: Generate protobuf code with buf
proto:
	buf generate

## tidy: Run go mod tidy
tidy:
	go mod tidy

## all: Format, vet, lint, test
all: fmt vet lint test
```

- [ ] **Step 4: Create buf.yaml and buf.gen.yaml**

Copy buf configuration from user-service. Check user-service for the exact buf.yaml and buf.gen.yaml files and adapt the module path to `message-service`.

- [ ] **Step 5: Commit**

```bash
git add go.mod config.yaml Makefile buf.yaml buf.gen.yaml
git commit -m "feat: project scaffolding with go.mod, config, Makefile, buf config"
```

---

### Task 2: Proto Definition

**Files:**
- Create: `api/proto/message/v1/message.proto`

- [ ] **Step 1: Write proto file**

```protobuf
syntax = "proto3";

package message.v1;

option go_package = "message-service/gen/message/v1";

import "buf/validate/validate.proto";

service MessageService {
  // Send an email
  rpc SendEmail(SendEmailRequest) returns (SendResponse);
  // Send an SMS
  rpc SendSMS(SendSMSRequest) returns (SendResponse);
  // Get a single message record
  rpc GetMessage(GetMessageRequest) returns (MessageRecord);
  // List message records with filters
  rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse);
  // Get message statistics
  rpc GetMessageStats(GetMessageStatsRequest) returns (MessageStatsResponse);
}

message SendEmailRequest {
  string to = 1 [(buf.validate.field).string.email = true];
  repeated string cc = 2;
  repeated string bcc = 3;
  string subject = 4 [(buf.validate.field).string.min_len = 1];
  string body = 5;
  string html_body = 6;
  string reply_to = 7;
}

message SendSMSRequest {
  string to = 1 [(buf.validate.field).string.min_len = 1];
  string content = 2;
  string template_id = 3;
  map<string, string> template_params = 4;
}

message SendResponse {
  int64 id = 1;
  string status = 2;
  string provider = 3;
}

message GetMessageRequest {
  int64 id = 1 [(buf.validate.field).int64.gt = 0];
}

message MessageRecord {
  int64 id = 1;
  string channel = 2;
  string provider = 3;
  string status = 4;
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
}

message ListMessagesRequest {
  string channel = 1;
  string status = 2;
  string target = 3;
  string provider = 4;
  int64 start_time = 5;
  int64 end_time = 6;
  int32 page = 7;
  int32 page_size = 8;
}

message ListMessagesResponse {
  repeated MessageRecord records = 1;
  int32 total = 2;
}

message GetMessageStatsRequest {
  string channel = 1;
  string provider = 2;
  int64 start_time = 3;
  int64 end_time = 4;
}

message MessageStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated ProviderStats provider_stats = 5;
}

message ProviderStats {
  string provider = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}
```

- [ ] **Step 2: Run buf generate**

Run: `buf generate`

Expected: Generated Go files in `gen/message/v1/`. Fix any proto errors if generation fails.

- [ ] **Step 3: Run go mod tidy**

Run: `GOPROXY=https://goproxy.cn,direct go mod tidy`

- [ ] **Step 4: Commit**

```bash
git add api/ gen/ go.mod go.sum
git commit -m "feat: proto definition and generated code"
```

---

### Task 3: Config Package

**Files:**
- Create: `internal/config/config.go`

- [ ] **Step 1: Write config.go**

Follow user-service's `internal/config/config.go` pattern. Config struct:

```go
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)

// Config holds all service configuration.
type Config struct {
	Server  *ServerConfig  `mapstructure:"server"`
	Database *dbx.Config   `mapstructure:"database"`
	Log     *logging.Config `mapstructure:"log"`
	Email   *EmailConfig   `mapstructure:"email"`
	SMS     *SMSConfig     `mapstructure:"sms"`
}

// ServerConfig holds gRPC and gateway listener addresses.
type ServerConfig struct {
	GRPC    *ListenConfig `mapstructure:"grpc"`
	Gateway *ListenConfig `mapstructure:"gateway"`
}

// ListenConfig holds a listener address.
type ListenConfig struct {
	Addr string `mapstructure:"addr"`
}

// EmailConfig holds email provider configurations.
type EmailConfig struct {
	Providers []EmailProviderConfig `mapstructure:"providers"`
}

// EmailProviderConfig holds config for a single email provider.
type EmailProviderConfig struct {
	Type     string `mapstructure:"type"`
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	From     string `mapstructure:"from"`
	Domain   string `mapstructure:"domain"`
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
}

// SMSConfig holds SMS provider configurations.
type SMSConfig struct {
	DefaultCountry string               `mapstructure:"default_country"`
	Providers      []SMSProviderConfig  `mapstructure:"providers"`
	Routes         []SMSRouteConfig     `mapstructure:"routes"`
}

// SMSProviderConfig holds config for a single SMS provider.
type SMSProviderConfig struct {
	AccessKeyID     string `mapstructure:"access_key_id"`
	AccessKeySecret string `mapstructure:"access_key_secret"`
	SignName        string `mapstructure:"sign_name"`
	RegionID        string `mapstructure:"region_id"`
}

// SMSRouteConfig holds country-based routing.
type SMSRouteConfig struct {
	Country   string              `mapstructure:"country"`
	Providers []SMSProviderConfig `mapstructure:"providers"`
}

// Load reads and parses configuration.
func Load() (*Config, error) {
	v := viper.New()

	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	if *configPath != "" {
		v.SetConfigFile(*configPath)
	} else if env := os.Getenv("MESSAGE_SERVICE_CONFIG"); env != "" {
		v.SetConfigFile(env)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/message-service")
	}

	v.SetEnvPrefix("MESSAGE_SERVICE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}
```

- [ ] **Step 2: Run go mod tidy**

Run: `GOPROXY=https://goproxy.cn,direct go mod tidy`

- [ ] **Step 3: Commit**

```bash
git add internal/config/ go.mod go.sum
git commit -m "feat: config package with email/SMS provider configs"
```

---

### Task 4: Error Codes

**Files:**
- Create: `internal/xcodes/xcodes.go`
- Create: `internal/xcodes/message.go`
- Create: `internal/xcodes/xcodes_test.go`

- [ ] **Step 1: Write xcodes.go (re-export common codes)**

```go
package xcodes

import xcodes "github.com/servekit/go-common/xerr/xcodes"

var (
	ErrBadRequest       = xcodes.ErrBadRequest
	ErrUnauthorized     = xcodes.ErrUnauthorized
	ErrForbidden        = xcodes.ErrForbidden
	ErrNotFound         = xcodes.ErrNotFound
	ErrConflict         = xcodes.ErrConflict
	ErrTooManyRequests  = xcodes.ErrTooManyRequests
	ErrInternal         = xcodes.ErrInternal
	ErrServiceUnavailable = xcodes.ErrServiceUnavailable
)
```

- [ ] **Step 2: Write message.go (domain error codes)**

```go
package xcodes

import "github.com/servekit/go-common/xerr"

var (
	ErrMessageNotFound     = xerr.New("MESSAGE_NOT_FOUND", xerr.CategoryNotFound, 404, "message record not found")
	ErrMessageSendFailed   = xerr.New("MESSAGE_SEND_FAILED", xerr.CategoryInternal, 500, "message send failed")
	ErrChannelNotSupported = xerr.New("CHANNEL_NOT_SUPPORTED", xerr.CategoryBadRequest, 400, "channel not supported")
)
```

- [ ] **Step 3: Write xcodes_test.go**

```go
package xcodes

import (
	"errors"
	"testing"

	"github.com/servekit/go-common/xerr"
)

func TestErrorCodesCreateAndMatch(t *testing.T) {
	tests := []struct {
		name     string
		code     xerr.Code
		reason   string
		httpCode int
	}{
		{"message not found", ErrMessageNotFound, "MESSAGE_NOT_FOUND", 404},
		{"send failed", ErrMessageSendFailed, "MESSAGE_SEND_FAILED", 500},
		{"channel not supported", ErrChannelNotSupported, "CHANNEL_NOT_SUPPORTED", 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.code.Reason() != tt.reason {
				t.Errorf("Reason() = %q, want %q", tt.code.Reason(), tt.reason)
			}
			if tt.code.HTTPCode() != tt.httpCode {
				t.Errorf("HTTPCode() = %d, want %d", tt.code.HTTPCode(), tt.httpCode)
			}
		})
	}
}

func TestErrorCodesWrapAndIs(t *testing.T) {
	err := ErrMessageNotFound.Wrap(errors.New("db error"))
	if !errors.Is(err, ErrMessageNotFound.New()) {
		t.Error("errors.Is should match by reason")
	}
}

func TestReExportedCommonCodes(t *testing.T) {
	if ErrNotFound.Reason() != "NOT_FOUND" {
		t.Errorf("ErrNotFound.Reason() = %q, want NOT_FOUND", ErrNotFound.Reason())
	}
	if ErrInternal.HTTPCode() != 500 {
		t.Errorf("ErrInternal.HTTPCode() = %d, want 500", ErrInternal.HTTPCode())
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/xcodes/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/xcodes/
git commit -m "feat: error code definitions with tests"
```

---

### Task 5: Database Migration

**Files:**
- Create: `migrations/000001_init.up.sql`
- Create: `migrations/000001_init.down.sql`

- [ ] **Step 1: Write up migration**

```sql
CREATE TABLE message_records (
    id              BIGINT PRIMARY KEY,
    channel         VARCHAR(16) NOT NULL,
    provider        VARCHAR(32) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'pending',
    target          VARCHAR(255) NOT NULL,
    subject         TEXT,
    content         TEXT,
    template_id     VARCHAR(64),
    template_params JSONB,
    sender_id       VARCHAR(64),
    error_message   TEXT,
    attempts        INT NOT NULL DEFAULT 1,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_message_records_channel   ON message_records (channel);
CREATE INDEX idx_message_records_status    ON message_records (status);
CREATE INDEX idx_message_records_target    ON message_records (target);
CREATE INDEX idx_message_records_provider  ON message_records (provider);
CREATE INDEX idx_message_records_created   ON message_records (created_at);
```

- [ ] **Step 2: Write down migration**

```sql
DROP TABLE IF EXISTS message_records;
```

- [ ] **Step 3: Commit**

```bash
git add migrations/
git commit -m "feat: initial migration for message_records table"
```

---

### Task 6: GORM Models

**Files:**
- Create: `internal/store/models/base.go`
- Create: `internal/store/models/message_record.go`
- Create: `internal/store/models/genconfig.go`

- [ ] **Step 1: Write base.go**

```go
package models

import (
	"time"

	"gorm.io/gorm"
)

// SnowflakeModel provides common fields for snowflake ID tables (non-auto-increment int64).
type SnowflakeModel struct {
	ID        int64     `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"not null;default:now()"`
	UpdatedAt time.Time `gorm:"not null;default:now()"`
}
```

- [ ] **Step 2: Write message_record.go**

```go
package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

// MessageRecord stores a complete record of every message sent through the service.
type MessageRecord struct {
	ID             int64          `gorm:"primaryKey"`
	Channel        string         `gorm:"size:16;not null;index"`                        // email / sms
	Provider       string         `gorm:"size:32;not null;index"`                        // smtp / mailgun / aliyun
	Status         string         `gorm:"size:16;not null;default:pending;index"`        // pending / sent / failed
	Target         string         `gorm:"size:255;not null;index"`                       // recipient email or phone
	Subject        string                                                                         // email subject
	Content        string                                                                         // message body
	TemplateID     string         `gorm:"size:64;column:template_id"`                    // template ID for SMS
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`             // template parameters
	SenderID       string         `gorm:"size:64;column:sender_id"`                      // sender identity (From address, SMS sign name)
	ErrorMessage   string         `gorm:"column:error_message"`                          // failure reason
	Attempts       int            `gorm:"not null;default:1"`                            // number of provider attempts
	SentAt         sql.NullTime   `gorm:"column:sent_at"`                                // when send succeeded
	CreatedAt      time.Time      `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time      `gorm:"not null;default:now()"`
}

// TableName overrides the default GORM table name.
func (MessageRecord) TableName() string { return "message_records" }

// MapStringString is a JSONB-compatible map for template parameters.
type MapStringString map[string]string

// Scan implements sql.Scanner for JSONB.
func (m *MapStringString) Scan(value any) error {
	if value == nil {
		*m = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to unmarshal JSONB value: %v", value)
	}
	return json.Unmarshal(bytes, m)
}

// Value implements driver.Valuer for JSONB.
func (m MapStringString) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}
```

Note: Add `"database/sql"`, `"encoding/json"`, `"fmt"`, `"database/driver"` imports as needed. The `MapStringString` type needs `database/sql/driver` for the `Value()` method return type.

- [ ] **Step 3: Write genconfig.go**

```go
package models

import (
	"database/sql"

	"gorm.io/cli/gorm/field"
	"gorm.io/cli/gorm/genconfig"
)

var _ = genconfig.Config{
	OutPath: "internal/generated",

	FieldTypeMap: map[any]any{
		sql.NullTime{}:    field.Time{},
		MapStringString{}: field.Field[map[string]string]{},
	},
}
```

- [ ] **Step 4: Run go mod tidy and gorm gen**

Run: `GOPROXY=https://goproxy.cn,direct go mod tidy`
Run: `gorm gen`

Expected: Generated files in `internal/store/generated/`. Verify `message_record.go` is generated with field accessors.

- [ ] **Step 5: Commit**

```bash
git add internal/store/ go.mod go.sum
git commit -m "feat: GORM models and generated field accessors"
```

---

### Task 7: Repository

**Files:**
- Create: `internal/store/repository/base.go`
- Create: `internal/store/repository/message_repository.go`
- Create: `internal/store/repository/message_repository_test.go`

- [ ] **Step 1: Write base.go**

```go
package repository

import "gorm.io/gorm"

// BaseRepo provides shared database access for all repositories.
type BaseRepo struct {
	db *gorm.DB
}

// NewBaseRepo creates a new BaseRepo.
func NewBaseRepo(db *gorm.DB) *BaseRepo {
	return &BaseRepo{db: db}
}

// DB returns the underlying *gorm.DB.
func (b *BaseRepo) DB() *gorm.DB {
	return b.db
}
```

- [ ] **Step 2: Write the failing test for MessageRepository**

```go
package repository

import (
	"context"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"
	"message-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestRepo(t *testing.T) *MessageRepository {
	t.Helper()
	db := dbx.SetupTestDB(t)
	return NewMessageRepository(db)
}

func TestMessageRepository_Create(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()

	record := &models.MessageRecord{
		ID:       time.Now().UnixNano(),
		Channel:  "email",
		Provider: "smtp",
		Status:   "pending",
		Target:   "test@example.com",
		Subject:  "Test",
		Content:  "Hello",
	}

	err := repo.Create(ctx, record)
	require.NoError(t, err)

	found, err := repo.FindByID(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, "email", found.Channel)
	assert.Equal(t, "test@example.com", found.Target)
}

func TestMessageRepository_UpdateStatus(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()

	record := &models.MessageRecord{
		ID:       time.Now().UnixNano(),
		Channel:  "sms",
		Provider: "aliyun",
		Status:   "pending",
		Target:   "13800138000",
	}
	require.NoError(t, repo.Create(ctx, record))

	err := repo.UpdateStatus(ctx, record.ID, "sent", "mailgun", 2, nil)
	require.NoError(t, err)

	found, err := repo.FindByID(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, "sent", found.Status)
	assert.Equal(t, "mailgun", found.Provider)
	assert.Equal(t, 2, found.Attempts)
}

func TestMessageRepository_List(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Create(ctx, &models.MessageRecord{
			ID:       time.Now().UnixNano() + int64(i),
			Channel:  "email",
			Provider: "smtp",
			Status:   "sent",
			Target:   "test@example.com",
		}))
	}

	records, total, err := repo.List(ctx, ListFilter{Channel: "email", Page: 1, PageSize: 3})
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	assert.Len(t, records, 3)
}

func TestMessageRepository_Stats(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, &models.MessageRecord{
		ID:       time.Now().UnixNano(),
		Channel:  "email", Provider: "smtp", Status: "sent", Target: "a@example.com",
	}))
	require.NoError(t, repo.Create(ctx, &models.MessageRecord{
		ID:       time.Now().UnixNano() + 1,
		Channel:  "email", Provider: "smtp", Status: "failed", Target: "b@example.com",
	}))
	require.NoError(t, repo.Create(ctx, &models.MessageRecord{
		ID:       time.Now().UnixNano() + 2,
		Channel:  "sms", Provider: "aliyun", Status: "sent", Target: "13800138000",
	}))

	stats, err := repo.Stats(ctx, StatsFilter{Channel: "email"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Total)
	assert.Equal(t, int64(1), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/store/repository/... -run TestMessageRepository`
Expected: FAIL (MessageRepository, ListFilter, StatsFilter not defined)

- [ ] **Step 4: Write message_repository.go**

```go
package repository

import (
	"context"
	"errors"
	"time"

	"message-service/internal/store/models"
	"message-service/internal/xcodes"

	"gorm.io/gorm"
)

// ListFilter holds parameters for listing message records.
type ListFilter struct {
	Channel   string
	Status    string
	Target    string
	Provider  string
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}

// StatsFilter holds parameters for message statistics.
type StatsFilter struct {
	Channel   string
	Provider  string
	StartTime *time.Time
	EndTime   *time.Time
}

// Stats holds aggregated message statistics.
type Stats struct {
	Total       int64
	Sent        int64
	Failed      int64
	SuccessRate float64
}

// ProviderStats holds statistics for a single provider.
type ProviderStats struct {
	Provider string
	Total    int64
	Sent     int64
	Failed   int64
}

// MessageRepository provides data access for MessageRecord.
type MessageRepository struct {
	*BaseRepo
}

// NewMessageRepository creates a new MessageRepository.
func NewMessageRepository(db *gorm.DB) *MessageRepository {
	return &MessageRepository{BaseRepo: NewBaseRepo(db)}
}

// Create inserts a new message record.
func (r *MessageRepository) Create(ctx context.Context, record *models.MessageRecord) error {
	if err := gorm.G[models.MessageRecord](r.db).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// FindByID returns a message record by ID.
func (r *MessageRepository) FindByID(ctx context.Context, id int64) (*models.MessageRecord, error) {
	record, err := gorm.G[models.MessageRecord](r.db).
		Where(generated.SnowflakeModel.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrMessageNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// UpdateStatus updates the send status and related fields.
func (r *MessageRepository) UpdateStatus(ctx context.Context, id int64, status string, provider string, attempts int, sentAt *time.Time) error {
	updates := map[string]any{
		"status":   status,
		"provider": provider,
		"attempts": attempts,
	}
	if sentAt != nil {
		updates["sent_at"] = *sentAt
	}

	result := r.db.WithContext(ctx).Model(&models.MessageRecord{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return xcodes.ErrInternal.Wrap(result.Error)
	}
	if result.RowsAffected == 0 {
		return xcodes.ErrMessageNotFound.New()
	}
	return nil
}

// UpdateError updates the record with failure information.
func (r *MessageRepository) UpdateError(ctx context.Context, id int64, provider string, attempts int, errMsg string) error {
	result := r.db.WithContext(ctx).Model(&models.MessageRecord{}).Where("id = ?", id).Updates(map[string]any{
		"status":        "failed",
		"provider":      provider,
		"attempts":      attempts,
		"error_message": errMsg,
	})
	if result.Error != nil {
		return xcodes.ErrInternal.Wrap(result.Error)
	}
	if result.RowsAffected == 0 {
		return xcodes.ErrMessageNotFound.New()
	}
	return nil
}

// List returns paginated message records matching the filter.
func (r *MessageRepository) List(ctx context.Context, f ListFilter) ([]*models.MessageRecord, int, error) {
	q := r.db.WithContext(ctx).Model(&models.MessageRecord{})

	if f.Channel != "" {
		q = q.Where("channel = ?", f.Channel)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Target != "" {
		q = q.Where("target = ?", f.Target)
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 20
	}
	offset := int((f.Page - 1) * f.PageSize)

	var records []*models.MessageRecord
	if err := q.Order("created_at DESC").Offset(offset).Limit(int(f.PageSize)).Find(&records).Error; err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	return records, int(total), nil
}

// Stats returns aggregated statistics for messages matching the filter.
func (r *MessageRepository) Stats(ctx context.Context, f StatsFilter) (*Stats, error) {
	q := r.db.WithContext(ctx).Model(&models.MessageRecord{})

	if f.Channel != "" {
		q = q.Where("channel = ?", f.Channel)
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}

	var total, sent, failed int64
	if err := q.Count(&total).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	sentQ := r.db.WithContext(ctx).Model(&models.MessageRecord{}).Where("status = ?", "sent")
	if f.Channel != "" {
		sentQ = sentQ.Where("channel = ?", f.Channel)
	}
	if f.Provider != "" {
		sentQ = sentQ.Where("provider = ?", f.Provider)
	}
	if f.StartTime != nil {
		sentQ = sentQ.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		sentQ = sentQ.Where("created_at <= ?", *f.EndTime)
	}
	if err := sentQ.Count(&sent).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	failed = total - sent

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

// ProviderStats returns statistics grouped by provider.
func (r *MessageRepository) ProviderStats(ctx context.Context, f StatsFilter) ([]ProviderStats, error) {
	q := r.db.WithContext(ctx).Model(&models.MessageRecord{}).

	if f.Channel != "" {
		q = q.Where("channel = ?", f.Channel)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}

	var results []ProviderStats
	rows, err := q.Select("provider, COUNT(*) as total, COUNT(*) FILTER (WHERE status = 'sent') as sent, COUNT(*) FILTER (WHERE status = 'failed') as failed").
		Group("provider").Rows()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ps ProviderStats
		if err := rows.Scan(&ps.Provider, &ps.Total, &ps.Sent, &ps.Failed); err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		results = append(results, ps)
	}
	return results, nil
}
```

Note: The `FindByID` method uses `generated.SnowflakeModel.ID.Eq(id)` — adjust to use the generated field accessor for MessageRecord's ID field (which will be named based on the generated output). If `gorm gen` produces a different struct name, update accordingly.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/store/repository/... -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store/repository/ go.mod go.sum
git commit -m "feat: message repository with CRUD, list, and stats"
```

---

### Task 8: Email Service

**Files:**
- Create: `internal/email/email_service.go`
- Create: `internal/email/email_service_test.go`

- [ ] **Step 1: Write the failing test**

```go
package email

import (
	"context"
	"testing"

	"github.com/servekit/go-common/message/email"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockProvider implements email.Provider for testing.
type mockProvider struct {
	err error
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Send(ctx context.Context, msg *email.Message) error { return m.err }

func TestEmailService_Send_Success(t *testing.T) {
	sender := email.NewSender([]email.Provider{&mockProvider{}})
	svc := NewService(sender, &mockRepo{})

	resp, err := svc.Send(context.Background(), &email.Message{
		To:      "test@example.com",
		Subject: "Test",
		Body:    "Hello",
	})
	require.NoError(t, err)
	assert.Equal(t, "sent", resp.Status)
	assert.Equal(t, "mock", resp.Provider)
	assert.NotZero(t, resp.ID)
}

func TestEmailService_Send_Failed(t *testing.T) {
	sender := email.NewSender([]email.Provider{&mockProvider{err: assert.AnError}})
	svc := NewService(sender, &mockRepo{})

	resp, err := svc.Send(context.Background(), &email.Message{
		To:      "test@example.com",
		Subject: "Test",
		Body:    "Hello",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
}

// mockRepo is a minimal in-memory repository for testing.
type mockRepo struct {
	records map[int64]string
}

func (m *mockRepo) Create(ctx context.Context, id int64, channel, provider, status, target, subject, content, senderID string) error {
	return nil
}
func (m *mockRepo) UpdateStatus(ctx context.Context, id int64, status, provider string, attempts int) error {
	return nil
}
func (m *mockRepo) UpdateError(ctx context.Context, id int64, provider string, attempts int, errMsg string) error {
	return nil
}
```

Note: The `mockRepo` interface should match what `Service` actually needs. Refine after writing the implementation to match the actual repository interface consumed by email.Service.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/email/... -v`
Expected: FAIL (package doesn't exist)

- [ ] **Step 3: Write email_service.go**

The `Service` struct holds:
- `sender *email.Sender` — from go-common/message
- `repo MessageRepository` — interface for record persistence (not the concrete type, to allow mocking)

`Send(ctx, msg)` method:
1. Generate snowflake ID
2. Call `repo.Create(ctx, id, "email", "", "pending", msg.To, msg.Subject, msg.Body, fromAddress)`
3. Call `sender.Send(ctx, msg)` — go-common/message handles provider fallback
4. Return `SendResponse{id, "sent", provider}` on success

Hook registration: When creating the `email.Sender` in the service constructor, pass `email.WithHook(email.HookFunc(...))` that captures `recordID` from context and updates the record via repo.

Context key pattern for passing recordID:

```go
type ctxKeyRecordID struct{}

func withRecordID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxKeyRecordID{}, id)
}

func recordIDFromCtx(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKeyRecordID{}).(int64)
	return id, ok
}
```

Write the full implementation following the pattern from the spec.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/email/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/email/ go.mod go.sum
git commit -m "feat: email service with Hook-based record update"
```

---

### Task 9: SMS Service

**Files:**
- Create: `internal/sms/sms_service.go`
- Create: `internal/sms/sms_service_test.go`

- [ ] **Step 1: Write the failing test**

Same pattern as email service tests, but using `sms.Sender` and `sms.Message`.

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Write sms_service.go**

Same pattern as `email_service.go` but using `sms.Sender` or `sms.Router`. The `Service` struct holds:
- `sender *sms.Sender` (or `router *sms.Router`)
- `repo` — same repository interface

`Send(ctx, msg)` method follows the same flow as email: create record → send → hook updates record.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/sms/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sms/ go.mod go.sum
git commit -m "feat: SMS service with Hook-based record update"
```

---

### Task 10: Query Service

**Files:**
- Create: `internal/query/query_service.go`
- Create: `internal/query/query_service_test.go`

- [ ] **Step 1: Write the failing test**

Test `Get`, `List`, and `Stats` methods using `dbx.SetupTestDB(t)` and a real MessageRepository.

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Write query_service.go**

```go
package query

import (
	"context"

	"message-service/internal/store/models"
	"message-service/internal/store/repository"
	"message-service/internal/xcodes"
)

// Service handles message record queries and statistics.
type Service struct {
	repo *repository.MessageRepository
}

// NewService creates a new query Service.
func NewService(repo *repository.MessageRepository) *Service {
	return &Service{repo: repo}
}

// Get returns a single message record by ID.
func (s *Service) Get(ctx context.Context, id int64) (*models.MessageRecord, error) {
	return s.repo.FindByID(ctx, id)
}

// List returns paginated message records matching the filter.
func (s *Service) List(ctx context.Context, f repository.ListFilter) ([]*models.MessageRecord, int, error) {
	return s.repo.List(ctx, f)
}

// Stats returns aggregated message statistics.
func (s *Service) Stats(ctx context.Context, f repository.StatsFilter) (*repository.Stats, []repository.ProviderStats, error) {
	stats, err := s.repo.Stats(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	provStats, err := s.repo.ProviderStats(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	return stats, provStats, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/query/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/query/ go.mod go.sum
git commit -m "feat: query service for record retrieval and stats"
```

---

### Task 11: Message Service (gRPC Dispatcher)

**Files:**
- Create: `internal/service/message_service.go`

- [ ] **Step 1: Write message_service.go**

This is the thin dispatcher implementing `pb.MessageServiceServer`. Follow the user-service pattern:

```go
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/servekit/go-common/dbx"
	pb "message-service/gen/message/v1"
	"message-service/internal/config"
	"message-service/internal/email"
	"message-service/internal/query"
	"message-service/internal/sms"
	"message-service/internal/store/repository"
	"message-service/internal/xcodes"

	smsemail "github.com/servekit/go-common/message/email"
	smssms "github.com/servekit/go-common/message/sms"
	smssmsaliyun "github.com/servekit/go-common/message/sms/aliyun"
	smsemailmailgun "github.com/servekit/go-common/message/email/mailgun"
	smsemailsmtp "github.com/servekit/go-common/message/email/smtp"

	"gorm.io/gorm"
)

// MessageService implements pb.MessageServiceServer.
type MessageService struct {
	pb.UnimplementedMessageServiceServer

	db     *gorm.DB
	ownDB  bool
	email  *email.Service
	smsSvc *sms.Service
	query  *query.Service
}

// Option configures a MessageService.
type Option func(*options)

type options struct {
	db *gorm.DB
}

// WithDB injects an external database connection.
func WithDB(db *gorm.DB) Option {
	return func(o *options) { o.db = db }
}

// New creates a new MessageService.
func New(cfg *config.Config, opts ...Option) (*MessageService, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	db, ownDB, err := resolveDB(&o, cfg)
	if err != nil {
		return nil, err
	}

	repo := repository.NewMessageRepository(db)
	emailSvc := buildEmailService(cfg, repo)
	smsSvc := buildSMSService(cfg, repo)
	querySvc := query.NewService(repo)

	return &MessageService{
		db:     db,
		ownDB:  ownDB,
		email:  emailSvc,
		smsSvc: smsSvc,
		query:  querySvc,
	}, nil
}

// Close cleans up resources owned by this instance.
func (s *MessageService) Close() {
	if s.ownDB && s.db != nil {
		if sqlDB, err := s.db.DB(); err == nil && sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

// --- gRPC method implementations ---

func (s *MessageService) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	msg := &smsemail.Message{
		To:       req.To,
		Cc:       req.Cc,
		Bcc:      req.Bcc,
		Subject:  req.Subject,
		Body:     req.Body,
		HTMLBody: req.HtmlBody,
		ReplyTo:  req.ReplyTo,
	}
	resp, err := s.email.Send(ctx, msg)
	if err != nil {
		return nil, xcodes.ErrMessageSendFailed.Wrap(err)
	}
	return &pb.SendResponse{
		Id:       resp.ID,
		Status:   resp.Status,
		Provider: resp.Provider,
	}, nil
}

func (s *MessageService) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	msg := &smssms.Message{
		To:       req.To,
		Content:  req.Content,
		Template: req.TemplateId,
		Params:   req.TemplateParams,
	}
	resp, err := s.smsSvc.Send(ctx, msg)
	if err != nil {
		return nil, xcodes.ErrMessageSendFailed.Wrap(err)
	}
	return &pb.SendResponse{
		Id:       resp.ID,
		Status:   resp.Status,
		Provider: resp.Provider,
	}, nil
}

func (s *MessageService) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	record, err := s.query.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return toProtoRecord(record), nil
}

func (s *MessageService) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	filter := repository.ListFilter{
		Channel:  req.Channel,
		Status:   req.Status,
		Target:   req.Target,
		Provider: req.Provider,
		Page:     req.Page,
		PageSize: req.PageSize,
	}
	if req.StartTime > 0 {
		t := time.Unix(req.StartTime, 0)
		filter.StartTime = &t
	}
	if req.EndTime > 0 {
		t := time.Unix(req.EndTime, 0)
		filter.EndTime = &t
	}

	records, total, err := s.query.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	pbRecords := make([]*pb.MessageRecord, len(records))
	for i, r := range records {
		pbRecords[i] = toProtoRecord(r)
	}
	return &pb.ListMessagesResponse{Records: pbRecords, Total: int32(total)}, nil
}

func (s *MessageService) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	filter := repository.StatsFilter{
		Channel:  req.Channel,
		Provider: req.Provider,
	}
	if req.StartTime > 0 {
		t := time.Unix(req.StartTime, 0)
		filter.StartTime = &t
	}
	if req.EndTime > 0 {
		t := time.Unix(req.EndTime, 0)
		filter.EndTime = &t
	}

	stats, provStats, err := s.query.Stats(ctx, filter)
	if err != nil {
		return nil, err
	}

	pbProvStats := make([]*pb.ProviderStats, len(provStats))
	for i, ps := range provStats {
		pbProvStats[i] = &pb.ProviderStats{
			Provider: ps.Provider,
			Total:    ps.Total,
			Sent:     ps.Sent,
			Failed:   ps.Failed,
		}
	}

	return &pb.MessageStatsResponse{
		Total:         stats.Total,
		Sent:          stats.Sent,
		Failed:        stats.Failed,
		SuccessRate:   stats.SuccessRate,
		ProviderStats: pbProvStats,
	}, nil
}

// --- internal helpers ---

func resolveDB(o *options, cfg *config.Config) (db *gorm.DB, own bool, err error) {
	if o.db != nil {
		return o.db, false, nil
	}
	db, err = dbx.New(cfg.Database)
	if err != nil {
		return nil, false, fmt.Errorf("database: %w", err)
	}
	return db, true, nil
}

func buildEmailService(cfg *config.Config, repo *repository.MessageRepository) *email.Service {
	var providers []smsemail.Provider
	for _, p := range cfg.Email.Providers {
		switch p.Type {
		case "smtp":
			if p, err := smsemailsmtp.NewProvider(&smsemailsmtp.Config{
				Host: p.Host, Port: p.Port, Username: p.Username, Password: p.Password, From: p.From,
			}); err == nil {
				providers = append(providers, p)
			}
		case "mailgun":
			providers = append(providers, smsemailmailgun.NewProvider(&smsemailmailgun.Config{
				Domain: p.Domain, APIKey: p.APIKey, From: p.From, Endpoint: p.Endpoint,
			}))
		}
	}
	sender := smsemail.NewSender(providers)
	return email.NewService(sender, repo)
}

func buildSMSService(cfg *config.Config, repo *repository.MessageRepository) *sms.Service {
	var providers []smssms.Provider
	for _, p := range cfg.SMS.Providers {
		if p, err := smssmsaliyun.NewProvider(&smssmsaliyun.Config{
			AccessKeyID: p.AccessKeyID, AccessKeySecret: p.AccessKeySecret,
			SignName: p.SignName, RegionID: p.RegionID,
		}); err == nil {
			providers = append(providers, p)
		}
	}
	sender := smssms.NewSender(providers)
	return sms.NewService(sender, repo)
}

func toProtoRecord(r *models.MessageRecord) *pb.MessageRecord {
	var sentAt int64
	if r.SentAt.Valid {
		sentAt = r.SentAt.Time.Unix()
	}
	var params map[string]string
	if r.TemplateParams != nil {
		params = map[string]string(r.TemplateParams)
	}
	return &pb.MessageRecord{
		Id:             r.ID,
		Channel:        r.Channel,
		Provider:       r.Provider,
		Status:         r.Status,
		Target:         r.Target,
		Subject:        r.Subject,
		Content:        r.Content,
		TemplateId:     r.TemplateID,
		TemplateParams: params,
		SenderId:       r.SenderID,
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		SentAt:         sentAt,
		CreatedAt:      r.CreatedAt.Unix(),
	}
}
```

Note: The `buildEmailService` and `buildSMSService` functions need error handling for provider creation. Adjust variable shadowing (the inner `p` shadows the loop variable — use different names like `prov`).

- [ ] **Step 2: Run go vet**

Run: `go vet ./internal/service/...`

Fix any compilation issues.

- [ ] **Step 3: Commit**

```bash
git add internal/service/ go.mod go.sum
git commit -m "feat: message service gRPC dispatcher"
```

---

### Task 12: Middleware

**Files:**
- Create: `internal/middleware/interceptors.go`

- [ ] **Step 1: Write interceptors.go**

```go
package middleware

import (
	"context"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/xerr"

	"google.golang.org/grpc"
)

// ErrorInterceptor translates xerr errors to proper gRPC status codes.
func ErrorInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		return nil, grpcx.TranslateError(err)
	}
	return resp, nil
}
```

Note: Check if `grpcx.TranslateError` exists in go-common. If not, implement error translation directly using `xerr` → `google.golang.org/grpc/status` mapping, following the same pattern as user-service's error interceptor.

- [ ] **Step 2: Commit**

```bash
git add internal/middleware/ go.mod go.sum
git commit -m "feat: error interceptor for gRPC status translation"
```

---

### Task 13: pkg/ Layer

**Files:**
- Create: `pkg/server.go`
- Create: `pkg/module.go`
- Create: `pkg/client.go`
- Create: `pkg/ptr/ptr.go`

- [ ] **Step 1: Write ptr/ptr.go**

```go
package ptr

// Ref returns a pointer to v.
func Ref[T any](v T) *T {
	return &v
}

// Deref dereferences a pointer, returning the zero value if nil.
func Deref[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}
```

- [ ] **Step 2: Write module.go**

```go
package messageservice

import (
	"message-service/internal/config"
	"message-service/internal/service"
)

// Module provides in-process access to message-service without gRPC overhead.
type Module struct {
	svc *service.MessageService
}

// NewModule initializes all components from config.
func NewModule(cfg *config.Config, opts ...service.Option) (*Module, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Module{svc: svc}, nil
}

// Close cleans up resources owned by this instance.
func (m *Module) Close() {
	m.svc.Close()
}
```

- [ ] **Step 3: Write client.go**

```go
package messageservice

import (
	"fmt"

	pb "message-service/gen/message/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps the generated gRPC client for message-service.
type Client struct {
	conn *grpc.ClientConn
	pb.MessageServiceClient
}

// NewClient creates a new gRPC client.
func NewClient(target string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	return &Client{conn: conn, MessageServiceClient: pb.NewMessageServiceClient(conn)}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
```

- [ ] **Step 4: Write server.go**

```go
package messageservice

import (
	"github.com/servekit/go-common/grpcx"

	pb "message-service/gen/message/v1"
	"message-service/internal/config"
	"message-service/internal/middleware"
	"message-service/internal/service"

	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
)

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []service.Option
}

// WithServiceOptions passes through options to the underlying service.New call.
func WithServiceOptions(opts ...service.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// Server wraps a gRPC server for message-service.
type Server struct {
	grpcSrv *grpcx.Server
	svc     *service.MessageService
}

// NewServer creates a Server with all dependencies.
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}

	svc, err := service.New(cfg, o.serviceOpts...)
	if err != nil {
		return nil, err
	}

	validator, err := protovalidate.New()
	if err != nil {
		return nil, err
	}

	grpcSrv := grpcx.New(
		grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPC.Addr,
			GatewayAddr: cfg.Server.Gateway.Addr,
		},
		func(s *grpc.Server) { pb.RegisterMessageServiceServer(s, svc) },
		pb.RegisterMessageServiceHandlerFromEndpoint,
		middleware.ErrorInterceptor,
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)

	return &Server{grpcSrv: grpcSrv, svc: svc}, nil
}

// Run starts gRPC + HTTP gateway and blocks until shutdown signal.
func (s *Server) Run() { s.grpcSrv.Run() }

// Stop gracefully stops all transports.
func (s *Server) Stop() { s.grpcSrv.Stop() }
```

- [ ] **Step 5: Commit**

```bash
git add pkg/ go.mod go.sum
git commit -m "feat: pkg layer with Server, Module, Client, and ptr utilities"
```

---

### Task 14: Entry Point

**Files:**
- Create: `cmd/server/main.go`

- [ ] **Step 1: Write main.go**

```go
package main

import (
	"log/slog"
	"os"

	"github.com/servekit/go-common/logging"

	"message-service/internal/config"
	messageservice "message-service/pkg"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logging.Setup(cfg.Log)

	srv, err := messageservice.NewServer(cfg)
	if err != nil {
		slog.Error("init server", "error", err)
		os.Exit(1)
	}
	srv.Run()
}
```

- [ ] **Step 2: Run go mod tidy**

Run: `GOPROXY=https://goproxy.cn,direct go mod tidy`

- [ ] **Step 3: Run go vet**

Run: `go vet ./...`

Fix any remaining compilation issues.

- [ ] **Step 4: Commit**

```bash
git add cmd/ go.mod go.sum
git commit -m "feat: server entry point"
```

---

### Task 15: Integration & Final Verification

**Files:**
- Modify: `go.mod` (final tidy)
- Modify: `config.yaml` (if needed)

- [ ] **Step 1: Run go mod tidy**

Run: `GOPROXY=https://goproxy.cn,direct go mod tidy`

- [ ] **Step 2: Run all tests**

Run: `go test -race ./...`
Expected: All tests PASS

- [ ] **Step 3: Run go vet**

Run: `go vet ./...`
Expected: No issues

- [ ] **Step 4: Build binary**

Run: `go build -o bin/server ./cmd/server/`
Expected: Binary builds successfully

- [ ] **Step 5: Final commit**

```bash
git add -A
git commit -m "feat: message-service v1 complete"
```
