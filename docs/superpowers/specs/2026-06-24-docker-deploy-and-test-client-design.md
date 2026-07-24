# Docker 部署 + 测试客户端设计

- 日期：2026-06-24
- 范围：Dockerfile 改造、docker-compose 部署、env-driven 配置、测试客户端 CLI（gRPC + HTTP 双协议）
- 状态：已对齐，待 plan

## 背景

message-service 核心功能（Send/List/Get/Stats for Email/SMS）已开发完成，需要：

1. **容易打包成 Docker 镜像并启动**：当前 `Dockerfile` 只构建 `server`，不构建 `migrate`；runtime 用 distroless/static 无 shell，无法在启动时自动跑 migrate
2. **配置文件通过环境变量注入**：希望 yaml 里所有字段都用 `${MESSAGE_SERVICE_*}` 引用，所有值由 env 提供。configx 当前只支持字段级 env 覆盖（`AutomaticEnv`），不支持 yaml 内 `${VAR}` 展开 —— 需要扩展 configx 加 `WithExpandEnv()` option
3. **docker 加载环境变量文件启动**：用 `.env` 文件 + docker-compose `env_file` 实现
4. **测试客户端**：覆盖 10 个 RPC，同时验证 gRPC 与 HTTP（grpc-gateway）两条路径

## 设计决策

| # | 决策点 | 选择 | 理由 |
|---|--------|------|------|
| 1 | migrate 处理 | 同一镜像 + entrypoint.sh 先 migrate 再 exec server | 用户选项；一行 `docker compose up` 即可起一套能用的服务 |
| 2 | runtime base image | `alpine:3.20` | distroless/static 无 shell，跑不了 entrypoint.sh；alpine ~15MB，行业惯例 |
| 3 | 部署形态 | docker-compose 含 postgres | 用户选项；一行命令起完整环境，方便联调 |
| 4 | postgres 版本 | `postgres:17-alpine` | 用户选项；最新稳定版 |
| 5 | redis 容器 | 不加 | 当前代码无 redis 依赖 |
| 6 | 配置策略 | yaml 内所有字段值用 `${MESSAGE_SERVICE_*}` 引用，env 提供实际值 | 用户选项；配置文件 = 完整配置清单，所有值强制走 env 注入 |
| 7 | `${VAR}` 展开实现 | 给 go-common/configx 加 `WithExpandEnv()` option | 通用能力，所有 base/ 服务受益；go-common 本地仓库可改 |
| 8 | 配置文件本地/容器区分 | 保留 `config.yaml`（默认值，本地开发用）+ 新增 `config.docker.yaml`（`${VAR}` 引用，镜像用）；Dockerfile 把后者 COPY 成容器内 `config.yaml` | 本地零改动；镜像内 config.yaml 即 ${VAR} 版本，加载即从 env 取值 |
| 9 | `.env` 加载方式 | docker-compose `env_file: .env` | 用户选项；标准做法 |
| 10 | 客户端形态 | CLI 工具 `cmd/testclient/` | 用户选项；可重复手测 |
| 11 | CLI 框架 | 标准库 `flag` + 子命令 dispatch | 项目当前无 cobra；10 个子命令标准库够用，避免新依赖 |
| 12 | 双协议覆盖方式 | 全局 `--mode grpc\|http` flag | 单一开关切换，每个 RPC 子命令根据 mode dispatch |
| 13 | smoke-test 子命令 | 提供 | 一键验证 send→get→list 链路；docker compose 起来后快速冒烟 |
| 14 | 测试客户端是否打镜像 | 不打 | 本地编译，连宿主机暴露的 9000/8080 端口即可 |

## 详细设计

### 1. Dockerfile 改造

builder 阶段同时构建 `server` 与 `migrate`；runtime 阶段切换到 alpine + entrypoint.sh。

```dockerfile
# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /out/migrate ./cmd/migrate

# Runtime stage
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

`entrypoint.sh` 在容器内（项目根目录）：

```sh
#!/bin/sh
set -e
echo "running migrations..."
./migrate
echo "starting server..."
exec ./server
```

`exec` 让 server 接管 PID 1，正确接收 SIGTERM。

### 2. docker-compose.yaml

项目根目录新增 `docker-compose.yaml`：

```yaml
services:
  postgres:
    image: postgres:17-alpine
    env_file: .env
    environment:
      # POSTGRES_DB/USER/PASSWORD 由 env_file 注入或这里显式设置
      - POSTGRES_DB=${MESSAGE_SERVICE_DATABASE_DBNAME:-message_service}
      - POSTGRES_USER=${MESSAGE_SERVICE_DATABASE_USER:-postgres}
      - POSTGRES_PASSWORD=${MESSAGE_SERVICE_DATABASE_PASSWORD:-postgres}
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 3s
      timeout: 3s
      retries: 20

  message-service:
    build: .
    env_file: .env
    environment:
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

