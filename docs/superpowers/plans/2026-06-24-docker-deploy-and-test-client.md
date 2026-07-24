# Docker 部署 + 测试客户端实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 message-service 容器化(entrypoint 自动 migrate + compose 编排 postgres)、配置完全 env 驱动(yaml 内 `${VAR}` 展开)、提供覆盖 10 个 RPC 的 gRPC/HTTP 双协议 CLI 测试客户端。

**Architecture:** 跨两个仓库:① `github.com/servekit/go-common` 加 `configx.WithExpandEnv()` option,在 viper `ReadInConfig` 后递归展开 yaml 字符串里的 `${VAR}`;② `message-service` 启用该 option,新增 `config.docker.yaml`(全 `${VAR}` 引用)、`.env.example`、`entrypoint.sh`、`docker-compose.yaml`,改 `Dockerfile` 用 alpine 同时构 server+migrate;新增 `cmd/testclient/` CLI(标准库 flag + 子命令,grpc 复用 `pkg.Client`,http 用 `net/http` 调 grpc-gateway 暴露的 REST 接口)。

**Tech Stack:** Go 1.26、viper、GORM、grpc-gateway、PostgreSQL 17、Docker、alpine 3.20、docker-compose。

**Spec:** `docs/superpowers/specs/2026-06-24-docker-deploy-and-test-client-design.md`

---

## File Structure

### go-common (`/Users/moss/code/base/go-common/`)

| 文件 | 责任 |
|------|------|
| `configx/options.go` | 新增 `WithExpandEnv()` option 和 loader 字段 |
| `configx/configx.go` | Load 中加 expandStringsInMap + 调用点 |
| `configx/expand.go` | 新建,放 `expandStrings` 递归辅助函数(纯函数,便于单测) |
| `configx/expand_test.go` | 新建,覆盖 expandStrings 单测(map/slice/string 三类 + 不修改非 string + 空 ${VAR} 行为) |
| `configx/configx_test.go` | 新增端到端测试 `TestLoad_ExpandEnv` |

### message-service (`/Users/moss/code/base/message-service/`)

| 文件 | 责任 |
|------|------|
| `pkg/config/config.go` | `Load()` 加 `configx.WithExpandEnv()` |
| `pkg/config/config_test.go` | 新建,验证 `${VAR}` 在 message-service Config 上端到端工作 |
| `config.docker.yaml` | 新建,所有字段 `${VAR}` 引用 |
| `.env.example` | 新建,列出所有 `MESSAGE_SERVICE_*` |
| `entrypoint.sh` | 新建,先 migrate 再 exec server |
| `Dockerfile` | 改造:builder 同时构 server+migrate,runtime 换 alpine,COPY config.docker.yaml → config.yaml |
| `.dockerignore` | 新建,排除 bin/、.git、coverage 等 |
| `docker-compose.yaml` | 新建,postgres + message-service 两个 service |
| `.gitignore` | 加 `.env` |
| `Makefile` | 加 `build-client` target |
| `cmd/testclient/main.go` | 入口、全局 flag、子命令 dispatch |
| `cmd/testclient/client.go` | `Caller` 接口(10 个方法) |
| `cmd/testclient/grpc_client.go` | gRPC 实现,复用 `messageservice.Client` |
| `cmd/testclient/http_client.go` | HTTP 实现,`net/http` + `encoding/json` |
| `cmd/testclient/email.go` | 5 个 email 子命令 |
| `cmd/testclient/sms.go` | 5 个 sms 子命令 |
| `cmd/testclient/smoke.go` | smoke-test 子命令 |

---

# Phase 1: go-common/configx 加 `${VAR}` 展开能力

## Task 1: 加 loader 字段 + `WithExpandEnv()` option

**项目:** go-common
**Files:**
- Modify: `/Users/moss/code/base/go-common/configx/options.go`
- Modify: `/Users/moss/code/base/go-common/configx/configx.go` (loader struct 定义处)

- [ ] **Step 1: 在 loader struct 加 expandEnv 字段**

修改 `/Users/moss/code/base/go-common/configx/configx.go` 第 117-124 行的 loader struct:

```go
// loader holds the resolved options for a single Load call.
type loader struct {
	serviceName string
	envPrefix   string
	configName  string
	configPaths []string
	decodeHooks []mapstructure.DecodeHookFunc
	expandEnv   bool
}
```

- [ ] **Step 2: 在 options.go 加 WithExpandEnv option**

在 `/Users/moss/code/base/go-common/configx/options.go` 末尾追加:

```go
// WithExpandEnv enables ${VAR} expansion in config file values.
// After ReadInConfig, all string values in viper.AllSettings() are passed
// through os.ExpandEnv. Useful for env-driven deployments where the YAML
// references environment variables by name (e.g. host: ${DB_HOST}).
//
// Unset variables expand to empty string (os.ExpandEnv semantics).
// Non-string values are not touched.
func WithExpandEnv() LoadOption {
	return func(l *loader) {
		l.expandEnv = true
	}
}
```

- [ ] **Step 3: 编译验证**

Run: `cd /Users/moss/code/base/go-common && go build ./configx/...`
Expected: 无输出(成功)

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/go-common
git add configx/options.go configx/configx.go
git commit -m "feat(configx): add WithExpandEnv option and loader field"
```

---

## Task 2: 写 expandStrings 递归辅助函数 + 单测(TDD)

**项目:** go-common
**Files:**
- Create: `/Users/moss/code/base/go-common/configx/expand.go`
- Create: `/Users/moss/code/base/go-common/configx/expand_test.go`

- [ ] **Step 1: 写失败测试**

创建 `/Users/moss/code/base/go-common/configx/expand_test.go`:

```go
package configx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandStrings_String(t *testing.T) {
	t.Setenv("EXPAND_TEST_HOST", "db.example.com")
	in := map[string]any{"host": "${EXPAND_TEST_HOST}"}
	expandStrings(in)
	require.Equal(t, "db.example.com", in["host"])
}

func TestExpandStrings_NestedMap(t *testing.T) {
	t.Setenv("EXPAND_TEST_PORT", "5432")
	in := map[string]any{
		"db": map[string]any{
			"port": "${EXPAND_TEST_PORT}",
		},
	}
	expandStrings(in)
	db := in["db"].(map[string]any)
	require.Equal(t, "5432", db["port"])
}

func TestExpandStrings_Slice(t *testing.T) {
	t.Setenv("EXPAND_TEST_H1", "a.com")
	in := map[string]any{
		"hosts": []any{"${EXPAND_TEST_H1}", "b.com"},
	}
	expandStrings(in)
	hosts := in["hosts"].([]any)
	require.Equal(t, "a.com", hosts[0])
	require.Equal(t, "b.com", hosts[1])
}

func TestExpandStrings_UnsetVariableBecomesEmpty(t *testing.T) {
	// os.ExpandEnv on unset var yields empty string (not the original ${VAR}).
	in := map[string]any{"k": "${DEFINITELY_UNSET_VAR_XYZ}"}
	expandStrings(in)
	require.Equal(t, "", in["k"])
}

func TestExpandStrings_NonStringUntouched(t *testing.T) {
	in := map[string]any{
		"port":   5432,
		"flag":   true,
		"nested": map[string]any{"n": 42},
		"arr":    []any{1, 2, 3},
	}
	expandStrings(in)
	require.Equal(t, 5432, in["port"])
	require.Equal(t, true, in["flag"])
	require.Equal(t, 42, in["nested"].(map[string]any)["n"])
	require.Equal(t, []any{1, 2, 3}, in["arr"])
}

func TestExpandStrings_StringWithoutVarUntouched(t *testing.T) {
	in := map[string]any{"k": "plain-value"}
	expandStrings(in)
	require.Equal(t, "plain-value", in["k"])
}

