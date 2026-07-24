# Service 架构层重构 — 设计文档

**日期**: 2026-06-21
**主题**: 根据 `.claude/skills/golang-service-development` 重构 message-service 的架构层(handler/service 分层、子包化、lifecycle.Manager 资源管理、HTTP gateway、DevOps 文件)

## 1. 背景与目标

`golang-service-development` skill 定义了团队所有 `-service` 项目的目录布局、分层规则、资源管理模式。message-service 之前已经做过 skills-alignment 重构(对齐 golang-development / gorm-cli-development / go-common-usage / proto-development 4 个 skill),但**架构层**仍有结构性差距。

本次重构目标:**全面对齐 golang-service-development skill,包括 DevOps 文件**。

### 非目标

- 不引入新业务功能
- 不改消息发送/路由逻辑
- 不重构 `internal/message/{email,sms}` 的 vendor registry(那是 vendor 抽象层,不是 service 业务)
- 不重构 `internal/store/`(上次已对齐 gorm-cli-development)
- 不改 proto 的字段编号/类型(wire-safe only)

## 2. 差距总览

### 主要差距(结构性)

| 维度 | 当前 | skill 要求 |
|---|---|---|
| **handler 层** | 不存在;`MessageService` 同时是 gRPC stub + 业务 + helper | `pkg/handler/` 薄壳,每个 RPC 一行委托 |
| **service 分层** | 单层:`service.go` + `send.go` + `query.go`,业务挂在 `MessageService` 上 | 两层:`service.go`(本体+facade) + `internal/service/<domain>/`(子包业务) |
| **资源管理** | `models.Database{ DB, owned bool }` + service.New 手工 Stop | `lifecycle.Manager` + `lifecycle.StopFunc` 统一注册,删 `owned bool` |
| **pkg/server.go** | 只持 `*service.MessageService` | 持 `*service.Service` + `*handler.Handler` |
| **pkg/module.go** | 返回 `*service.MessageService` | 返回 `*handler.Handler`(对外只暴露 handler) |
| **HTTP gateway** | `grpcx.New` 第 3 个参数为 nil | 启用 gateway(传 `RegisterXxxHandlerFromEndpoint`) |

### 次要差距

| 维度 | 当前 | skill 要求 |
|---|---|---|
| Makefile | 缺 `migrate` target | 必须提供 `migrate` |
| Dockerfile | 不存在 | 多阶段构建,distroless 静态镜像,CGO=0,ldflags `-s -w` |
| `.golangci.yml` | 不存在 | 复用 ai-kit-studio 模板,`local-prefixes` 配本服务 module |
| proto HTTP annotation | 不存在 | 加 `google.api.http` 启用 HTTP gateway(wire-safe) |

### 已经符合

- ✅ `pkg/{config,option,thirdcall,xcodes,client.go,server.go,module.go}` 布局
- ✅ `internal/{thirdcall,store/{models,generated,dal}}` 布局
- ✅ proto enum → DB int32 转换
- ✅ Option + lifecycle.Manager 骨架(只缺 StopFunc 注册方式)
- ✅ buf v2 + managed mode + protovalidate
- ✅ cmd/{server,migrate}/main.go
- ✅ service.New 失败回滚路径
- ✅ xcodes 按域分文件

## 3. 设计

### 3.1 目录结构变化