关键点：
- `postgres.healthcheck` + `depends_on.condition: service_healthy` 保证 migrate 不撞上 PG 未就绪
- `MESSAGE_SERVICE_DATABASE_HOST=postgres` 强制覆盖（容器内网 DNS 解析 service name）
- postgres 凭据可从 `.env` 注入，compose 兜底默认值与 config.yaml 默认值一致

### 3. 配置策略

#### `config.docker.yaml`（项目根目录新增）

镜像里用的配置文件，**所有字段值都用 `${MESSAGE_SERVICE_*}` 引用**，提供完整配置清单：

```yaml
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

Dockerfile 构建阶段 `COPY config.docker.yaml ./config.yaml`（容器内文件名仍是 `config.yaml`，configx 默认就能找到）。

#### `.env.example`（项目根目录新增）

提供所有环境变量的实际值。敏感字段示例值留空：

```bash
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

# ---- Email (custom SMTP) ----
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_NAME=default
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_HOST=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PORT=587
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_USERNAME=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_PASSWORD=
MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_FROM=

# ---- SMS (Aliyun) ----
MESSAGE_SERVICE_SMS_DEFAULT_COUNTRY=CN
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_NAME=default
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_ID=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_ACCESS_KEY_SECRET=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_SIGN_NAME=
MESSAGE_SERVICE_SMS_VENDORS_ALIYUN_ACCOUNTS_0_REGION_ID=cn-hangzhou

# ---- Cron ----
MESSAGE_SERVICE_CRON_TIMEZONE=Asia/Shanghai
MESSAGE_SERVICE_CRON_OVERLAP_POLICY=skip

# ---- Snowflake ----
MESSAGE_SERVICE_THIRD_PARTY_GID_MODE=module
MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_MACHINE_ID=1
MESSAGE_SERVICE_THIRD_PARTY_GID_SNOWFLAKE_START_TIME=2026-06-01T00:00:00Z
```

使用流程：
```bash
cp .env.example .env
# 编辑 .env，填入实际值（至少 db password、可选 vendor 凭据）
docker compose up --build
```

#### go-common/configx 改动：`WithExpandEnv()` option

新增 option，启用后 `configx.Load` 在 `ReadInConfig` 之后对 `viper.AllSettings()` 返回的 map 递归做 `os.ExpandEnv`，再 `MergeConfigMap` 回去：

```go
// 在 configx/configx.go 的 Load 函数中插入：
if l.expandEnv {
    settings := v.AllSettings()
    expandStringsInMap(settings)  // 递归遍历，对所有 string 值做 os.ExpandEnv
    if err := v.MergeConfigMap(settings); err != nil {
        return ErrReadConfig.Wrap(err)
    }
}
```

- `expandStringsInMap` 处理嵌套 `map[string]any`、`[]any`、`string` 三种类型
- 不修改非字符串类型（int/bool 直接由 viper decode hook 处理 ${VAR} 替换后的字符串）

#### configx env var 路径规则

- 字段路径驼峰转 snake_case，前缀 `MESSAGE_SERVICE_`
- 例如 `Database.Host` → `MESSAGE_SERVICE_DATABASE_HOST`
- 数组下标：`Email.Vendors["custom_smtp"].Accounts[0].Host` → `MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_ACCOUNTS_0_HOST`
- map key 在路径中保持原样（vendor 名字小写：`aliyun`、`custom_smtp`）

#### 配置文件本地/容器区分

- `config.yaml`：**保持不变**，保留面向本地开发的默认值
- `config.docker.yaml`：新增，`${VAR}` 引用版，仅镜像用
- 本地开发零改动；docker 用户改 `.env` 即可

### 4. 测试客户端

#### 目录结构

```
cmd/testclient/
├── main.go              # 入口、全局 flag、子命令 dispatch
├── client.go            # Caller 接口，grpc/http 各自实现
├── grpc_client.go       # 复用 pkg.Client
├── http_client.go       # net/http + encoding/json
├── email.go             # 5 个 email 子命令
├── sms.go               # 5 个 sms 子命令
└── smoke.go             # smoke-test 命令
```

#### 全局 flag

```
--mode=grpc|http       默认 grpc
--target=localhost:9000  (grpc)
--base=http://localhost:8080  (http)
--sender=<int64>       sender_id（多数 RPC 必填）
```

#### 子命令清单（10 + 1）

| 子命令 | 对应 RPC | 必填 flag |
|--------|----------|-----------|
| `send-email` | SendEmail | `--to`, `--subject`, `--body`, `--vendor` |
| `get-email` | GetEmail | `--id` |
| `list-emails` | ListEmails | （可选 `--page-size`, `--page`） |
| `list-emails-by-cursor` | ListEmailsByCursor | （可选 `--page-size`, `--cursor`） |
| `get-email-stats` | GetEmailStats | （时间范围） |
| `send-sms` | SendSMS | `--phone`, `--content`, `--vendor` |
| `get-sms` | GetSMS | `--id` |
| `list-sms` | ListSMS | 同上 |
| `list-sms-by-cursor` | ListSMSByCursor | 同上 |
| `get-sms-stats` | GetSMSStats | 同上 |
| `smoke-test` | Send+Get+List 各 2 | 仅 vendor 凭据 |

#### Caller 接口

```go
type Caller interface {
    SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error)
    GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error)
    // ... 10 个 RPC
}
```

- `grpcClient` 包装 `*messageservice.Client`，直接调 RPC
- `httpClient` 用 `net/http` POST 到 grpc-gateway 路径（如 `POST /v1/emails:send`）
- proto 消息体在 HTTP 模式下 marshal 成 JSON

#### 子命令 dispatch

每个子命令函数签名：
```go
func runSendEmail(ctx context.Context, c Caller, fs *flag.FlagSet)
```

main.go 解析第一个位置参数为子命令名，剩下的传给 `flag.NewFlagSet`；根据全局 `--mode` 构造 Caller，调对应函数。

#### HTTP 路径

proto 已有完整 `google.api.http` 注解，10 个 RPC 对应的 REST endpoint：

| 子命令 | HTTP 方法 + 路径 |
|--------|------------------|
| `send-email` | `POST /v1/messages:email` |
| `send-sms` | `POST /v1/messages:sms` |
| `get-email` | `GET /v1/emails/{id}` |
| `list-emails` | `GET /v1/emails` |
| `list-emails-by-cursor` | `GET /v1/emails:cursor` |
| `get-email-stats` | `GET /v1/emails:stats` |
| `get-sms` | `GET /v1/sms/{id}` |
| `list-sms` | `GET /v1/sms` |
| `list-sms-by-cursor` | `GET /v1/sms:cursor` |
| `get-sms-stats` | `GET /v1/sms:stats` |

`pb.RegisterMessageServiceHandlerFromEndpoint` 已注册 gateway，无需改 proto 或生成代码。HTTP 模式下请求体用 proto JSON 序列化（`encoding/json` 即可，proto 字段都是 JSON-friendly 类型）。

#### Makefile 入口

新增独立 target，**不并入 `build`**（`build` 仍只构 server + migrate 用于镜像；testclient 是本地工具）：

```makefile
build-client:
	go build -o bin/msgclient ./cmd/testclient/