func TestExpandStrings_PartialExpansion(t *testing.T) {
	t.Setenv("EXPAND_TEST_USER", "admin")
	in := map[string]any{"dsn": "postgres://${EXPAND_TEST_USER}@db"}
	expandStrings(in)
	require.Equal(t, "postgres://admin@db", in["dsn"])
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/moss/code/base/go-common && go test ./configx/ -run TestExpandStrings -v`
Expected: 编译失败,`undefined: expandStrings`

- [ ] **Step 3: 写 expandStrings 实现**

创建 `/Users/moss/code/base/go-common/configx/expand.go`:

```go
package configx

import "os"

// expandStrings walks m recursively and applies os.ExpandEnv to every string
// value. Nested map[string]any and []any are walked; non-string values are
// left untouched.
//
// unset variables expand to empty string (os.ExpandEnv semantics).
func expandStrings(m map[string]any) {
	for k, v := range m {
		m[k] = expandValue(v)
	}
}

func expandValue(v any) any {
	switch val := v.(type) {
	case string:
		return os.ExpandEnv(val)
	case map[string]any:
		expandStrings(val)
		return val
	case []any:
		for i, item := range val {
			val[i] = expandValue(item)
		}
		return val
	default:
		return v
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/moss/code/base/go-common && go test ./configx/ -run TestExpandStrings -v`
Expected: 7 个测试全部 PASS

- [ ] **Step 5: Commit**

```bash
cd /Users/moss/code/base/go-common
git add configx/expand.go configx/expand_test.go
git commit -m "feat(configx): add expandStrings recursive helper with tests"
```

---

## Task 3: 在 Load 函数里 wire ExpandEnv + 端到端测试

**项目:** go-common
**Files:**
- Modify: `/Users/moss/code/base/go-common/configx/configx.go` (Load 函数)
- Modify: `/Users/moss/code/base/go-common/configx/configx_test.go` (新增 TestLoad_ExpandEnv)

- [ ] **Step 1: 写失败测试**

在 `/Users/moss/code/base/go-common/configx/configx_test.go` 末尾追加:

```go
func TestLoad_ExpandEnv(t *testing.T) {
	dir := tempDir(t)
	writeTempConfig(t, dir, "config.yaml", `
server:
  grpc_addr: "${TEST_EXPAND_GRPC}"
log:
  level: "${TEST_EXPAND_LEVEL}"
  format: text
`)
	chdir(t, dir)
	setenv(t, "TEST_EXPAND_GRPC", ":9999")
	setenv(t, "TEST_EXPAND_LEVEL", "debug")

	var cfg FullConfig
	require.NoError(t, Load(&cfg, WithExpandEnv()))
	require.Equal(t, ":9999", cfg.Server.GRPCAddr)
	require.Equal(t, "debug", cfg.Log.Level)
}

func TestLoad_ExpandEnv_DisabledByDefault(t *testing.T) {
	dir := tempDir(t)
	writeTempConfig(t, dir, "config.yaml", `
server:
  grpc_addr: "${TEST_EXPAND_OFF}"
`)
	chdir(t, dir)
	setenv(t, "TEST_EXPAND_OFF", ":1111")

	var cfg FullConfig
	require.NoError(t, Load(&cfg))
	// Without WithExpandEnv, ${VAR} stays literal (then AutomaticEnv kicks in
	// for the env key TEST_EXPAND_OFF if it matches a field path — but here
	// the field path is server.grpc_addr, so the literal ${...} survives).
	require.Equal(t, "${TEST_EXPAND_OFF}", cfg.Server.GRPCAddr)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/moss/code/base/go-common && go test ./configx/ -run TestLoad_ExpandEnv -v`
Expected: `TestLoad_ExpandEnv` FAIL(expand 未生效,GRPCAddr 是 "${TEST_EXPAND_GRPC}"),`TestLoad_ExpandEnv_DisabledByDefault` 可能 PASS

- [ ] **Step 3: 在 Load 里 wire ExpandEnv**

修改 `/Users/moss/code/base/go-common/configx/configx.go` 的 Load 函数,在 `v.ReadInConfig()` 后、`v.Unmarshal(...)` 前插入(大约第 188 行后):

```go
	// Read config file.
	if err := v.ReadInConfig(); err != nil {
		return ErrReadConfig.Wrap(err)
	}

	// Expand ${VAR} in string values if enabled.
	if l.expandEnv {
		settings := v.AllSettings()
		expandStrings(settings)
		if err := v.MergeConfigMap(settings); err != nil {
			return ErrReadConfig.Wrap(err)
		}
	}

	// Unmarshal with decode hooks and tagless matching.
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/moss/code/base/go-common && go test ./configx/ -v`
Expected: 所有测试 PASS(包括原有的 + 新增的 8 个 ExpandEnv 测试)

- [ ] **Step 5: 跑整个 configx 包的测试做回归**

Run: `cd /Users/moss/code/base/go-common && go test ./configx/... -race -cover`
Expected: PASS,覆盖率不下降

- [ ] **Step 6: gofmt 和 goimports**

Run:
```bash
cd /Users/moss/code/base/go-common
gofmt -w configx/expand.go configx/expand_test.go configx/options.go configx/configx.go configx/configx_test.go
goimports -w configx/
```
Expected: 无 diff 输出(如果 goimports 不存在,跳过;gofmt 必须执行)

- [ ] **Step 7: Commit**

```bash
cd /Users/moss/code/base/go-common
git add configx/configx.go configx/configx_test.go
git commit -m "feat(configx): wire WithExpandEnv into Load with e2e tests"
```

---

## Task 4: 跑 go-common 全包测试 + lint 做最终回归

**项目:** go-common

- [ ] **Step 1: 全包测试**

Run: `cd /Users/moss/code/base/go-common && go test ./... -race`
Expected: 全部 PASS

- [ ] **Step 2: golangci-lint(如果项目有配置)**

Run: `cd /Users/moss/code/base/go-common && golangci-lint run ./configx/... 2>&1 | head -30`
Expected: 无 issue,或者 issue 已存在(与本次改动无关)

如果项目根没有 `.golangci.yaml`,跳过这一步。

- [ ] **Step 3: 在 message-service 端验证 go-common 改动可被消费**

Run:
```bash
cd /Users/moss/code/base/message-service
go build ./...
```
Expected: 编译通过(go.mod replace 指向 ../go-common,本地改动立刻可见)

---

# Phase 2: message-service 启用 ExpandEnv + 准备 docker 配置文件

## Task 5: message-service config.Load 启用 WithExpandEnv + 单测

**项目:** message-service
**Files:**
- Modify: `/Users/moss/code/base/message-service/pkg/config/config.go` (Load 函数)
- Create: `/Users/moss/code/base/message-service/pkg/config/config_test.go`

- [ ] **Step 1: 写失败测试**

创建 `/Users/moss/code/base/message-service/pkg/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
    snowflake:
      machine_id: ${TEST_MS_MACHINE_ID}
      start_time: ${TEST_MS_START_TIME}
`)
	chdir(t, dir)

	setenv(t, "TEST_MS_GRPC_ADDR", ":9000")
	setenv(t, "TEST_MS_HTTP_ADDR", ":8080")
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
	require.Equal(t, ":9000", cfg.Server.GRPCAddr)
	require.Equal(t, ":8080", cfg.Server.HTTPAddr)
	require.Equal(t, "postgres", cfg.Database.Host)
	require.Equal(t, "secret", cfg.Database.Password)
	require.Equal(t, "module", cfg.ThirdParty.GID.Mode)
	require.Equal(t, int64(1), cfg.ThirdParty.GID.Snowflake.MachineID)
}

// TestLoad_NoExpandEnv_KeepsLiteral verifies that without ${VAR} the existing
// default-value style still works (regression guard for local config.yaml).
func TestLoad_NoExpandEnv_KeepsLiteral(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":9000"
database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: message_service
  sslmode: disable
third_party:
  gid:
    mode: module
    snowflake:
      machine_id: 1
      start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ":9000", cfg.Server.GRPCAddr)
	require.Equal(t, "localhost", cfg.Database.Host)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/moss/code/base/message-service && go test ./pkg/config/ -run TestLoad_ExpandEnv -v`
Expected: FAIL —— `cfg.Server.GRPCAddr` 是 `${TEST_MS_GRPC_ADDR}` 字面值,不是 `:9000`(因为 Load 还没启用 WithExpandEnv)

- [ ] **Step 3: 启用 WithExpandEnv**

修改 `/Users/moss/code/base/message-service/pkg/config/config.go` 第 129-141 行的 Load 函数:

```go
// Load reads the message-service config from YAML (and env overrides),
// applies defaults, and runs Validate. Returns a string-keyed Config;
// conversion to the internal enum-keyed form happens at service.New.
//
// WithExpandEnv enables ${VAR} expansion in YAML string values — used by
// the Docker deployment (config.docker.yaml) where every field is a
// ${MESSAGE_SERVICE_*} reference.
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName("message-service"),
		configx.WithEnvPrefix("MESSAGE_SERVICE"),
		configx.WithExpandEnv(),
	); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /Users/moss/code/base/message-service && go test ./pkg/config/ -v`
Expected: 2 个测试 PASS

- [ ] **Step 5: 跑全项目编译,确认改动没破坏其他包**

Run: `cd /Users/moss/code/base/message-service && go build ./...`
Expected: 编译通过

- [ ] **Step 6: Commit**

```bash
cd /Users/moss/code/base/message-service
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "feat(config): enable WithExpandEnv for \${VAR} expansion in YAML"
```

---

## Task 6: 创建 config.docker.yaml

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/config.docker.yaml`

- [ ] **Step 1: 创建文件**

创建 `/Users/moss/code/base/message-service/config.docker.yaml`:

```yaml
# config.docker.yaml — Docker 镜像内的配置文件。
# 所有字段值都引用 MESSAGE_SERVICE_* 环境变量。
# Dockerfile 构建时 COPY 本文件为容器内的 config.yaml。
# 环境变量的实际值由 .env 文件提供（docker-compose 用 env_file 加载）。

server:
  grpc_addr: ${MESSAGE_SERVICE_SERVER_GRPC_ADDR}
  http_addr: ${MESSAGE_SERVICE_SERVER_HTTP_ADDR}

database:
  host: ${MESSAGE_SERVICE_DATABASE_HOST}
  port: ${MESSAGE_SERVICE_DATABASE_PORT}
  user: ${MESSAGE_SERVICE_DATABASE_USER}
  password: ${MESSAGE_SERVICE_DATABASE_PASSWORD}
  dbname: ${MESSAGE_SERVICE_DATABASE_DBNAME}
  sslmode: ${MESSAGE_SERVICE_DATABASE_SSLMODE}

log:
  level: ${MESSAGE_SERVICE_LOG_LEVEL}
  format: ${MESSAGE_SERVICE_LOG_FORMAT}

email:
  vendors:
    custom_smtp:
      accounts:
        - name: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_NAME}
          host: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_HOST}
          port: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PORT}
          username: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_USERNAME}
          password: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PASSWORD}
          from: ${MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_FROM}