```
message-service/
├── pkg/
│   ├── handler/                        ★ 新增
│   │   └── message.go                  # Handler struct + 5 个 RPC 一行委托
│   ├── config/  option/  thirdcall/  xcodes/  client.go  ✓ 不变(除 config 加 Cron 字段)
│   ├── server.go                       # 改:持 *handler.Handler(通过 grpcSrv),启用 gateway
│   └── module.go                       # 改:NewModule 返回 *Handler
├── internal/
│   ├── service/
│   │   ├── service.go                  # 改:Service 本体 + New + Start/Stop + setupJobs + facade
│   │   └── message/                    ★ 新增子包
│   │       └── message.go              # 业务方法 + persistXxx + toProtoRecord + vendor helper
│   ├── store/{models,generated,dal}    ✓ 不变(除 models/base.go 删 Database wrapper)
│   ├── thirdcall/gid_service/          ✓ 不变
│   ├── jobs/                           ★ 新增(skill §5)
│   │   └── jobs.go                     # jobs.Scheduler 实现 lifecycle.Service
│   └── provider/                       ★ 改名自 internal/message/
│       ├── email/                      # 邮件 provider(SMTP/Aliyun SDK 封装)
│       └── sms/                        # 短信 provider(Aliyun SDK 封装)
├── cmd/{server,migrate}/               ✓ 不变
├── api/proto/message/v1/message.proto  # 改:加 google.api.http annotation
├── Makefile                            # 改:加 migrate target
├── Dockerfile                          ★ 新增
└── .golangci.yml                       ★ 新增
```

**删除**:
- `internal/service/send.go`(业务迁到 `internal/service/message/message.go`)
- `internal/service/query.go`(同上)
- `internal/store/models/base.go` 的 `Database` 类型(改用 `lifecycle.StopFunc`)

**改名**:
- `internal/message/` → `internal/provider/`(vendor 协议封装挪到 skill §1 标准目录)

**关键概念区分**:
- `internal/provider/` = **协议封装**(SMTP/Aliyun SDK 调用层,AccountRegistry/Sender/SendResult)
- `internal/service/message/` = **业务逻辑**(调用 provider 完成发送)
- `internal/jobs/` = **调度器**(cron wrapper,空架子,无 cron 任务时也保留)

### 3.2 pkg/handler/message.go

handler 是 proto service 的薄壳:每个 RPC 一行委托,不写业务,不做协议转换。

```go
package handler

import (
    "context"

    pb "message-service/gen/message/v1"
    "message-service/internal/service"
)

// Handler implements pb.MessageServiceServer as a thin delegate to service.Service.
// It also satisfies signalx.Service by forwarding Start/Stop, so in-process module
// callers can manage lifecycle on the same object they call RPCs on.
type Handler struct {
    pb.UnimplementedMessageServiceServer
    svc *service.Service
}

// New creates a Handler that delegates to svc.
func New(svc *service.Service) *Handler { return &Handler{svc: svc} }

// Start forwards to the underlying service.
func (h *Handler) Start() error { return h.svc.Start() }

// Stop forwards to the underlying service.
func (h *Handler) Stop() error { return h.svc.Stop() }

// SendEmail sends an email via the configured vendor/account.
func (h *Handler) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
    return h.svc.SendEmail(ctx, req)
}

// SendSMS sends an SMS via the configured vendor/account or by phone country code.
func (h *Handler) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
    return h.svc.SendSMS(ctx, req)
}

// GetMessage returns a single message record by ID.
func (h *Handler) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
    return h.svc.GetMessage(ctx, req)
}

// ListMessages returns a paginated list of message records matching the filter.
func (h *Handler) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
    return h.svc.ListMessages(ctx, req)
}

// GetMessageStats returns aggregated statistics for messages matching the filter.
func (h *Handler) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
    return h.svc.GetMessageStats(ctx, req)
}
```

### 3.3 internal/service/ 分层

**`internal/service/service.go`** — 本体 + facade:

```go
package service

import (
    // ...
    "message-service/internal/service/message"
)

// Service is the application's service root. It owns resources (db, gid,
// registries) and exposes one facade method per RPC that delegates to the
// message domain subpackage.
type Service struct {
    cfg *config.Config
    mgr *lifecycle.Manager

    db  *gorm.DB
    gid thirdcall.GIDService

    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router

    message *message.Service  // domain subpackage
}

func New(cfg *config.Config, opts ...option.Option) (*Service, error) {
    o := option.Apply(opts...)
    mgr := lifecycle.NewManager()

    db, err := resolveDB(cfg, o.DB, mgr)
    if err != nil { return nil, errors.Join(err, mgr.Stop()) }
    gid, err := resolveGID(cfg, o.GIDService, mgr)
    if err != nil { return nil, errors.Join(err, mgr.Stop()) }

    emailRegistry, err := email.NewAccountRegistry(cfg.Email)
    if err != nil { return nil, errors.Join(fmt.Errorf("email registry: %w", err), mgr.Stop()) }
    smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
    if err != nil { return nil, errors.Join(fmt.Errorf("sms registry: %w", err), mgr.Stop()) }
    smsRouter, err := sms.BuildRouter(cfg.SMS, smsRegistry)
    if err != nil { return nil, errors.Join(fmt.Errorf("sms router: %w", err), mgr.Stop()) }

    return &Service{
        cfg: cfg, mgr: mgr,
        db: db, gid: gid,
        emailRegistry: emailRegistry,
        smsRegistry:   smsRegistry,
        smsRouter:     smsRouter,
        message:       message.New(db, gid, emailRegistry, smsRegistry, smsRouter),
    }, nil
}

// Start starts all lifecycle-registered components (close-only resources are no-op).
func (s *Service) Start() error { return s.mgr.Start() }

// Stop stops all lifecycle-registered components in reverse order.
func (s *Service) Stop() error { return s.mgr.Stop() }

// --- RPC facades (one line each) ---

func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
    return s.message.SendEmail(ctx, req)
}
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
    return s.message.SendSMS(ctx, req)
}
func (s *Service) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
    return s.message.GetMessage(ctx, req)
}
func (s *Service) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
    return s.message.ListMessages(ctx, req)
}
func (s *Service) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
    return s.message.GetMessageStats(ctx, req)
}
```

**`internal/service/message/message.go`** — 子包业务实现:

```go
package message

import (
    "context"
    "database/sql"
    "log/slog"
    "time"

    pb "message-service/gen/message/v1"
    "message-service/internal/message/email"
    "message-service/internal/message/sms"
    "message-service/internal/store/dal"
    "message-service/internal/store/models"
    "message-service/pkg/xcodes"

    emailcommon "github.com/servekit/go-common/message/email"
    smscommon "github.com/servekit/go-common/message/sms"
    "gorm.io/gorm"
)

// Service implements the message domain business logic. Resources (db, gid,
// vendor registries) are injected via New; the parent service.Service owns
// their lifecycle.
type Service struct {
    db            *gorm.DB
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router
}

func New(db *gorm.DB, gid thirdcall.GIDService, emailReg *email.AccountRegistry, smsReg *sms.AccountRegistry, smsRouter *sms.Router) *Service {
    return &Service{db: db, gid: gid, emailRegistry: emailReg, smsRegistry: smsReg, smsRouter: smsRouter}
}

// SendEmail / SendSMS / GetMessage / ListMessages / GetMessageStats
// (从当前 send.go / query.go 迁移,签名不变,只改 receiver 从 *MessageService 到 *Service)

// persistEmailRecord / persistSMSRecord  (从 send.go 迁移)
// toProtoRecord                          (从 service.go 迁移)
// emailVendorToString / smsVendorToString / 反向 helper  (从 send.go 迁移)
```

**关键不变量**:
- 子包 `Service` 不持有父 `*service.Service` 引用(避免循环依赖)
- 子包不直接调 `dal.X(ctx, ..., s.db, ...)` 之外的 store API(只通过 dal)
- 子包不知道 lifecycle(资源由父管)
- 子包方法签名跟 proto RPC 一一对应(直接吃 proto 类型)

### 3.4 资源管理重构(删 ownDB bool)

**删除 `models.Database` wrapper**。资源生命周期由 `lifecycle.Manager` 统一管理。

**改造点**:

```go
// internal/service/service.go

func resolveDB(cfg *config.Config, injected *gorm.DB, mgr *lifecycle.Manager) (*gorm.DB, error) {
    if injected != nil {
        return injected, nil  // 调用方注入,不注册到 mgr
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

**约束**:
- `internal/thirdcall/gid_service/grpc.go` 的 `grpcGID` 已经实现了 `Close()`,可直接被注册
- `internal/thirdcall/gid_service/module.go` 的 `moduleGID` 没有 Close(无需清理),不注册
- 删 `models.Database` / `NewDatabase` / `(*Database).Stop` 整个类型
- `cmd/migrate/main.go` 不受影响(它独立 `dbx.New`,不需要 lifecycle)
- `models.AllModels()` 保留(还在 base.go 里)

**slog 在 cleanup path 是允许的例外**:全局规则是库代码不打日志,但 cleanup path 的 Stop 失败只能用日志(skill §5 明示)。

### 3.4a jobs.Scheduler 接入(skill §5)

按 skill §5 要求,每个 `-service` 项目都有 `internal/jobs/` 包(空架子也保留),`service.New` 末尾调 `setupJobs()` 注册到 `lifecycle.Manager`。

**`internal/jobs/jobs.go`**(直接拷贝 `demo-service/internal/jobs/jobs.go`,无业务逻辑改动):

```go
// Package jobs owns the cron scheduler for periodic background work.
package jobs

import (
    "fmt"

    "github.com/servekit/go-common/cronx"
    "github.com/servekit/go-common/lifecycle"
    "github.com/robfig/cron/v3"
)

// Scheduler wraps a cron.Cron and adapts it to lifecycle.Service.
type Scheduler struct {
    cron     *cron.Cron
    ownsCron bool
}

type Deps struct {
    Config *cronx.Config
    Cron   *cron.Cron
}

var _ lifecycle.Service = (*Scheduler)(nil)

func New(d *Deps) (*Scheduler, error) { /* see demo-service */ }
func (s *Scheduler) AddFunc(spec string, cmd func()) error { /* see demo-service */ }
func (s *Scheduler) Start() error { /* no-op unless ownsCron */ }
func (s *Scheduler) Stop() error { /* no-op unless ownsCron */ }
```

**`internal/service/service.go` 加 setupJobs**:

```go
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

`New()` 末尾调一次 `setupJobs()`,失败时 `mgr.Stop()` 回滚已注册资源。

**`pkg/config/config.go` 加 `Cron *CronConfig` 字段**:

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

**何时不用 cron 任务**:`setupJobs` 内不加 `AddFunc` 即可。包结构仍保留(skill 推荐),为未来加 cron 任务留好位置。

### 3.5 pkg/server.go 和 pkg/module.go

**`pkg/server.go`** — Server 同时持 service + handler:

```go
type Server struct {
    grpcSrv *grpcx.Server
    svc     *service.Service
    hdl     *handler.Handler
}

func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
    var o serverOptions
    for _, opt := range opts { opt(&o) }

    svc, err := service.New(cfg, o.serviceOpts...)
    if err != nil { return nil, err }

    hdl := handler.New(svc)

    validator, err := protovalidate.New()
    if err != nil { return nil, err }

    grpcSrv := grpcx.New(
        grpcx.ServerConfig{GRPCAddr: cfg.Server.GRPCAddr, GatewayAddr: cfg.Server.HTTPAddr},
        func(s *grpc.Server) { pb.RegisterMessageServiceServer(s, hdl) },
        pb.RegisterMessageServiceHandlerFromEndpoint,  // 启用 HTTP gateway
        grpcx.ErrorInterceptor,
        protovalidate_middleware.UnaryServerInterceptor(validator),
    )

    return &Server{grpcSrv: grpcSrv, svc: svc, hdl: hdl}, nil
}

func (s *Server) Start() error {
    if err := s.svc.Start(); err != nil { return err }
    if err := s.grpcSrv.Start(); err != nil { return errors.Join(err, s.svc.Stop()) }
    return nil
}

func (s *Server) Stop() error { return errors.Join(s.grpcSrv.Stop(), s.svc.Stop()) }
```