```

### 5. .gitignore 调整

新增：
```
# Local env file (keep .env.example committed)
.env
```

## 文件清单

**message-service 新增**：
- `entrypoint.sh`
- `docker-compose.yaml`
- `.env.example`
- `config.docker.yaml`（${VAR} 引用版配置，镜像内重命名为 config.yaml）
- `cmd/testclient/main.go`
- `cmd/testclient/client.go`
- `cmd/testclient/grpc_client.go`
- `cmd/testclient/http_client.go`
- `cmd/testclient/email.go`
- `cmd/testclient/sms.go`
- `cmd/testclient/smoke.go`

**message-service 修改**：
- `Dockerfile`：加 migrate 构建、换 alpine、COPY config.docker.yaml → config.yaml、加 entrypoint
- `.gitignore`：加 `.env`
- `pkg/config/config.go`：`Load()` 调用 `configx.WithExpandEnv()`

**go-common 修改**（跨项目）：
- `configx/options.go`：新增 `WithExpandEnv()` option 和 loader 字段
- `configx/configx.go`：Load 中 ReadInConfig 后做 `${VAR}` 展开（递归遍历 AllSettings，对 string 值 `os.ExpandEnv`，MergeConfigMap 回写）
- `configx/configx_test.go`：覆盖 `${VAR}` 展开 + 未设变量保留原值 + 嵌套 map/slice 等场景

**不修改**：
- `config.yaml`（保持本地开发默认值，本地用户零感知）

## 验证计划

1. `cp .env.example .env`，填入测试用的 db 密码
2. `docker compose up --build` —— postgres 就绪后 message-service 自动 migrate + 启动
3. `make build-client && ./bin/msgclient smoke-test` —— 跑一轮 send+get+list（grpc + http 各一次）
4. 手测：
   - `./bin/msgclient --mode=grpc send-email --to=... --subject=...`
   - `./bin/msgclient --mode=http send-email --to=... --subject=...`
   - `./bin/msgclient --mode=grpc list-emails`
5. `docker compose down -v` 清理

## 关联

**实现计划：** 待生成（writing-plans 阶段产出 [[services/message-service/plan/v4/docker-deploy-and-test-client|docker-deploy-and-test-client]]）