sms:
  default_country: ${MESSAGE_SERVICE_SMS_DEFAULT_COUNTRY}
  vendors:
    aliyun:
      accounts:
        - name: ${MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_NAME}
          access_key_id: ${MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_ID}
          access_key_secret: ${MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_SECRET}
          sign_name: ${MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_SIGN_NAME}
          region_id: ${MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_REGION_ID}

cron:
  timezone: ${MESSAGE_SERVICE_CRON_TIMEZONE}
  overlap_policy: ${MESSAGE_SERVICE_CRON_OVERLAP_POLICY}

third_party:
  gid:
    mode: ${MESSAGE_SERVICE_THIRD_PARTY_GID_MODE}
    snowflake:
      machine_id: ${MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_MACHINE_ID}
      start_time: ${MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_START_TIME}
```

- [ ] **Step 2: 验证 yaml 语法正确**

Run: `cd /Users/moss/code/base/message-service && python3 -c "import yaml; yaml.safe_load(open('config.docker.yaml'))"`
Expected: 无输出(yaml 语法 OK)

如果没有 python3,用 docker:`docker run --rm -v $(pwd):/w -w /w python:3-alpine python -c "import yaml; yaml.safe_load(open('config.docker.yaml'))"`

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/message-service
git add config.docker.yaml
git commit -m "feat(config): add config.docker.yaml with \${VAR} references for Docker"
```

---

## Task 7: 创建 .env.example

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/.env.example`

- [ ] **Step 1: 创建文件**

创建 `/Users/moss/code/base/message-service/.env.example`:

```bash
# .env.example — message-service 部署环境变量模板
# 使用:cp .env.example .env，编辑 .env 填入实际值（至少 db password）
# docker-compose 自动通过 env_file 加载 .env

# ---- Server ----
MESSAGE_SERVICE_SERVER_GRPC_ADDR=:9000
MESSAGE_SERVICE_SERVER_HTTP_ADDR=:8080

# ---- Database ----
MESSAGE_SERVICE_DATABASE_HOST=postgres
MESSAGE_SERVICE_DATABASE_PORT=5432
MESSAGE_SERVICE_DATABASE_USER=postgres
MESSAGE_SERVICE_DATABASE_PASSWORD=
MESSAGE_SERVICE_DATABASE_DBNAME=message_service
MESSAGE_SERVICE_DATABASE_SSLMODE=disable

# ---- Logging ----
MESSAGE_SERVICE_LOG_LEVEL=info
MESSAGE_SERVICE_LOG_FORMAT=json

# ---- Email (custom SMTP；不填 host 则禁用 custom_smtp vendor) ----
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_NAME=default
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_HOST=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PORT=587
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_USERNAME=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PASSWORD=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_FROM=

# ---- SMS (Aliyun；不填 key 则禁用 aliyun vendor) ----
MESSAGE_SERVICE_SMS_DEFAULT_COUNTRY=CN
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_NAME=default
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_ID=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_SECRET=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_SIGN_NAME=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_REGION_ID=cn-hangzhou

# ---- Cron ----
MESSAGE_SERVICE_CRON_TIMEZONE=Asia/Shanghai
MESSAGE_SERVICE_CRON_OVERLAP_POLICY=skip

# ---- Snowflake（module 模式） ----
MESSAGE_SERVICE_THIRD_PARTY_GID_MODE=module
MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_MACHINE_ID=1
MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_START_TIME=2026-06-01T00:00:00Z
```

- [ ] **Step 2: Commit**

```bash
cd /Users/moss/code/base/message-service
git add .env.example
git commit -m "feat(deploy): add .env.example listing all MESSAGE_SERVICE_* vars"
```

---

# Phase 3: Docker 部署

## Task 8: 创建 entrypoint.sh + .dockerignore

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/entrypoint.sh`
- Create: `/Users/moss/code/base/message-service/.dockerignore`

- [ ] **Step 1: 创建 entrypoint.sh**

创建 `/Users/moss/code/base/message-service/entrypoint.sh`:

```sh
#!/bin/sh
set -e

echo "[entrypoint] running migrations..."
./migrate

echo "[entrypoint] starting server..."
exec ./server
```

- [ ] **Step 2: 加可执行权限**

Run: `cd /Users/moss/code/base/message-service && chmod +x entrypoint.sh && ls -la entrypoint.sh`
Expected: 文件 mode 是 `-rwxr-xr-x` 或类似(有 x)

- [ ] **Step 3: 创建 .dockerignore**

创建 `/Users/moss/code/base/message-service/.dockerignore`:

```
# Build artifacts
bin/
coverage.out
coverage.html

# Local env (must not leak into image)
.env

# Git and IDE
.git/
.gitignore
.idea/
.vscode/

# Docs and plans
docs/

# Worktrees
.worktrees/

# Test/CI scratch
*.tmp
*.log
```

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/message-service
git add entrypoint.sh .dockerignore
git commit -m "feat(deploy): add entrypoint.sh (migrate then exec server) and .dockerignore"
```

---

## Task 9: 改造 Dockerfile

**项目:** message-service
**Files:**
- Modify: `/Users/moss/code/base/message-service/Dockerfile`

- [ ] **Step 1: 重写 Dockerfile**

完整替换 `/Users/moss/code/base/message-service/Dockerfile`:

```dockerfile
# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# Runtime stage:alpine has a shell so entrypoint.sh can run migrate first.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 appuser

WORKDIR /app
COPY --from=builder /out/server         ./server
COPY --from=builder /out/migrate        ./migrate
COPY entrypoint.sh                      ./entrypoint.sh
COPY config.docker.yaml                 ./config.yaml
RUN chmod +x entrypoint.sh server migrate

USER appuser
EXPOSE 9000 8080
ENTRYPOINT ["./entrypoint.sh"]
```

- [ ] **Step 2: 本地验证 docker build 能成功**

Run: `cd /Users/moss/code/base/message-service && docker build -t message-service:test .`
Expected: 构建成功(无 ERROR),最终镜像 tag 是 message-service:test

如果失败,先排查:
- go mod download 失败 → 检查 go.sum
- compile 失败 → go build ./... 应该已经能通过
- COPY 失败 → 文件路径核对

- [ ] **Step 3: 验证镜像内 entrypoint 能列出文件**

Run: `docker run --rm --entrypoint sh message-service:test -c "ls -la /app"`
Expected: 看到 server、migrate、entrypoint.sh、config.yaml 四个文件

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/message-service
git add Dockerfile
git commit -m "feat(deploy): rewrite Dockerfile to build both binaries, use alpine, auto-migrate via entrypoint"
```

