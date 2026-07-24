# go-common/message 瘦身设计

**日期**：2026-06-16
**状态**：设计已确认，待写实施计划
**作者**：moss

## 背景

`go-common/message` 当前承担了完整的消息发送栈：接口定义、Sender（fallback 循环 + hooks）、Router（按号码国家码路由）、AccountRegistry（vendor+account 索引）、各厂商子包（smtp/mailgun/aliyun）。

实际使用情况：

- `go-common/message` 的外部使用者**只有 message-service**（pay-service / user-service / membership-service / storage-service / gid-service 均未 import）。它本质上是 message-service 的"私有库"被放在了公共位置。
- `Router` 在 message-service 中**完全闲置**——`send.go` 没接入，配置文件也没有 `routes` 段。
- `Hook` 机制同样闲置——`service.go` 没 `WithHook(...)`，`send.go` 没注册 hook。
- v1 阶段（已合并）刚把 AccountRegistry 从 message-service 提取到 go-common；现在判断这个提取方向错了——AccountRegistry 依赖业务概念（vendor/account），本就属于 message-service 的领域。

message-service 的 CLAUDE.md 明确分工：它负责"工程化封装：持久化、**供应商配置**、记录查询"。`AccountRegistry`、`Router` 都属于"供应商配置 + 选择策略"，应该回到 message-service。

## 目标

1. **go-common/message 瘦身为纯接口 + 厂商实现**：只保留每个 channel 的 `Provider` 接口、`Message` 类型，以及厂商子包。
2. **message-service 接管所有组合/选择/路由能力**：Sender、SendResult、AccountRegistry、Router 全部搬到 `internal/message/`。
3. **本次同时接入 Router 到 sendSMS**：vendor/account 都空时按手机号国家码路由（取代现有 DefaultSender fallback）。
4. **行为可观察**：落库记录实际使用的 vendor + account。

## 非目标

- 不重构 `Provider` 接口本身（签名 `Name() / Send(ctx, *Message) error` 不变）。
- 不动 email 侧的路由逻辑——email 没有"号码路由"概念，保持 AccountRegistry + Sender 现状。
- 不引入新厂商（如 Twilio）——本次只搬家 + 接入 Router，新厂商是独立工作。
- 不改 gRPC API 协议（proto 文件不变）。

## 切分边界

| 内容 | 现位置 | 重构后 |
|------|--------|--------|
| `Provider` 接口 + `Message` 类型 | `go-common/message/{email,sms}` | **保留在 go-common** |
| 厂商子包 `smtp` / `mailgun` / `aliyun` | `go-common/message/{email,sms}/<vendor>` | **保留在 go-common** |
| `Sender`（fallback 循环） | `go-common/message/{email,sms}/sender.go` | → message-service |
| `SendResult` | 同上 | → message-service |
| `Hook` / `HookFunc` / `WithHook` | 同上 | **删除**（闲置 + YAGNI） |
| `AccountRegistry` | `go-common/message/{email,sms}/registry/` | → message-service |
| `Router` | `go-common/message/sms/router.go` | → message-service |

**为什么 Provider 接口必须留在 go-common**：厂商子包（smtp/mailgun/aliyun）需要实现这个接口，接口和实现必须在同一侧，否则就是循环依赖。

**为什么删 Hook**：当前完全闲置；`persist*Record` 直接消费 `SendResult`，不需要观察者机制。未来要做 metric/审计再加。

## 目录结构

message-service 新增：

```
internal/message/
├── email/
│   ├── sender.go      # Sender + SendResult（从 go-common 搬来，删 Hook）
│   └── registry.go    # AccountRegistry（从 go-common 搬来）
└── sms/
    ├── sender.go      # Sender + SendResult
    ├── registry.go    # AccountRegistry
    └── router.go      # Router（从 go-common 搬来 + 改造接入）
```

`pkg/config/config.go` 中 `Email *emailregistry.Config` / `SMS *smsregistry.Config` 改为引用 `message-service/internal/message/{email,sms}.Config`。

go-common/message 删除：
- `email/sender.go` 中的 `Sender` / `SendResult` / `Hook` / `HookFunc` / `WithHook` / `SenderOption`
- `email/registry/`（整个子包）
- `sms/sender.go` 中同上
- `sms/registry/`（整个子包）
- `sms/router.go`
- `integration_test.go`（测试发送栈组合的，下沉后由 message-service 重新组织测试）

go-common/message 保留：
- `email/sender.go` 仅保留 `Provider` 接口 + `Message` 类型
- `email/smtp/`、`email/mailgun/`
- `sms/sender.go` 仅保留 `Provider` 接口 + `Message` 类型
- `sms/aliyun/`

## Router 接入设计

### 触发方式

`SendSMSRequest` 现有 `vendor` + `account` 字段（CEL 校验"要么都设要么都空"），**不改 proto**：

- `vendor != "" && account != ""` → `registry.SenderFor(vendor, account)`（显式指定，现状不变）
- `vendor == "" && account == ""` → `router.SenderForPhone(req.To)`（**新行为**：按手机号国家码路由，取代原 DefaultSender fallback）

**为什么不改 proto**：Router 的天然定位就是"caller 不知道用谁时的兜底"，正好对应 vendor/account 都空的语义。现有 DefaultSender 的 fallback 行为（试所有 account）在生产里基本无意义——同厂商的多个 account 通常配置同质化，一个失败另一个大概率也失败。