**`pkg/module.go`** — 返回 `*handler.Handler`:

```go
// NewModule creates an in-process Handler with the given config and service options.
// The returned Handler implements signalx.Service: callers should defer Stop() to
// release owned resources. RPC methods are called directly on the Handler.
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
    svc, err := service.New(cfg, opts...)
    if err != nil { return nil, err }
    return handler.New(svc), nil
}
```

**注意**:这是 breaking change。当前 `pkg/module.go` 通过 type alias 暴露 `service.MessageService`。重构后调用方拿到的是 `*handler.Handler`,需要相应调整。当前仓库内没有外部 in-process 调用方(gid-service 用 gRPC),影响可控。

### 3.6 HTTP gateway proto annotation

加 `google.api.http` annotation 启用 HTTP gateway(wire-safe:annotation 不影响二进制 wire format,只声明 HTTP 路径映射)。

```proto
syntax = "proto3";

package message.v1;

import "buf/validate/validate.proto";
import "google/api/annotations.proto";  // 新增

// ... enums unchanged ...

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

// ... messages unchanged ...
```

**HTTP 路径选择理由**:
- REST 资源风格:`/v1/messages` 是集合,`/v1/messages/{id}` 是单条
- `:email` / `:sms` / `:stats` 用 Google AIP custom method 风格(冒号分隔动词),避免跟资源路径冲突
- Send 用 POST + `body: "*"`,所有字段从 body 读

**buf generate 后**:gateway 代码自动生成,gRPC stub 不变。gen/ diff 会增加 `message.pb.gw.go`(gateway 文件)。

### 3.7 DevOps 文件

**Makefile 加 `migrate` target**:

```make
## migrate: Run database migrations (AutoMigrate)
migrate:
	go run ./cmd/migrate/
```

**Dockerfile**(多阶段 + distroless):

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

**`.golangci.yml`** — 复用 `ai-kit-studio/skills/golang-development/.golangci.yml`,`local-prefixes` 改为 `message-service,github.com/servekit/go-common`:

```yaml
# 拷贝 ai-kit-studio/skills/golang-development/.golangci.yml 内容
# 改 local-prefixes 部分(如果有):
linters-settings:
  # ... 其他保持原样 ...
```

(具体内容从模板拷贝,只改 module 名相关的字段)

### 3.8 测试调整

**`internal/service/service_test.go`** → 拆分:

- **`internal/service/message/message_test.go`** — 子包业务测试
  - 沿用现有测试逻辑,但 receiver 从 `*MessageService` 改为 `*message.Service`
  - setup helper 改成构造 `message.New(db, gid, emailReg, smsReg, smsRouter)`
  - 5 类测试:Send success/fail/fallback、vendor+account selection、query、stats

- **`internal/service/service_test.go`** — 留最小化或删除
  - 如果有跨域编排(目前没有),才保留 service 层测试
  - 否则删除(子包测试覆盖足够)

- **`pkg/handler/handler_test.go`**(可选) — 测试 Handler 正确委托
  - 表驱动测试,断言每个 RPC 都委托到 svc

### 3.9 memory 更新

| 当前 memory | 操作 |
|---|---|
| `service-dal-package-level-functions` | **补充**:加 handler/service 分层规则、子包结构 |
| `tests-prefer-real-db-over-mocks` | **保留**(继续用 dbx.SetupTestDB) |
| `avoid-empty-abstraction-base-classes` | **保留**(仍适用) |

新增 memory:
- **`handler-service-layering`** — 记录分层是 golang-service-development skill 的强制约定:handler 是薄壳、service 是本体+facade、领域业务在 `internal/service/<domain>/` 子包

## 4. 风险与缓解