---

## Task 10: 创建 docker-compose.yaml + 更新 .gitignore

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/docker-compose.yaml`
- Modify: `/Users/moss/code/base/message-service/.gitignore`

- [ ] **Step 1: 创建 docker-compose.yaml**

创建 `/Users/moss/code/base/message-service/docker-compose.yaml`:

```yaml
services:
  postgres:
    image: postgres:17-alpine
    environment:
      - POSTGRES_DB=${MESSAGE_SERVICE_DATABASE_DBNAME:-message_service}
      - POSTGRES_USER=${MESSAGE_SERVICE_DATABASE_USER:-postgres}
      - POSTGRES_PASSWORD=${MESSAGE_SERVICE_DATABASE_PASSWORD:-postgres}
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${MESSAGE_SERVICE_DATABASE_USER:-postgres}"]
      interval: 3s
      timeout: 3s
      retries: 20

  message-service:
    build: .
    env_file: .env
    environment:
      # Force container-internal DNS resolution to the postgres service.
      - MESSAGE_SERVICE_DATABASE_HOST=postgres
      - MESSAGE_SERVICE_DATABASE_PORT=5432
    ports:
      - "9000:9000"
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy

volumes:
  pgdata:
```

注意:
- postgres service 不挂 env_file(避免 message-service 的环境变量污染 postgres);POSTGRES_* 通过 `${VAR:-default}` 从 docker-compose 启动环境(由 `.env` 自动注入)取值
- message-service 强制覆盖 DATABASE_HOST/PORT,避免 .env 里写了 localhost 的本地开发值污染容器

- [ ] **Step 2: 更新 .gitignore**

修改 `/Users/moss/code/base/message-service/.gitignore`,在末尾追加:

```
# Local env file (keep .env.example committed)
.env
```

- [ ] **Step 3: 验证 compose 配置语法**

Run: `cd /Users/moss/code/base/message-service && cp .env.example .env.test && MESSAGE_SERVICE_DATABASE_PASSWORD=test docker compose --env-file .env.test config > /dev/null && rm .env.test`
Expected: 无输出(yaml 语法 OK,变量解析正常)

如果失败:
- 检查 docker-compose.yaml 缩进
- 检查 `${VAR:-default}` 语法

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/message-service
git add docker-compose.yaml .gitignore
git commit -m "feat(deploy): add docker-compose.yaml with postgres healthcheck + gitignore .env"
```

---

## Task 11: 端到端验证 docker compose up

**项目:** message-service

这个 task 没有代码改动,只验证 Task 6-10 的产物能联动起来。手动验证,不写自动化测试。

- [ ] **Step 1: 准备 .env**

Run:
```bash
cd /Users/moss/code/base/message-service
cp .env.example .env
# 设置一个测试用的 db 密码（本地测试用，不要 commit）
sed -i.bak 's/^MESSAGE_SERVICE_DATABASE_PASSWORD=$/MESSAGE_SERVICE_DATABASE_PASSWORD=docker-test-pwd/' .env
rm .env.bak
cat .env | grep PASSWORD
```
Expected: 看到 `MESSAGE_SERVICE_DATABASE_PASSWORD=docker-test-pwd`

- [ ] **Step 2: docker compose up**

Run: `cd /Users/moss/code/base/message-service && docker compose up --build -d`
Expected:
- postgres 拉镜像/启动 → healthy
- message-service 镜像构建成功
- message-service 容器启动,日志显示 migrate 完成 + server 启动

- [ ] **Step 3: 查看日志确认 migrate + server 都启动**

Run: `cd /Users/moss/code/base/message-service && docker compose logs message-service | tail -30`
Expected: 日志里有 "[entrypoint] running migrations..." + "[entrypoint] starting server..." + server 正常监听 9000/8080

- [ ] **Step 4: 用 grpcurl 验证 gRPC 端口可访问(可选)**

如果有 grpcurl:`grpcurl -plaintext localhost:9000 list`

Expected: 列出 `message.v1.MessageService`

如果没有 grpcurl,跳过。

- [ ] **Step 5: 用 curl 验证 HTTP 端口可访问**

Run: `curl -s -o /dev/null -w "%{http_code}\n" "http://localhost:8080/v1/emails"`
Expected: HTTP 200(ListEmails 空列表)或 400(参数校验失败);非 502/503/连接拒绝即说明 gateway 起来了

- [ ] **Step 6: 清理(测试通过后)**

Run: `cd /Users/moss/code/base/message-service && docker compose down -v`
Expected: 容器和 volume 都被清除

⚠️ **保留 .env 文件**(后续 Task 19 smoke-test 还要用),不要删。

- [ ] **Step 7: 这个 task 不 commit**(纯验证步骤)

---

# Phase 4: 测试客户端

## Task 12: 创建 Caller 接口 + 目录骨架

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/client.go`

- [ ] **Step 1: 创建 client.go 定义 Caller 接口**

创建 `/Users/moss/code/base/message-service/cmd/testclient/client.go`:

```go
// Package main implements msgclient, a CLI for smoke-testing message-service
// over both gRPC and HTTP (grpc-gateway). Each subcommand exercises one RPC.
package main

import (
	"context"

	pb "message-service/gen/message/v1"
)

// Caller abstracts the transport so subcommands can be written once and
// dispatched to either grpcClient or httpClient based on --mode.
type Caller interface {
	SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error)
	SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error)
	GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error)
	ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error)
	ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error)
	GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error)
	GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error)
	ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error)
	ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error)
	GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error)
}
```

- [ ] **Step 2: 编译验证(此时 main.go 还不存在,跳过编译,只检查语法)**

Run: `cd /Users/moss/code/base/message-service && go vet ./cmd/testclient/ 2>&1 | head -5`
Expected: 报 "no Go files" 或类似(因为还没有 main)—— 这是预期的,继续下一步

- [ ] **Step 3: 暂不 commit**(等 main.go 一起 commit,因为没 main 的包不能编译)

---

## Task 13: 实现 grpcClient

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/grpc_client.go`

- [ ] **Step 1: 创建 grpc_client.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/grpc_client.go`:

```go
package main

import (
	"context"

	messageservice "message-service/pkg"
	pb "message-service/gen/message/v1"
)

// grpcClient adapts *messageservice.Client to the Caller interface.
// All methods are thin pass-throughs to the embedded pb client.
type grpcClient struct {
	c *messageservice.Client
}

func newGRPCClient(target string) (*grpcClient, error) {
	c, err := messageservice.NewClient(target)
	if err != nil {
		return nil, err
	}
	return &grpcClient{c: c}, nil
}

func (g *grpcClient) Close() error { return g.c.Close() }

func (g *grpcClient) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return g.c.SendEmail(ctx, req)
}

func (g *grpcClient) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return g.c.SendSMS(ctx, req)
}

func (g *grpcClient) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return g.c.GetEmail(ctx, req)
}

func (g *grpcClient) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return g.c.ListEmails(ctx, req)
}

func (g *grpcClient) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	return g.c.ListEmailsByCursor(ctx, req)
}

func (g *grpcClient) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return g.c.GetEmailStats(ctx, req)
}

func (g *grpcClient) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return g.c.GetSMS(ctx, req)
}

func (g *grpcClient) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return g.c.ListSMS(ctx, req)
}

func (g *grpcClient) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	return g.c.ListSMSByCursor(ctx, req)
}

func (g *grpcClient) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return g.c.GetSMSStats(ctx, req)
}
```

- [ ] **Step 2: 暂不单独验证**(等 Task 14 main.go 一起编译)

---

## Task 14: 实现 httpClient

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/http_client.go`

- [ ] **Step 1: 创建 http_client.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/http_client.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	pb "message-service/gen/message/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// httpClient calls message-service via the grpc-gateway REST endpoints.
// It uses protojson for marshaling so enum fields are string-typed (e.g.
// "EMAIL_VENDOR_CUSTOM_SMTP"), matching grpc-gateway's default response codec.
type httpClient struct {
	base string
	c    *http.Client
}

func newHTTPClient(base string) *httpClient {
	return &httpClient{
		base: strings.TrimRight(base, "/"),
		c:    &http.Client{},
	}
}

