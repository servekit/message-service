package config

import (
	"github.com/servekit/go-common/configx"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/message-service/internal/provider/email"
)

func writeTestConfig(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0644))
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func setenv(t *testing.T, key, value string) {
	t.Helper()
	orig := os.Getenv(key)
	require.NoError(t, os.Setenv(key, value))
	t.Cleanup(func() { _ = os.Setenv(key, orig) })
}

// TestLoad_ExpandEnv_VerifiesDockerConfigShape loads a config.docker.yaml-
// shaped file (all ${VAR} references) and verifies every field resolves.
func TestLoad_ExpandEnv_VerifiesDockerConfigShape(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ${TEST_MS_GRPC_ADDR}
  http_addr: ${TEST_MS_HTTP_ADDR}
database:
  driver: ${TEST_MS_DB_DRIVER}
  postgres:
    host: ${TEST_MS_DB_HOST}
    port: ${TEST_MS_DB_PORT}
    user: ${TEST_MS_DB_USER}
    password: ${TEST_MS_DB_PASSWORD}
    dbname: ${TEST_MS_DB_NAME}
    sslmode: ${TEST_MS_DB_SSLMODE}
log:
  level: ${TEST_MS_LOG_LEVEL}
  format: ${TEST_MS_LOG_FORMAT}
cron:
  timezone: ${TEST_MS_CRON_TZ}
  overlap_policy: ${TEST_MS_CRON_OVERLAP}
third_party:
  gid:
    mode: ${TEST_MS_GID_MODE}
    config:
      snowflake:
        machine_id: ${TEST_MS_MACHINE_ID}
        start_time: ${TEST_MS_START_TIME}
`)
	chdir(t, dir)

	setenv(t, "TEST_MS_GRPC_ADDR", ":19092")
	setenv(t, "TEST_MS_HTTP_ADDR", ":18082")
	setenv(t, "TEST_MS_DB_DRIVER", "postgres")
	setenv(t, "TEST_MS_DB_HOST", "postgres")
	setenv(t, "TEST_MS_DB_PORT", "5432")
	setenv(t, "TEST_MS_DB_USER", "postgres")
	setenv(t, "TEST_MS_DB_PASSWORD", "secret")
	setenv(t, "TEST_MS_DB_NAME", "message_service")
	setenv(t, "TEST_MS_DB_SSLMODE", "disable")
	setenv(t, "TEST_MS_LOG_LEVEL", "info")
	setenv(t, "TEST_MS_LOG_FORMAT", "json")
	setenv(t, "TEST_MS_CRON_TZ", "Asia/Shanghai")
	setenv(t, "TEST_MS_CRON_OVERLAP", "skip")
	setenv(t, "TEST_MS_GID_MODE", "module")
	setenv(t, "TEST_MS_MACHINE_ID", "1")
	setenv(t, "TEST_MS_START_TIME", "2026-06-01T00:00:00Z")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":19092", cfg.Server.GRPCAddr)
	require.Equal(t, ":18082", cfg.Server.HTTPAddr)
	require.Equal(t, "postgres", cfg.Database.Postgres.Host)
	require.Equal(t, "secret", cfg.Database.Postgres.Password)
	require.Equal(t, configx.ModeModule, cfg.ThirdParty.GID.Mode)
	require.Equal(t, int64(1), cfg.ThirdParty.GID.Config.Snowflake.MachineID)
}

// TestLoad_NoExpandEnv_KeepsLiteral verifies that without ${VAR} the existing
// default-value style still works (regression guard for local config.yaml).
func TestLoad_NoExpandEnv_KeepsLiteral(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
database:
  driver: postgres
  postgres:
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    dbname: message_service
    sslmode: disable
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":19092", cfg.Server.GRPCAddr)
	require.Equal(t, "localhost", cfg.Database.Postgres.Host)
}

// TestLoad_PersistenceOmitted_DefaultsTrue verifies that a yaml without the
// persistence fields loads with both channels enabled.
func TestLoad_PersistenceOmitted_DefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
database:
  driver: postgres
  postgres:
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    dbname: message_service
    sslmode: disable
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Email.Persistence, "omitted email.persistence must default to true")
	require.True(t, cfg.SMS.Persistence, "omitted sms.persistence must default to true")
}

// TestLoad_PersistenceExplicitFalse verifies that explicit false is loaded
// when set under each domain's block.
func TestLoad_PersistenceExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
database:
  driver: postgres
  postgres:
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    dbname: message_service
    sslmode: disable
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
email:
  persistence: false
sms:
  persistence: true
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Email.Persistence, "explicit email.persistence=false must be honored")
	require.True(t, cfg.SMS.Persistence, "explicit sms.persistence=true must be honored")
}

// TestEmailConfig_IdempotencyTTLDuration verifies the default 5m TTL on nil
// receiver and zero value, plus explicit / unparseable handling.
func TestEmailConfig_IdempotencyTTLDuration(t *testing.T) {
	var nilPtr *EmailConfig
	require.Equal(t, 5*time.Minute, nilPtr.IdempotencyTTLDuration(), "nil receiver must default to 5m")

	var empty EmailConfig
	require.Equal(t, 5*time.Minute, empty.IdempotencyTTLDuration(), "empty IdempotencyTTL must default to 5m")

	explicit := &EmailConfig{IdempotencyTTL: "10m"}
	require.Equal(t, 10*time.Minute, explicit.IdempotencyTTLDuration())

	bad := &EmailConfig{IdempotencyTTL: "not-a-duration"}
	require.Equal(t, 5*time.Minute, bad.IdempotencyTTLDuration(), "unparseable must fall back to 5m")
}

// TestSMSConfig_IdempotencyTTLDuration mirrors TestEmailConfig_IdempotencyTTLDuration.
func TestSMSConfig_IdempotencyTTLDuration(t *testing.T) {
	var nilPtr *SMSConfig
	require.Equal(t, 5*time.Minute, nilPtr.IdempotencyTTLDuration(), "nil receiver must default to 5m")

	var empty SMSConfig
	require.Equal(t, 5*time.Minute, empty.IdempotencyTTLDuration(), "empty IdempotencyTTL must default to 5m")

	explicit := &SMSConfig{IdempotencyTTL: "1h"}
	require.Equal(t, time.Hour, explicit.IdempotencyTTLDuration())

	bad := &SMSConfig{IdempotencyTTL: "not-a-duration"}
	require.Equal(t, 5*time.Minute, bad.IdempotencyTTLDuration(), "unparseable must fall back to 5m")
}

// TestLoad_IdempotencyKeyPrefix_Default verifies the default "msg:idem" is
// applied when the field is omitted, and that explicit values are honored.
func TestLoad_IdempotencyKeyPrefix_Default(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
database:
  driver: postgres
  postgres:
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    dbname: message_service
    sslmode: disable
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "msg:idem", cfg.IdempotencyKeyPrefix, "omitted idempotency_key_prefix must default to msg:idem")

	// Explicit override honored.
	dir2 := t.TempDir()
	writeTestConfig(t, dir2, `
server:
  grpc_addr: ":19092"
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
idempotency_key_prefix: "tenantA:msg:idem"
`)
	chdir(t, dir2)

	cfg2, err := Load()
	require.NoError(t, err)
	require.Equal(t, "tenantA:msg:idem", cfg2.IdempotencyKeyPrefix, "explicit prefix must be honored")
}

// TestLoad_RedisSection loads a yaml with the redis section and verifies it
// lands in cfg.Redis.
func TestLoad_RedisSection(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
database:
  driver: postgres
  postgres:
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    dbname: message_service
    sslmode: disable
redis:
  addr: localhost:6379
  db: 0
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Redis, "Redis section must load")
	require.Equal(t, "localhost:6379", cfg.Redis.Addr)
	require.Equal(t, 0, cfg.Redis.DB)
}

// TestValidate_ModuleMode_NilSnowflake verifies that omitting the snowflake
// section in module mode returns a config error rather than nil-pointer
// panicking. Regression: previously dereferenced gid.Snowflake without a
// nil check, crashing migrate / server startup / module callers.
func TestValidate_ModuleMode_NilSnowflake(t *testing.T) {
	cfg := &Config{
		ThirdParty: &ThirdPartyConfig{
			GID: &RemoteServiceConfig[*gidconfig.Config]{Mode: "module"},
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "snowflake",
		"missing snowflake in module mode must return a config error, not panic")
}

// TestAttachmentConfig_Defaults is a placeholder keeping the suite aware that
// AttachmentConfig now has only inline-cap fields — their defaults come from
// `default:` tags wired by configx at Load time (no XxxOr() helpers). The
// former FetchTimeoutDuration fallbacks were removed together with the
// URL-fetch logic.
func TestAttachmentConfig_Defaults(t *testing.T) {
	cfg := &AttachmentConfig{MaxInlineBytes: 5 * 1024 * 1024}
	require.Equal(t, int64(5*1024*1024), cfg.MaxInlineBytes)
}

// loadEnvFile parses a dotenv file and sets each KEY=VALUE into the test
// environment. Mirrors docker-compose .env semantics: blank lines and lines
// starting with '#' are skipped, inline comments are NOT supported (the value
// is everything after the first '='), and quotes are not stripped.
func loadEnvFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read env file: %s", path)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		require.NotEqual(t, -1, idx, "malformed env line (no '='): %q", line)
		key := strings.TrimSpace(line[:idx])
		require.NotEmpty(t, key, "empty key in env line: %q", line)
		t.Setenv(key, line[idx+1:])
	}
}

// TestExampleConfigsAreLoadable guards that config.example.yaml + .env.example
// stay self-consistent: loads the YAML with every ${VAR} resolved from
// .env.example and asserts Load() + Validate() succeed with real (expanded)
// values. Catches drift in either direction — a ${VAR} in the YAML with no
// matching var in .env.example, or an env value that breaks parsing.
func TestExampleConfigsAreLoadable(t *testing.T) {
	root := filepath.Join("..", "..")
	loadEnvFile(t, filepath.Join(root, ".env.example"))
	t.Setenv("MESSAGE_SERVICE_CONFIG", filepath.Join(root, "config.example.yaml"))

	cfg, err := Load()
	require.NoError(t, err, "config.example.yaml + .env.example must load and validate")

	// Spot-check that ${VAR} was actually expanded, not left literal.
	require.Equal(t, ":19092", cfg.Server.GRPCAddr)
	require.Equal(t, "postgres", cfg.Database.Postgres.Host)
	require.Equal(t, "message_service", cfg.Database.Postgres.DBName)
	require.Equal(t, true, cfg.Email.Persistence)
	require.Equal(t, configx.ModeModule, cfg.ThirdParty.GID.Mode)
	require.NotEqual(t, "${MESSAGE_SERVICE_DATABASE_PASSWORD}", cfg.Database.Postgres.Password,
		"env expansion must have replaced the placeholder")
}

// TestLoad_EmailAccounts_Decode guards that email.accounts decode into the
// embedded email.Config.Accounts. Regression: with a pointer embed,
// mapstructure did not squash, so Email.Config stayed nil and accounts were
// silently dropped — SendEmail then had no accounts at runtime. Requires the
// value embed + ,squash on EmailConfig.
func TestLoad_EmailAccounts_Decode(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":19092"
third_party:
  gid:
    mode: module
    config:
      snowflake:
        machine_id: 1
        start_time: "2026-06-01T00:00:00Z"
email:
  persistence: true
  idempotency_ttl: 5m
  accounts:
    - name: primary
      vendor: aliyun
      host: smtp.example.com
      port: 587
      username: user
      password: pass
      from: noreply@example.com
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)

	// Sibling fields on EmailConfig itself still decode alongside the squashed
	// embedded email.Config (squash must not swallow the outer fields).
	require.True(t, cfg.Email.Persistence, "email.persistence must still decode alongside squashed embed")
	require.Equal(t, "5m", cfg.Email.IdempotencyTTL)

	// The squashed embedded email.Config must hold the decoded account. Pre-fix
	// this was the failure: Email.Config was nil, so accounts were lost.
	require.Len(t, cfg.Email.Accounts, 1, "email.accounts must decode into the embedded email.Config")
	require.Equal(t, &email.AccountConfig{
		Name:     "primary",
		Vendor:   "aliyun",
		Host:     "smtp.example.com",
		Port:     587,
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	}, cfg.Email.Accounts[0])
}