| 风险 | 缓解 |
|---|---|
| service 拆分涉及大量代码移动(send.go/query.go → message/message.go) | 每个 task 独立 commit,失败可回滚 |
| HTTP gateway 启用后 `buf generate` 产生新文件 | gateway 代码是 additive,不影响 gRPC client;`git diff gen/` 验证 |
| `models.Database` 删除影响其他调用方 | grep 确认使用范围;migrate 入口不依赖 Database wrapper |
| `pkg/module.go` 返回类型从 `*service.MessageService` → `*handler.Handler` | 这是 breaking change,但仓库内无外部 in-process 调用方 |
| Handler 添加后 grpc health check 路径变化 | health check 是 grpcx 内置,handler embed `Unimplemented` 不影响 |
| gid_service 的 grpcGID 当前未导出 Close 注册 | 加 `interface{ Close() error }` 类型断言,模块模式无 Close 跳过 |
| 子包循环依赖(message 子包 import internal/message/email,而 email 不 import message 子包) | email 子包不 import service 层,只被 service 层 import,单向 |

## 5. 验证清单

完成后必须全部通过:
- [ ] `gofmt -l .`(除 gen/)无输出
- [ ] `goimports -l .`(除 gen/)无输出
- [ ] `golangci-lint run ./...` 0 issues
- [ ] `go build ./...` 无错
- [ ] `go test -race -count=1 ./internal/message/... ./pkg/xcodes/... ./pkg/handler/...` 全过
- [ ] `go test -race -count=1 ./internal/service/...`(若本地有 Docker)全过
- [ ] `buf lint` 无 violation
- [ ] `buf generate` 后 `git diff gen/` 包含 gateway 文件(`message.pb.gw.go`)
- [ ] `make proto && git diff --exit-code`
- [ ] `make generate && git diff --exit-code`
- [ ] `make migrate` 能跑(若有 PostgreSQL)
- [ ] `docker build .` 成功
- [ ] 每个 RPC 在 `service.go` 都有对应 facade 方法(一一对应)
- [ ] 每个 RPC 在 `pkg/handler/message.go` 都有对应委托方法
- [ ] 业务方法都在 `internal/service/message/` 子包,`internal/service/` 根目录只有 `service.go`
- [ ] grep 确认无 `MessageService` 残留(类型已重命名为 `Service`,handler 单独)
- [ ] grep 确认无 `models.Database` 残留(已删除)

## 6. 实施顺序(供 writing-plans 参考)

1. **改名 `internal/message/` → `internal/provider/`** — 纯目录重命名 + import 路径更新
2. **创建 `internal/jobs/jobs.go`** — 拷贝 demo-service 的 jobs 包
3. **拆 service.go** — 重命名 `MessageService` → `Service`,创建 `internal/service/message/` 子包,业务方法迁移
4. **创建 pkg/handler** — Handler struct + 5 个 stub
5. **改 pkg/server.go + pkg/module.go** — Server 通过 Handler,gateway 暂禁用
6. **资源管理重构** — 删 `models.Database`,resolveDB/resolveGID 用 lifecycle.StopFunc
7. **接入 jobs** — config.go 加 Cron 字段,service.New 加 setupJobs()
8. **proto HTTP annotation** — 加 google.api.http,buf generate,启用 gateway
9. **Makefile migrate + Dockerfile + .golangci.yml**
10. **测试调整** — service_test.go 拆到 message/message_test.go
11. **memory 更新**
12. **全量验证**

每步独立 commit,失败可回滚。

## 7. 关联

**相关历史 spec**:
- `2026-06-20-skills-alignment-refactor-design.md`(dal 重构 + 4 skills 对齐)

**参考**:
- `ai-kit-studio/skills/golang-service-development/golang-service-development.md`
- `ai-kit-studio/skills/golang-service-development/demo-service/`(canonical 实现)

**实现计划**:见 `docs/superpowers/plans/2026-06-21-service-architecture-refactor-plan.md`(待 writing-plans 生成)
