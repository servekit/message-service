# message-service

消息发送服务:负责短信、邮件的发送、记录与查询。底层发送能力由 `go-common/message` 提供(已实现 SMTP / Mailgun / Aliyun / Tencent / Volcengine / Byteplus 等供应商),本服务负责工程化封装:**幂等去重、消息持久化、发送记录查询、按区域路由**。

支持三种使用方式(与 user-service 一致):

- **`Server`**(`pkg/server.go`)—— 独立微服务部署,gRPC server 监听 `:19092`,grpc-gateway 监听 `:18082`
- **`Module`**(`pkg/module.go`)—— in-process 使用,父服务直接 import,无网络开销,注入已有 DB/Redis 连接
- **`Client`**(`pkg/client.go`)—— 远程 gRPC 客户端,embed `pb.MessageServiceClient`,所有 RPC 方法直接可用

---

## 目录

- [依赖](#依赖)
- [核心机制](#核心机制)
  - [幂等(Redis)](#幂等redis)
  - [持久化开关(per-channel)](#持久化开关per-channel)
  - [SMS 路径选择(domestic vs international)](#sms-路径选择domestic-vs-international)
- [典型流程](#典型流程)
  - [发送邮件](#发送邮件)
  - [发送短信(国内 / 国际)](#发送短信国内--国际)
  - [查询发送记录(分页 / 游标 / 统计)](#查询发送记录分页--游标--统计)
- [三种使用方式](#三种使用方式)
- [错误码](#错误码)
- [与 user-service 集成](#与-user-service-集成)

---

## 依赖

| 组件 | 用途 | 备注 |
|---|---|---|
| PostgreSQL | 持久化发送记录(`message_email_records` / `message_sms_records`) | 可 per-channel 关闭 |
| Redis | **幂等硬依赖** + 必选,服务启动时 Ping | 关闭会导致 SendEmail/SendSMS 失败 |
| gid-service | 雪花算法 ID 生成 | 通过 gRPC 获取 record_id |
| `go-common/message` | 底层 vendor 抽象(SMTP / Aliyun / Tencent / ...) | 配置在 yaml 里 |

---

## 核心机制

### 幂等(Redis)

**核心契约**:无论持久化开关如何,**幂等永远走 Redis**。

调用方在 `SendEmailRequest` / `SendSMSRequest` 上设置 `idempotency_key`(同一逻辑发送意图的 UUID)即可启用:

| 调用顺序 | 行为 |
|---|---|
| 首次请求 | 发送 + 缓存成功响应 |
| 并发第二请求(还在发送中) | 返回 `ErrIdempotencyConflict` (409) |
| 成功后再次重放(同 key) | 直接返回缓存响应,**不重复发送** |
| 失败(vendor 拒绝/网络) | 释放 reservation,**可重试同一个 key** |

Key 格式:`msg:idem:{channel}:{senderID}:{key}`,TTL per-channel 配置(`idempotency.email_ttl` / `sms_ttl`,默认 5 分钟)。

### 持久化开关(per-channel)

`persistence.email.enabled` / `persistence.sms.enabled`(默认 `true`)只控制 **DB 写入**:

- **发送 RPC**(SendEmail / SendSMS):持久化关闭时跳过 DB 写,vendor 照常调用 + 幂等照常生效
- **查询 RPC**(Get / List / Stats):持久化关闭时返回 `ErrPersistenceDisabled` (503)

`pkg/option.WithEmailPersistence` / `WithSMSPersistence` 用于 module 模式从代码覆盖配置(不影响 Redis 幂等)。

### SMS 路径选择(domestic vs international)

由 `region_code` 自动决定,**两条路径互不交叉**:

| 路径 | 触发条件 | 必填字段 | Vendor 限制 |
|---|---|---|---|
| 国内(domestic) | `region_code == "CN"` | `template_id` + `sign_name` + `template_params` | 强制模板(监管要求),raw `content` 被忽略 |
| 国际(international) | 其他 region_code | `template_id` 或 `content`(二选一) | 模板派(Byteplus / Tencent intl) vs raw 派(Aliyun intl / Twilio) |

设置 `vendor + account` 可显式覆盖路由,否则由 router 按 region 决定。

---

## 典型流程

### 发送邮件

```
SendEmail(to, subject, body, scene=EMAIL_SCENE_LOGIN_CODE, sender_id="user-service", idempotency_key=uuid)
  → 选 vendor(显式 or 默认 fallback chain)
  → 调 vendor API / SMTP
  → 成功:写 DB 记录 + 缓存幂等响应 → 返回 {id, status=SENT}
  → 失败:释放幂等 reservation → 返回 ErrMessageSendFailed(可重试同一个 key)
```

`sender_id` 是**调用方服务名**(如 `user-service` / `pay-service`),**不是**终端用户 ID —— 调用方需要把 user_id / admin_id 记到自己的审计表。

### 发送短信(国内 / 国际)

**国内(CN):**
```
SendSMS(
  region_code="CN", phone="13800138000",
  template_id="SMS_123", template_params={"code": "1234"},
  sign_name="字节跳动",  // 必填,需在 vendor 后台预注册
  scene=SMS_SCENE_LOGIN_CODE,
  sender_id="user-service",
)
```

**国际(以美国为例,raw content):**
```
SendSMS(
  region_code="US", phone="5551234567",
  content="Your code is 1234",  // 模板派 vendor 用 template_id 代替
  scene=SMS_SCENE_LOGIN_CODE,
  sender_id="user-service",
)
```

phone 必须是**本地号**且**不以 "+" 开头** —— 服务端用 `region_code` 作为 `defaultRegion` 解析为 E.164。`"+8613800138000"` 会被验证规则拒掉。

### 查询发送记录(分页 / 游标 / 统计)

| 用途 | RPC |
|---|---|
| 后台 UI 列表(需要总页数) | `ListEmails` / `ListSMS` —— offset 分页,每页都跑 COUNT |
| 大数据集 / 流式扫描 | `ListEmailsByCursor` / `ListSMSByCursor` —— 游标分页,默认不跑 COUNT |
| 仪表盘聚合 | `GetEmailStats` / `GetSMSStats` —— total / sent / failed / success_rate + per-vendor |
| 单条详情 | `GetEmail` / `GetSMS` |
| 前端筛选下拉框 | `ListSMSRegions` / `ListSMSSenders` / `ListEmailSenders` |

游标分页顺序为 `(sort_field, id)`,`id` 兜底,所以并发写入不会丢行或重行。第一次页通常能直接用 `len(records)` 当 total,避免 COUNT。

---

## 三种使用方式

### 1. Server — 独立部署

```go
import msgsrv "message-service/pkg"

srv, err := msgsrv.NewServer(cfg, msgsrv.WithServiceOptions(opts...))
if err != nil { return err }
defer srv.Stop()
return srv.Start()  // blocks; listens :19092 + :18082
```

### 2. Module — in-process(无网络开销)

```go
import (
    msgsrv "message-service/pkg"
    "message-service/pkg/option"
)

handler, err := msgsrv.NewModule(cfg,
    option.WithDB(parentDB),         // 复用父服务连接,生命周期由父服务管
    option.WithRedis(parentRedis),
)
if err != nil { return err }
defer handler.Stop()

if err := handler.Start(); err != nil { return err }  // 启动后台任务

// 直接调方法,不走网络
resp, err := handler.SendEmail(ctx, &pb.SendEmailRequest{...})
```

`handler` 同时实现 `pb.MessageServiceServer`(可注册到父服务的 gRPC server)和 `signalx.Service`(生命周期管理)。

### 3. Client — 远程调用

```go
import msgsrv "message-service/pkg"

client, err := msgsrv.NewClient("dns:///message-service:19092",
    grpc.WithTransportCredentials(insecure.NewCredentials()),  // 或 mTLS
)
if err != nil { return err }
defer client.Close()

resp, err := client.SendEmail(ctx, &pb.SendEmailRequest{...})
// client embeds pb.MessageServiceClient,所有 RPC 方法直接可用
```

---

## 错误码

所有错误都是 `xerr.Error`(go-common/xerr),定义在 `pkg/xcodes/message.go`:

| Error | HTTP | 触发条件 |
|---|---|---|
| `ErrBadRequest` | 400 | 字段校验失败(vendor+account 不配对、scene 缺失、phone 格式错等) |
| `ErrEmailNotFound` / `ErrSMSNotFound` | 404 | GetEmail / GetSMS 找不到记录 |
| `ErrPersistenceDisabled` | 503 | 查询 RPC 在持久化关闭时调用 |
| `ErrMessageSendFailed` | 500 | vendor 同步拒绝、网络失败、context 取消。**不缓存,可重试同一 key** |
| `ErrIdempotencyConflict` | 409 | 同一 (sender_id, idempotency_key) 正在 in-flight |
| `ErrInternal` | 500 | DB 错误、Redis 错误、gid 失败等意外错误 |

比较方式:`errors.Is(err, xcodes.ErrXxx)`(xerr 已实现 `Is()` + `Unwrap()`)。

---

## 与 user-service 集成

user-service 调用本服务发送验证码:

```go
// user-service 的 SendVerificationCode handler 内部:
_, err := messageClient.SendEmail(ctx, &messagepb.SendEmailRequest{
    To:       target,
    Subject:  strings.ReplaceAll(req.EmailSubject, "{code}", code),
    Body:     strings.ReplaceAll(req.EmailBody, "{code}", code),
    Scene:    messagepb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
    SenderId: "user-service",  // ← 调用方身份,不是终端用户
})
```

`{code}` 占位符约定:user-service 把验证码替换进 `subject` / `body` / `html_body`,再转发。message-service 本身**不做任何模板替换**(除了底层 vendor 自己的 template_params)。

详细的 caller 视角契约见 user-service 的 `SendVerificationCodeRequest` proto 注释。