// doPost sends a POST with a JSON body and decodes the JSON response into resp.
func (h *httpClient) doPost(ctx context.Context, path string, req any, resp any) error {
	body, err := protojson.Marshal(req.(protoMessage))
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return h.do(httpReq, resp)
}

// doGet sends a GET with query string derived from req via struct tags and
// decodes the JSON response into resp.
func (h *httpClient) doGet(ctx context.Context, path string, query url.Values, resp any) error {
	u := h.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return h.do(httpReq, resp)
}

func (h *httpClient) do(httpReq *http.Request, resp any) error {
	httpResp, err := h.c.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", httpResp.StatusCode, string(raw))
	}
	if resp == nil {
		return nil
	}
	if err := protojson.Unmarshal(raw, resp.(protoMessage)); err != nil {
		return fmt.Errorf("unmarshal response: %w (body=%s)", err, string(raw))
	}
	return nil
}

// protoMessage is a constraint alias for proto.Message to avoid importing
// the full proto package interface name in signatures.
type protoMessage interface {
	Reset()
	String() string
	ProtoMessage()
}

// --- Caller impl ---

func (h *httpClient) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	resp := &pb.SendResponse{}
	return resp, h.doPost(ctx, "/v1/messages:email", req, resp)
}

func (h *httpClient) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	resp := &pb.SendResponse{}
	return resp, h.doPost(ctx, "/v1/messages:sms", req, resp)
}

func (h *httpClient) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	resp := &pb.EmailRecord{}
	return resp, h.doGet(ctx, "/v1/emails/"+strconv.FormatInt(req.Id, 10), nil, resp)
}

func (h *httpClient) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	resp := &pb.ListEmailsResponse{}
	return resp, h.doGet(ctx, "/v1/emails", listEmailsQuery(req), resp)
}

func (h *httpClient) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	resp := &pb.ListEmailsByCursorResponse{}
	return resp, h.doGet(ctx, "/v1/emails:cursor", listEmailsByCursorQuery(req), resp)
}

func (h *httpClient) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	resp := &pb.EmailStatsResponse{}
	q := url.Values{}
	if req.Vendor != 0 {
		q.Set("vendor", req.Vendor.String())
	}
	if req.Scene != 0 {
		q.Set("scene", req.Scene.String())
	}
	if req.StartTime != 0 {
		q.Set("start_time", strconv.FormatInt(req.StartTime, 10))
	}
	if req.EndTime != 0 {
		q.Set("end_time", strconv.FormatInt(req.EndTime, 10))
	}
	return resp, h.doGet(ctx, "/v1/emails:stats", q, resp)
}

func (h *httpClient) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	resp := &pb.SMSRecord{}
	return resp, h.doGet(ctx, "/v1/sms/"+strconv.FormatInt(req.Id, 10), nil, resp)
}

func (h *httpClient) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	resp := &pb.ListSMSResponse{}
	return resp, h.doGet(ctx, "/v1/sms", listSMSQuery(req), resp)
}

func (h *httpClient) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	resp := &pb.ListSMSByCursorResponse{}
	return resp, h.doGet(ctx, "/v1/sms:cursor", listSMSByCursorQuery(req), resp)
}

func (h *httpClient) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	resp := &pb.SMSStatsResponse{}
	q := url.Values{}
	if req.Vendor != 0 {
		q.Set("vendor", req.Vendor.String())
	}
	if req.Scene != 0 {
		q.Set("scene", req.Scene.String())
	}
	if req.StartTime != 0 {
		q.Set("start_time", strconv.FormatInt(req.StartTime, 10))
	}
	if req.EndTime != 0 {
		q.Set("end_time", strconv.FormatInt(req.EndTime, 10))
	}
	return resp, h.doGet(ctx, "/v1/sms:stats", q, resp)
}

// --- query builders (kept in their respective command files; declared here for stats above) ---
// listEmailsQuery, listEmailsByCursorQuery, listSMSQuery, listSMSByCursorQuery
// are defined in email.go and sms.go to keep transport-agnostic logic near the
// command that owns each request shape.
```

---

## Task 15: 实现 email 子命令

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/email.go`

- [ ] **Step 1: 创建 email.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/email.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"

	pb "message-service/gen/message/v1"
)

// parseEmailVendor converts a string like "custom_smtp" or "aliyun" to enum.
// Returns UNSPECIFIED on empty (let server default).
func parseEmailVendor(s string) pb.EmailVendor {
	switch s {
	case "", "unspecified":
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	case "custom_smtp":
		return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	}
	return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
}

func parseEmailScene(s string) pb.EmailScene {
	switch s {
	case "login_code":
		return pb.EmailScene_EMAIL_SCENE_LOGIN_CODE
	case "forgot_password":
		return pb.EmailScene_EMAIL_SCENE_FORGOT_PASSWORD
	case "register":
		return pb.EmailScene_EMAIL_SCENE_REGISTER
	case "change_password":
		return pb.EmailScene_EMAIL_SCENE_CHANGE_PASSWORD
	case "bind_account":
		return pb.EmailScene_EMAIL_SCENE_BIND_ACCOUNT
	case "notification":
		return pb.EmailScene_EMAIL_SCENE_NOTIFICATION
	}
	return pb.EmailScene_EMAIL_SCENE_UNSPECIFIED
}