**潜在兼容性风险**：如果存在 caller 依赖"vendor/account 都空 → DefaultSender"的语义，行为会变。需在迁移前确认（grep 调用方）。

### 配置形态（YAML）

```yaml
sms:
  default_country: CN              # 号码无国际前缀时的默认国家（Router 解析用）
  vendors: {...}                   # 现有，不变
  routes:                          # 新增
    - country: CN
      targets:
        - { vendor: aliyun, account: default }
        - { vendor: aliyun, account: backup }
    - country: US
      targets:
        - { vendor: twilio, account: default }
    - country: "*"                 # 兜底，未匹配的国家走这里
      targets:
        - { vendor: aliyun, account: default }
```

启动时 Router 把 `routes` 解析为 `map[country][]AccountProvider`。每个 target 的 `vendor/account` 必须在 `vendors` 段已定义，否则启动失败（fail-fast）。

### AccountProvider 类型

go-common 的 `sms.Provider` 接口只有 `Name() / Send()`，无法携带 account 信息。message-service 内部包装：

```go
// AccountProvider wraps a vendor Provider with its (vendor, account) identity,
// so Router can return enough context for record persistence.
type AccountProvider struct {
    Vendor   string
    Account  string
    Provider sms.Provider  // 来自 go-common
}
```

`Sender` 改为持有 `[]AccountProvider`（不再直接持有 `[]sms.Provider`），`Send` 返回的 `SendResult` 带 vendor+account。

### SendResult 扩展

```go
type SendResult struct {
    Message  *Message
    Vendor   string        // 新增：vendor 名（原 Provider 字段语义变更）
    Account  string        // 新增：account 名
    Target   string
    Success  bool
    Error    error
    Duration time.Duration
    Attempts int
}
```

**字段重命名**：原 `Provider string` 改为 `Vendor string`。语义更清晰——这是 vendor 维度，account 是另一个维度。

### DB schema 改动

`MessageRecord` 加 `Account string` 列：

```go
type MessageRecord struct {
    ...
    Provider int32   `gorm:"not null;index"`  // 现有，proto enum
    Account  string  `gorm:"size:64"`         // 新增
    ...
}
```

走 `cmd/migrate/` 加 AutoMigrate。`Account` 不加索引（查询频率低，按 vendor+target 组合查询更常见）。

`persistEmailRecord` 也填 `Account` 字段（email 走 SenderFor 同样有 account 信息，统一落库）。

### sendSMS 流程（伪码）

```go
func (s *MessageService) sendSMS(ctx, req) {
    var sender *sms.Sender
    if req.Vendor != "" {
        sender = s.smsRegistry.SenderFor(req.Vendor, req.Account)
    } else {
        sender = s.smsRouter.SenderForPhone(req.To)
    }
    result, err := sender.Send(ctx, msg)
    // result.Vendor / result.Account 落库
}
```

### 路由失败处理

| 场景 | 错误 |
|------|------|
| 手机号为空 | `ErrBadRequest`（"phone number is empty"） |
| 号码格式无法解析 | `ErrBadRequest`（"invalid phone number"） |
| country 无匹配且无 `*` 兜底 | `ErrBadRequest`（"no route for country X"） |
| targets 全部发送失败 | `ErrMessageSendFailed`（行为同当前 Sender 全失败，落库 FAILED 记录） |
| ctx 取消 | 返回 ctx.Err()，落库 FAILED 记录 |

### email 不变

email 没有"号码路由"概念（收件人地址无国家语义），保持现状：`registry.SenderFor(vendor, account).Send()`。但 `Sender` / `SendResult` / `AccountRegistry` 也下沉到 `internal/message/email/`，落库逻辑统一填 Account 字段。

## 迁移路径（高层）

1. **搬家**：先把 go-common 的 Sender/SendResult/Registry/Router 代码搬到 message-service `internal/message/`，删除 Hook。message-service 切换 import，行为完全不变。测试原样保留。
2. **DB schema**：加 `Account` 列，AutoMigrate。
3. **Router 改造**：Router 输入从 `[]sms.Provider` 改为 `[]AccountProvider`；SendResult 加 Vendor/Account 字段；persist*Record 填 Account。
4. **接入 sendSMS**：实现"vendor/account 空时走 Router"，加 routes YAML 配置。
5. **清理 go-common**：删除 go-common/message 中已搬走的代码 + 测试。

详细步骤由后续实施计划给出。

## 风险

- **行为变更**：vendor/account 都空时从 DefaultSender fallback 改为 Router 路由。需 grep 现有 caller 确认无人依赖原行为。
- **YAML 配置变更**：现有 `config.yaml` 没有 `routes` 段。需提供示例配置，并在 routes 为空时给出明确错误（不能静默走 DefaultSender——会让"忘记配置 routes"的问题难排查）。
- **Router 配置漂移**：`routes` 里的 vendor/account 引用必须在 `vendors` 段已定义。启动时校验，失败 fail-fast。
- **go-common/message 的破坏性变更**：删除 Sender/Registry/Router 是 breaking change。但实际只有 message-service 在用，影响为零。仍需在 go-common 提交信息里标明 BREAKING。

## 关联

**实现计划**：待写（writing-plans 阶段）

**历史**：
- v1（已合并）：把 AccountRegistry 从 message-service 提取到 go-common/message（方向判断错误，本次反向）