func runSendEmail(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("send-email", flag.ExitOnError)
	to := fs.String("to", "", "recipient email (required)")
	subject := fs.String("subject", "", "subject (required)")
	body := fs.String("body", "", "plain text body")
	htmlBody := fs.String("html-body", "", "HTML body (optional)")
	vendor := fs.String("vendor", "", "email vendor: custom_smtp|aliyun|tencent|netease (empty = default fallback)")
	account := fs.String("account", "", "vendor account name (required if --vendor set)")
	scene := fs.String("scene", "notification", "business scene: login_code|forgot_password|register|change_password|bind_account|notification")
	senderID := fs.String("sender", "", "sender_id (required)")
	fs.Parse(args)

	if *to == "" || *subject == "" || *senderID == "" {
		return fmt.Errorf("--to, --subject, --sender are required")
	}

	req := &pb.SendEmailRequest{
		To:       *to,
		Subject:  *subject,
		Body:     *body,
		HtmlBody: *htmlBody,
		Vendor:   parseEmailVendor(*vendor),
		Account:  *account,
		Scene:    parseEmailScene(*scene),
		SenderId: *senderID,
	}
	resp, err := c.SendEmail(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("sent. id=%d status=%s\n", resp.Id, resp.Status)
	return nil
}

func runGetEmail(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("get-email", flag.ExitOnError)
	id := fs.Int64("id", 0, "email record id (required)")
	fs.Parse(args)
	if *id <= 0 {
		return fmt.Errorf("--id is required and must be > 0")
	}
	r, err := c.GetEmail(ctx, &pb.GetEmailRequest{Id: *id})
	if err != nil {
		return err
	}
	printJSON(r)
	return nil
}

func runListEmails(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-emails", flag.ExitOnError)
	page := fs.Int("page", 1, "page number (1-based)")
	pageSize := fs.Int("page-size", 10, "page size")
	vendor := fs.String("vendor", "", "filter by vendor")
	scene := fs.String("scene", "", "filter by scene")
	senderID := fs.String("sender", "", "filter by sender_id")
	fs.Parse(args)

	req := &pb.ListEmailsRequest{
		Page:     int32(*page),
		PageSize: int32(*pageSize),
		SenderId: *senderID,
	}
	if *vendor != "" {
		req.Vendor = parseEmailVendor(*vendor)
	}
	if *scene != "" {
		req.Scene = parseEmailScene(*scene)
	}
	resp, err := c.ListEmails(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("total=%d total_pages=%d has_more=%v records=%d\n",
		resp.Total, resp.TotalPages, resp.HasMore, len(resp.Records))
	for _, r := range resp.Records {
		fmt.Printf("  id=%d status=%s to=%s subject=%q\n", r.Id, r.Status, r.Target, r.Subject)
	}
	return nil
}

func runListEmailsByCursor(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-emails-by-cursor", flag.ExitOnError)
	pageSize := fs.Int("page-size", 10, "page size")
	pageToken := fs.String("page-token", "", "cursor from previous response.next_page_token (empty = first page)")
	senderID := fs.String("sender", "", "filter by sender_id")
	fs.Parse(args)

	req := &pb.ListEmailsByCursorRequest{
		PageSize:  int32(*pageSize),
		PageToken: *pageToken,
		SenderId:  *senderID,
	}
	resp, err := c.ListEmailsByCursor(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("next_page_token=%q records=%d\n", resp.NextPageToken, len(resp.Records))
	for _, r := range resp.Records {
		fmt.Printf("  id=%d status=%s to=%s\n", r.Id, r.Status, r.Target)
	}
	return nil
}

func runGetEmailStats(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("get-email-stats", flag.ExitOnError)
	startStr := fs.String("start", "", "start time RFC3339 (e.g. 2026-06-01T00:00:00Z)")
	endStr := fs.String("end", "", "end time RFC3339")
	vendor := fs.String("vendor", "", "filter by vendor")
	fs.Parse(args)

	req := &pb.GetEmailStatsRequest{}
	if *startStr != "" {
		t, err := time.Parse(time.RFC3339, *startStr)
		if err != nil {
			return fmt.Errorf("--start: %w", err)
		}
		req.StartTime = t.Unix()
	}
	if *endStr != "" {
		t, err := time.Parse(time.RFC3339, *endStr)
		if err != nil {
			return fmt.Errorf("--end: %w", err)
		}
		req.EndTime = t.Unix()
	}
	if *vendor != "" {
		req.Vendor = parseEmailVendor(*vendor)
	}
	resp, err := c.GetEmailStats(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("total=%d sent=%d failed=%d success_rate=%d\n",
		resp.Total, resp.Sent, resp.Failed, resp.SuccessRate)
	return nil
}

// listEmailsQuery builds the query string for ListEmails HTTP requests.
// Used by httpClient.ListEmails.
func listEmailsQuery(req *pb.ListEmailsRequest) url.Values {
	q := url.Values{}
	if req.Vendor != 0 {
		q.Set("vendor", req.Vendor.String())
	}
	if req.Scene != 0 {
		q.Set("scene", req.Scene.String())
	}
	if req.SenderId != "" {
		q.Set("sender_id", req.SenderId)
	}
	if req.Page != 0 {
		q.Set("page", strconv.Itoa(int(req.Page)))
	}
	if req.PageSize != 0 {
		q.Set("page_size", strconv.Itoa(int(req.PageSize)))
	}
	return q
}

// listEmailsByCursorQuery builds the query string for ListEmailsByCursor HTTP requests.
func listEmailsByCursorQuery(req *pb.ListEmailsByCursorRequest) url.Values {
	q := url.Values{}
	if req.PageSize != 0 {
		q.Set("page_size", strconv.Itoa(int(req.PageSize)))
	}
	if req.PageToken != "" {
		q.Set("page_token", req.PageToken)
	}
	if req.SenderId != "" {
		q.Set("sender_id", req.SenderId)
	}
	return q
}
```

---

## Task 16: 实现 sms 子命令

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/sms.go`

- [ ] **Step 1: 创建 sms.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/sms.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"

	pb "message-service/gen/message/v1"
)

func parseSmsVendor(s string) pb.SmsVendor {
	switch s {
	case "", "unspecified":
		return pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	case "aliyun":
		return pb.SmsVendor_SMS_VENDOR_ALIYUN
	}
	return pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
}

func parseSmsScene(s string) pb.SmsScene {
	switch s {
	case "login_code":
		return pb.SmsScene_SMS_SCENE_LOGIN_CODE
	case "forgot_password":
		return pb.SmsScene_SMS_SCENE_FORGOT_PASSWORD
	case "register":
		return pb.SmsScene_SMS_SCENE_REGISTER
	case "change_password":
		return pb.SmsScene_SMS_SCENE_CHANGE_PASSWORD
	case "bind_account":
		return pb.SmsScene_SMS_SCENE_BIND_ACCOUNT
	}
	return pb.SmsScene_SMS_SCENE_UNSPECIFIED
}

func runSendSMS(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("send-sms", flag.ExitOnError)
	to := fs.String("phone", "", "recipient phone E.164 or local (required)")
	content := fs.String("content", "", "SMS text content")
	vendor := fs.String("vendor", "", "sms vendor: aliyun (empty = route by country)")
	account := fs.String("account", "", "vendor account name (required if --vendor set)")
	scene := fs.String("scene", "login_code", "business scene")
	senderID := fs.String("sender", "", "sender_id (required)")
	fs.Parse(args)

	if *to == "" || *senderID == "" {
		return fmt.Errorf("--phone, --sender are required")
	}

	req := &pb.SendSMSRequest{
		To:       *to,
		Content:  *content,
		Vendor:   parseSmsVendor(*vendor),
		Account:  *account,
		Scene:    parseSmsScene(*scene),
		SenderId: *senderID,
	}
	resp, err := c.SendSMS(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("sent. id=%d status=%s\n", resp.Id, resp.Status)
	return nil
}

func runGetSMS(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("get-sms", flag.ExitOnError)
	id := fs.Int64("id", 0, "sms record id (required)")
	fs.Parse(args)
	if *id <= 0 {
		return fmt.Errorf("--id is required and must be > 0")
	}
	r, err := c.GetSMS(ctx, &pb.GetSMSRequest{Id: *id})
	if err != nil {
		return err
	}
	printJSON(r)
	return nil
}

func runListSMS(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-sms", flag.ExitOnError)
	page := fs.Int("page", 1, "page number")
	pageSize := fs.Int("page-size", 10, "page size")
	vendor := fs.String("vendor", "", "filter by vendor")
	scene := fs.String("scene", "", "filter by scene")
	senderID := fs.String("sender", "", "filter by sender_id")
	fs.Parse(args)

	req := &pb.ListSMSRequest{
		Page:     int32(*page),
		PageSize: int32(*pageSize),
		SenderId: *senderID,
	}
	if *vendor != "" {
		req.Vendor = parseSmsVendor(*vendor)
	}
	if *scene != "" {
		req.Scene = parseSmsScene(*scene)
	}
	resp, err := c.ListSMS(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("total=%d total_pages=%d has_more=%v records=%d\n",
		resp.Total, resp.TotalPages, resp.HasMore, len(resp.Records))
	for _, r := range resp.Records {
		fmt.Printf("  id=%d status=%s to=%s\n", r.Id, r.Status, r.Target)
	}
	return nil
}

func runListSMSByCursor(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-sms-by-cursor", flag.ExitOnError)
	pageSize := fs.Int("page-size", 10, "page size")
	pageToken := fs.String("page-token", "", "cursor from previous response.next_page_token")
	senderID := fs.String("sender", "", "filter by sender_id")
	fs.Parse(args)

	req := &pb.ListSMSByCursorRequest{
		PageSize:  int32(*pageSize),
		PageToken: *pageToken,
		SenderId:  *senderID,
	}
	resp, err := c.ListSMSByCursor(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("next_page_token=%q records=%d\n", resp.NextPageToken, len(resp.Records))
	for _, r := range resp.Records {
		fmt.Printf("  id=%d status=%s to=%s\n", r.Id, r.Status, r.Target)
	}
	return nil
}

func runGetSMSStats(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("get-sms-stats", flag.ExitOnError)
	startStr := fs.String("start", "", "start time RFC3339")
	endStr := fs.String("end", "", "end time RFC3339")
	vendor := fs.String("vendor", "", "filter by vendor")
	fs.Parse(args)

	req := &pb.GetSMSStatsRequest{}
	if *startStr != "" {
		t, err := time.Parse(time.RFC3339, *startStr)
		if err != nil {
			return fmt.Errorf("--start: %w", err)
		}
		req.StartTime = t.Unix()
	}
	if *endStr != "" {
		t, err := time.Parse(time.RFC3339, *endStr)
		if err != nil {
			return fmt.Errorf("--end: %w", err)
		}
		req.EndTime = t.Unix()
	}
	if *vendor != "" {
		req.Vendor = parseSmsVendor(*vendor)
	}
	resp, err := c.GetSMSStats(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("total=%d sent=%d failed=%d success_rate=%d\n",
		resp.Total, resp.Sent, resp.Failed, resp.SuccessRate)
	return nil
}

// listSMSQuery and listSMSByCursorQuery are used by httpClient.
func listSMSQuery(req *pb.ListSMSRequest) url.Values {
	q := url.Values{}
	if req.Vendor != 0 {
		q.Set("vendor", req.Vendor.String())
	}
	if req.Scene != 0 {
		q.Set("scene", req.Scene.String())
	}
	if req.SenderId != "" {
		q.Set("sender_id", req.SenderId)
	}
	if req.Page != 0 {
		q.Set("page", strconv.Itoa(int(req.Page)))
	}
	if req.PageSize != 0 {
		q.Set("page_size", strconv.Itoa(int(req.PageSize)))
	}
	return q
}

func listSMSByCursorQuery(req *pb.ListSMSByCursorRequest) url.Values {
	q := url.Values{}
	if req.PageSize != 0 {
		q.Set("page_size", strconv.Itoa(int(req.PageSize)))
	}
	if req.PageToken != "" {
		q.Set("page_token", req.PageToken)
	}
	if req.SenderId != "" {
		q.Set("sender_id", req.SenderId)
	}
	return q
}
```

---

## Task 17: 实现 smoke-test + main.go 入口

**项目:** message-service
**Files:**
- Create: `/Users/moss/code/base/message-service/cmd/testclient/smoke.go`
- Create: `/Users/moss/code/base/message-service/cmd/testclient/main.go`

- [ ] **Step 1: 创建 smoke.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/smoke.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	pb "message-service/gen/message/v1"
)

// runSmokeTest exercises the full send→get→list loop once for each protocol
// the Caller wraps. Caller is constructed by main based on --mode.
// It uses timestamp-derived unique values so repeated runs don't collide
// on idempotency or polluted listing.
func runSmokeTest(ctx context.Context, c Caller, senderID string) error {
	stamp := time.Now().UnixMilli()

	// 1. send email
	emailReq := &pb.SendEmailRequest{
		To:       fmt.Sprintf("smoke+%d@example.com", stamp),
		Subject:  fmt.Sprintf("smoke test %d", stamp),
		Body:     "smoke-test body",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: senderID,
	}
	er, err := c.SendEmail(ctx, emailReq)
	if err != nil {
		return fmt.Errorf("SendEmail: %w", err)
	}
	fmt.Printf("[smoke] SendEmail ok: id=%d status=%s\n", er.Id, er.Status)

	// 2. get email back
	if _, err := c.GetEmail(ctx, &pb.GetEmailRequest{Id: er.Id}); err != nil {
		return fmt.Errorf("GetEmail(%d): %w", er.Id, err)
	}
	fmt.Printf("[smoke] GetEmail(%d) ok\n", er.Id)

	// 3. list emails (just verify it returns no error)
	if _, err := c.ListEmails(ctx, &pb.ListEmailsRequest{SenderId: senderID, PageSize: 5}); err != nil {
		return fmt.Errorf("ListEmails: %w", err)
	}
	fmt.Printf("[smoke] ListEmails ok\n")

	// 4. send sms (note: vendor credentials may not be set; this may fail at
	// the vendor layer. We tolerate SendSMS errors here so smoke-test still
	// validates the gRPC/HTTP plumbing end-to-end).
	smsReq := &pb.SendSMSRequest{
		To:       "13800000000",
		Content:  fmt.Sprintf("smoke %d", stamp),
		Vendor:   pb.SmsVendor_SMS_VENDOR_ALIYUN,
		Account:  "default",
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId: senderID,
	}
	sr, err := c.SendSMS(ctx, smsReq)
	if err != nil {
		fmt.Printf("[smoke] SendSMS returned error (expected if vendor creds unset): %v\n", err)
	} else {
		fmt.Printf("[smoke] SendSMS ok: id=%d status=%s\n", sr.Id, sr.Status)
		if _, err := c.GetSMS(ctx, &pb.GetSMSRequest{Id: sr.Id}); err != nil {
			fmt.Printf("[smoke] GetSMS(%d) returned error: %v\n", sr.Id, err)
		} else {
			fmt.Printf("[smoke] GetSMS(%d) ok\n", sr.Id)
		}
	}

	// 5. list sms
	if _, err := c.ListSMS(ctx, &pb.ListSMSRequest{SenderId: senderID, PageSize: 5}); err != nil {
		return fmt.Errorf("ListSMS: %w", err)
	}
	fmt.Printf("[smoke] ListSMS ok\n")

	fmt.Println("[smoke] DONE")
	return nil
}
```

- [ ] **Step 2: 创建 main.go**

创建 `/Users/moss/code/base/message-service/cmd/testclient/main.go`:

```go
// Package main is the msgclient binary entrypoint:dispatches subcommands to
// gRPC or HTTP based on --mode.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

const usage = `msgclient — message-service test client

Usage:
  msgclient [global flags] <subcommand> [subcommand flags]

Global flags:
  -mode string     grpc | http (default "grpc")
  -target string   gRPC target (default "localhost:9000")
  -base string     HTTP base URL (default "http://localhost:8080")
  -sender string   default sender_id for subcommands that need one (default "smoke")

Subcommands:
  send-email, get-email, list-emails, list-emails-by-cursor, get-email-stats
  send-sms,    get-sms,  list-sms,  list-sms-by-cursor,    get-sms-stats
  smoke-test

Examples:
  msgclient send-email --to=a@b.com --subject=hi --body=hello --sender=admin
  msgclient -mode http -base http://localhost:8080 send-email --to=a@b.com --subject=hi --body=hello --sender=admin
  msgclient smoke-test
  msgclient list-emails --page-size 5
`

type globalFlags struct {
	mode     string
	target   string
	base     string
	sender   string
}

func main() {
	os.Exit(run())
}

func run() int {
	g := parseGlobals()
	if g == nil {
		fmt.Print(usage)
		return 2
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing subcommand")
		fmt.Print(usage)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, cleanup, err := buildCaller(ctx, g)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: build caller: %v\n", err)
		return 1
	}
	defer cleanup()

	if err := dispatch(ctx, c, g, args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func parseGlobals() *globalFlags {
	g := &globalFlags{}
	flag.StringVar(&g.mode, "mode", "grpc", "grpc | http")
	flag.StringVar(&g.target, "target", "localhost:9000", "gRPC target")
	flag.StringVar(&g.base, "base", "http://localhost:8080", "HTTP base URL")
	flag.StringVar(&g.sender, "sender", "smoke", "default sender_id")
	flag.Usage = func() { fmt.Print(usage) }
	flag.Parse()
	return g
}

type closeable interface {
	Close() error
}

func buildCaller(_ context.Context, g *globalFlags) (Caller, func(), error) {
	switch g.mode {
	case "grpc":
		c, err := newGRPCClient(g.target)
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	case "http":
		return newHTTPClient(g.base), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unknown mode %q (use grpc or http)", g.mode)
	}
}

func dispatch(ctx context.Context, c Caller, g *globalFlags, args []string) error {
	sub := args[0]
	rest := args[1:]
	sender := g.sender

	switch sub {
	case "send-email":
		return runSendEmail(ctx, c, rest)
	case "get-email":
		return runGetEmail(ctx, c, rest)
	case "list-emails":
		return runListEmails(ctx, c, rest)
	case "list-emails-by-cursor":
		return runListEmailsByCursor(ctx, c, rest)
	case "get-email-stats":
		return runGetEmailStats(ctx, c, rest)
	case "send-sms":
		return runSendSMS(ctx, c, rest)
	case "get-sms":
		return runGetSMS(ctx, c, rest)
	case "list-sms":
		return runListSMS(ctx, c, rest)
	case "list-sms-by-cursor":
		return runListSMSByCursor(ctx, c, rest)
	case "get-sms-stats":
		return runGetSMSStats(ctx, c, rest)
	case "smoke-test":
		return runSmokeTest(ctx, c, sender)
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

// printJSON pretty-prints a proto message as canonical JSON.
// Using encoding/json directly works because generated proto structs carry
// json tags; for richer fidelity (enum names, etc.) protojson would be used,
// but for human-readable CLI output this is sufficient.
func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
```

- [ ] **Step 3: 尝试编译**

Run: `cd /Users/moss/code/base/message-service && go build ./cmd/testclient/`
Expected: 编译通过

- [ ] **Step 4: 编译并产出二进制**

Run: `cd /Users/moss/code/base/message-service && go build -o bin/msgclient ./cmd/testclient/ && ls -la bin/msgclient`
Expected: `bin/msgclient` 文件存在,可执行

- [ ] **Step 5: 测试 CLI help**

Run: `cd /Users/moss/code/base/message-service && ./bin/msgclient 2>&1 | head -10`
Expected: 看到 usage 文本,exit code 是 2(无子命令)

- [ ] **Step 6: Commit**

```bash
cd /Users/moss/code/base/message-service
git add cmd/testclient/
git commit -m "feat(testclient): add msgclient CLI with grpc/http dual-protocol support"
```

---

## Task 18: Makefile 加 build-client target

**项目:** message-service
**Files:**
- Modify: `/Users/moss/code/base/message-service/Makefile`

- [ ] **Step 1: 在 Makefile 末尾追加 build-client target**

修改 `/Users/moss/code/base/message-service/Makefile`,在文件末尾追加:

```makefile

## build-client: Build the test client binary
build-client:
	go build -o bin/msgclient ./cmd/testclient/
```

同时把 `.PHONY` 行(第 1 行)补上 `build-client`:

修改 Makefile 第 1 行从:
```makefile
.PHONY: all build test lint generate migrate fmt vet tidy run proto
```
改为:
```makefile
.PHONY: all build test lint generate migrate fmt vet tidy run proto build-client
```

- [ ] **Step 2: 验证 make target 工作**

Run: `cd /Users/moss/code/base/message-service && make build-client && ls -la bin/msgclient`
Expected: 二进制生成成功

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/message-service
git add Makefile
git commit -m "feat(make): add build-client target for testclient"
```

---

## Task 19: 端到端 smoke-test 验证(grpc + http)

**项目:** message-service

这个 task 是手动验证,确保 docker 部署的服务能被 gRPC 和 HTTP 客户端正常调用。

- [ ] **Step 1: 确保 docker 服务在跑**

如果 Task 11 之后已经 down 了,重新起:

Run:
```bash
cd /Users/moss/code/base/message-service
[ -f .env ] || cp .env.example .env
docker compose up --build -d
docker compose ps
```
Expected: postgres 和 message-service 都 Up & Healthy

- [ ] **Step 2: 等服务完全启动**

Run: `cd /Users/moss/code/base/message-service && for i in $(seq 1 10); do curl -sf -o /dev/null http://localhost:8080/v1/emails && break; sleep 1; done && echo "ready"`
Expected: 看到 `ready`

- [ ] **Step 3: gRPC smoke-test**

Run: `cd /Users/moss/code/base/message-service && ./bin/msgclient -mode grpc -sender smoke smoke-test`
Expected: 看到 `[smoke] SendEmail ok`、`[smoke] GetEmail(...) ok`、`[smoke] ListEmails ok`、`[smoke] DONE`(SMS 部分可能因无凭据返回错误,这是预期)

- [ ] **Step 4: HTTP smoke-test**

Run: `cd /Users/moss/code/base/message-service && ./bin/msgclient -mode http -sender smoke smoke-test`
Expected: 同上,验证 grpc-gateway 路径

- [ ] **Step 5: 手测单条 email(grpc)**

Run:
```bash
cd /Users/moss/code/base/message-service
./bin/msgclient -mode grpc send-email \
  --to=test@example.com \
  --subject="manual test" \
  --body="hello from grpc" \
  --sender=admin
```
Expected: 看到 `sent. id=NNN status=SENT`(或 PENDING)

- [ ] **Step 6: 手测单条 email(http)**

Run:
```bash
cd /Users/moss/code/base/message-service
./bin/msgclient -mode http send-email \
  --to=test@example.com \
  --subject="manual test http" \
  --body="hello from http" \
  --sender=admin
```
Expected: 看到 `sent. id=NNN status=SENT`

- [ ] **Step 7: 列表查询(grpc + http)**

Run:
```bash
cd /Users/moss/code/base/message-service
echo "--- grpc ---"
./bin/msgclient -mode grpc list-emails --page-size 5
echo "--- http ---"
./bin/msgclient -mode http list-emails --page-size 5
```
Expected: 两种模式都返回非零记录数(至少包含 Step 5/6 发的两条)

- [ ] **Step 8: 清理**

Run: `cd /Users/moss/code/base/message-service && docker compose down -v`
Expected: 容器和 volume 都清除

- [ ] **Step 9: 此 task 不 commit**(纯验证)

---

## Task 20: 最终验收 + 文档同步

**项目:** message-service

- [ ] **Step 1: 全项目编译 + 测试**

Run:
```bash
cd /Users/moss/code/base/message-service
go build ./...
go test ./... -race
```
Expected: 编译通过,所有测试 PASS

- [ ] **Step 2: gofmt + golangci-lint**

Run:
```bash
cd /Users/moss/code/base/message-service
gofmt -w .
goimports -w .
golangci-lint run ./...
```
Expected: 无 issue

- [ ] **Step 3: 更新 message-service/CLAUDE.md(可选)**

如果 CLAUDE.md 里有"目录结构"章节,补一行 `cmd/testclient/` 说明。如果没有明确位置,跳过。

- [ ] **Step 4: 把 Obsidian plan 文档同步过去**

Obsidian 端的 plan 文档(对应本文件)需要同步到 `services/message-service/plan/v4/docker-deploy-and-test-client.md`。

执行:
```bash
cp docs/superpowers/plans/2026-06-24-docker-deploy-and-test-client.md \
   "/Users/moss/Library/Mobile Documents/iCloud~md~obsidian/Documents/only/services/message-service/plan/v4/docker-deploy-and-test-client.md"

# 创建 plan/v4 目录(如果不存在)
VAULT="/Users/moss/Library/Mobile Documents/iCloud~md~obsidian/Documents/only"
mkdir -p "$VAULT/services/message-service/plan/v4"
cp docs/superpowers/plans/2026-06-24-docker-deploy-and-test-client.md \
   "$VAULT/services/message-service/plan/v4/docker-deploy-and-test-client.md"
```

更新索引:
```bash
obsidian vault=only append file="services/message-service/index" content="
## 实现计划（v4）

| 文档 | 说明 |
|------|------|
| [[services/message-service/plan/v4/docker-deploy-and-test-client\|Docker 部署 + 测试客户端实施计划]] | 20 个 task,4 个 phase:go-common/configx ExpandEnv → message-service 启用 → Docker 化 → CLI 客户端 |
"

obsidian vault=only append file="services/message-service/changes" content="
- 2026-06-24: 新增 services/message-service/plan/v4/docker-deploy-and-test-client.md — Docker 部署 + 测试客户端实施计划
"
```

- [ ] **Step 5: 检查 message-service git status 干净**

Run: `cd /Users/moss/code/base/message-service && git status`
Expected: clean(所有改动已 commit),或者只有 .env(被 gitignore 排除,不会出现)

- [ ] **Step 6: 检查 go-common git status**

Run: `cd /Users/moss/code/base/go-common && git log --oneline -5 && git status`
Expected: 最近 3 个 commit 是 Phase 1 的 Task 1/2/3,working tree clean

---

# Self-Review Checklist(实施完成后请逐条核对)

**Spec 覆盖:**
- [x] Dockerfile 改造(alpine + migrate + entrypoint) — Task 9
- [x] docker-compose 含 postgres — Task 10
- [x] `.env` 加载(env_file) — Task 10
- [x] yaml + `${VAR}` 展开 — Task 1-3(go-common)+ Task 5-6(message-service)
- [x] `.env.example` — Task 7
- [x] 测试客户端 10 个 RPC + smoke-test — Task 12-17
- [x] 双协议(grpc/http)覆盖 — Task 13/14
- [x] Makefile build-client — Task 18
- [x] 端到端验证 — Task 11、Task 19

**Placeholder 扫描:** 无 TBD/TODO,所有代码块完整。

**类型一致性:**
- `Caller` 接口在 client.go 定义,grpc_client.go / http_client.go 实现一致(Task 12/13/14)
- `runSendEmail` 等子命令函数签名统一:`(ctx context.Context, c Caller, args []string) error`(Task 15/16/17)
- `printJSON` 在 main.go 定义,email.go / sms.go 调用(Task 15、16、17)
- `parseEmailVendor` / `parseSmsVendor` 等辅助函数命名一致

**已知风险点:**
1. go-common 是 replace 本地路径,message-service commit 时不会自动带 go-common 的 commit。**go-common 仓库需要单独 push**(本次范围只做本地 commit)
2. `.env` 不会被 git 跟踪(.gitignore 已配置),用户每次部署需自己 cp .env.example .env
3. SMS smoke-test 默认会因为无 vendor 凭据返回错误,这是预期,不影响 plumbing 验证
