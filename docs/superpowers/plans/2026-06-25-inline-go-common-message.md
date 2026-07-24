# Inline go-common/message Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `go-common/message` 的 4 个文件(Provider interface + Message struct + SMTP/Aliyun vendor impl)平移到 `message-service/internal/provider/`,合并 `Provider` 与 `AccountProvider`,升级 `Vendor` 字段为 proto enum,最后删除 `go-common/message`。

**Architecture:** 扁平化设计 —— vendor impl 文件直接放在 `email/` 和 `sms/` package 内(不用子目录),避免 Go 循环 import。自底向上 7 步:① 创建 Message 类型 → ② 创建 SMTP vendor impl → ③ 创建 Aliyun vendor impl → ④ 切换 email 层(sender/registry/service/测试同改)→ ⑤ 切换 sms 层 → ⑥ 删除 go-common/message → ⑦ 全量验证。Task 4/5 是 atomic 的(中间状态不编译),一个 commit 完成所有切换。

**Tech Stack:** Go 1.26、GORM、grpc-gateway、`pb.EmailVendor` / `pb.SmsVendor` proto enum、`wneessen/go-mail`、`alibabacloud-go/dysmsapi`。

**Spec:** [`docs/superpowers/specs/2026-06-25-inline-go-common-message-design.md`](../specs/2026-06-25-inline-go-common-message-design.md)

---

## File Structure

**新增**(4 个文件,扁平化):
- `internal/provider/email/message.go` — `Message` struct
- `internal/provider/email/smtp.go` — SMTP vendor impl(`SMTPProvider` / `SMTPConfig` / `NewSMTPProvider`)
- `internal/provider/sms/message.go` — `Message` struct
- `internal/provider/sms/aliyun.go` — Aliyun vendor impl(`AliyunProvider` / `AliyunConfig` / `NewAliyunProvider`)

**vendor impl 测试**(2 个文件):
- `internal/provider/email/smtp_test.go` — fakeSMTPServer 测试,平移自 go-common
- `internal/provider/sms/aliyun_test.go` — mock SDK client 测试,平移自 go-common

**改造**(13 个文件):
- `internal/provider/email/sender.go` — `AccountProvider` struct→interface
- `internal/provider/email/registry.go` — `buildProvider` 返回 interface,调用同包 `NewSMTPProvider`
- `internal/provider/email/sender_test.go` — mock 改为实现 interface
- `internal/provider/email/registry_test.go` — 同上
- `internal/provider/sms/sender.go` — 同 email
- `internal/provider/sms/registry.go` — 同 email,调用同包 `NewAliyunProvider`
- `internal/provider/sms/router.go` — 字段访问 → 方法调用
- `internal/provider/sms/router_builder.go` — 类型引用变化
- `internal/provider/sms/sender_test.go` — mock 改造
- `internal/provider/sms/registry_test.go` — 同上
- `internal/provider/sms/router_test.go` — 同上
- `internal/provider/sms/router_builder_test.go` — 同上
- `internal/service/message/email.go` — 删反向转换 + import 本地
- `internal/service/message/sms.go` — 同上

**删除**:`go-common/message/` 整个目录(7 个文件)

**命名约定**:vendor impl 类型加 vendor 前缀以避免与父包已有类型冲突:
- email 包:`SMTPProvider` / `SMTPConfig` / `NewSMTPProvider`
- sms 包:`AliyunProvider` / `AliyunConfig` / `NewAliyunProvider`

---

## Task 1: 创建 Message struct(email + sms)

把 go-common 的 `Message` struct 平移到本地包。此时只新增类型,vendor impl 还没引用它,不影响其他代码。

**Files:**
- Create: `internal/provider/email/message.go`
- Create: `internal/provider/sms/message.go`

- [ ] **Step 1: 创建 email/message.go**

```go
package email

// Message represents an email to be sent.
type Message struct {
	To       string
	Cc       []string
	Bcc      []string
	Subject  string
	Body     string
	HTMLBody string
	ReplyTo  string

	// Template is an optional vendor-side template identifier. Vendors
	// that do not support templating (e.g. SMTP) ignore this field.
	Template string
	// TemplateParams supplies variable substitutions when Template is set.
	// Vendors that do not support templating ignore this field.
	TemplateParams map[string]string
}
```

注意:`internal/provider/email/sender.go` 顶部已有一段 package doc,本 step 不动它(Task 4 重写)。该 doc 描述与新文件不冲突,编译仍通过。

- [ ] **Step 2: 创建 sms/message.go**

```go
package sms

// Message represents an SMS to be sent.
type Message struct {
	To       string
	Content  string
	Template string
	Params   map[string]string
}
```

- [ ] **Step 3: 验证编译**

```bash
go build ./...
```

Expected: PASS(无输出)

- [ ] **Step 4: Commit**

```bash
git add internal/provider/email/message.go internal/provider/sms/message.go
git commit -m "feat(provider): add local Message structs for email and sms"
```

---

## Task 2: 创建 SMTP vendor impl

在 `internal/provider/email/` 包内新增 `smtp.go`,定义 `SMTPProvider` 实现。基于 go-common 的 SMTP 代码,加 `Vendor()` / `Account()` 方法、改 `NewSMTPProvider` 签名、引用同包 `Message`。**此时该文件独立可编译但还未被 registry 使用**(因为 AccountProvider 还不是 interface,vendor impl 暂不实现 interface 断言)。

**Files:**
- Create: `internal/provider/email/smtp.go`
- Create: `internal/provider/email/smtp_test.go`

- [ ] **Step 1: 创建 smtp.go**

```go
package email

import (
	"context"
	"fmt"

	mail "github.com/wneessen/go-mail"

	pb "message-service/gen/message/v1"
)

// SMTPConfig holds the configuration for the SMTP provider.
type SMTPConfig struct {
	Host     string
	Port     int `default:"587"` // STARTTLS submission port; WithPort rejects 0
	Username string
	Password string
	From     string
}

// SMTPProvider sends emails via SMTP using a reusable client. Implements
// AccountProvider once Task 4 turns AccountProvider into an interface.
type SMTPProvider struct {
	account string
	client  *mail.Client
	from    string
}

// NewSMTPProvider creates an SMTP provider. The mail.Client is initialized
// once and reused across Send calls (each call still dials a new connection
// via DialAndSendWithContext, which is safe for concurrent use).
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider.
func NewSMTPProvider(account string, config *SMTPConfig) (*SMTPProvider, error) {
	client, err := mail.NewClient(config.Host,
		mail.WithPort(config.Port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(config.Username),
		mail.WithPassword(config.Password),
		mail.WithTLSPortPolicy(mail.TLSOpportunistic),
	)
	if err != nil {
		return nil, fmt.Errorf("smtp: create client: %w", err)
	}
	return &SMTPProvider{account: account, client: client, from: config.From}, nil
}

// Vendor identifies this provider as CUSTOM_SMTP in the proto enum.
func (p *SMTPProvider) Vendor() pb.EmailVendor { return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP }

// Account returns the account name this provider was constructed with.
func (p *SMTPProvider) Account() string { return p.account }

// Send sends an email via SMTP.
func (p *SMTPProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("smtp: recipient is empty")
	}

	m := mail.NewMsg()
	if err := m.From(p.from); err != nil {
		return fmt.Errorf("smtp: invalid from address: %w", err)
	}
	if err := m.To(msg.To); err != nil {
		return fmt.Errorf("smtp: invalid to address: %w", err)
	}
	if len(msg.Cc) > 0 {
		if err := m.Cc(msg.Cc...); err != nil {
			return fmt.Errorf("smtp: invalid cc address: %w", err)
		}
	}
	if len(msg.Bcc) > 0 {
		if err := m.Bcc(msg.Bcc...); err != nil {
			return fmt.Errorf("smtp: invalid bcc address: %w", err)
		}
	}
	if msg.ReplyTo != "" {
		if err := m.ReplyTo(msg.ReplyTo); err != nil {
			return fmt.Errorf("smtp: invalid reply-to address: %w", err)
		}
	}

	subject := msg.Subject
	if subject == "" {
		subject = "Notification"
	}
	m.Subject(subject)

	if msg.HTMLBody != "" {
		m.SetBodyString(mail.TypeTextHTML, msg.HTMLBody)
		if msg.Body != "" {
			m.AddAlternativeString(mail.TypeTextPlain, msg.Body)
		}
	} else {
		m.SetBodyString(mail.TypeTextPlain, msg.Body)
	}

	if err := p.client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: 创建 smtp_test.go**

平移 go-common 的 `smtp_test.go`,改动:
1. `package smtp` → `package email`(同包测试)
2. 删除 `import "github.com/servekit/go-common/message/email"`(Message 在本包)
3. `TestProvider_Name` → `TestSMTPProvider_Vendor`,断言 `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP`
4. `newTestProvider` 接受 account 参数,内部调用 `NewSMTPProvider`
5. 所有 `email.Message` → `Message`(同包)
6. `Provider` 类型引用 → `SMTPProvider`

```go
package email

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// fakeSMTPServer starts a minimal SMTP server on 127.0.0.1, returns its address.
func fakeSMTPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		fmt.Fprintf(rw, "220 test ESMTP\r\n")
		rw.Flush()

		inData := false
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if inData {
				if line == "." {
					fmt.Fprintf(rw, "250 2.0.0 Ok: queued\r\n")
					rw.Flush()
					inData = false
				}
				continue
			}

			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprintf(rw, "250-test\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "HELO"):
				fmt.Fprintf(rw, "250 test\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				fmt.Fprintf(rw, "235 2.7.0 Authentication successful\r\n")
			case strings.HasPrefix(line, "MAIL FROM"):
				fmt.Fprintf(rw, "250 2.1.0 Ok\r\n")
			case strings.HasPrefix(line, "RCPT TO"):
				fmt.Fprintf(rw, "250 2.1.5 Ok\r\n")
			case strings.HasPrefix(line, "DATA"):
				fmt.Fprintf(rw, "354 End data with <CR><LF>.<CR><LF>\r\n")
				inData = true
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprintf(rw, "221 2.0.0 Bye\r\n")
				rw.Flush()
				return
			default:
				fmt.Fprintf(rw, "250 2.0.0 Ok\r\n")
			}
			rw.Flush()
		}
	}()

	return ln.Addr().String()
}

func newSMTPTestProvider(t *testing.T, account, host string, port int) *SMTPProvider {
	t.Helper()
	p, err := NewSMTPProvider(account, &SMTPConfig{
		Host:     host,
		Port:     port,
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.NoError(t, err)
	return p
}

func TestSMTPProvider_Vendor(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "localhost", 1025)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, p.Vendor())
}

func TestSMTPProvider_Account(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "localhost", 1025)
	require.Equal(t, "primary", p.Account())
}

func TestSMTPProvider_Send_success(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: "user@test.com", Subject: "Hello", Body: "World"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_defaultSubject(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: "user@test.com", Body: "No subject"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_emptyRecipient(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "localhost", 1025)
	err := p.Send(context.Background(), &Message{To: "", Body: "test"})
	require.EqualError(t, err, "smtp: recipient is empty")
}

func TestSMTPProvider_Send_connectionRefused(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "127.0.0.1", 19999)
	err := p.Send(context.Background(), &Message{To: "user@test.com", Body: "test"})
	require.Error(t, err)
}

func TestSMTPProvider_Send_emptyBody(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: "user@test.com", Subject: "No body"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_withCc(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:      "user@test.com",
		Cc:      []string{"cc1@test.com", "cc2@test.com"},
		Subject: "With CC",
		Body:    "Hello",
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_withBcc(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:      "user@test.com",
		Bcc:     []string{"bcc@test.com"},
		Subject: "With Bcc",
		Body:    "Secret",
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_htmlBody(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:       "user@test.com",
		Subject:  "HTML",
		Body:     "Plain text fallback",
		HTMLBody: "<h1>Hello</h1><p>World</p>",
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_htmlOnlyNoPlainFallback(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:       "user@test.com",
		Subject:  "HTML only",
		HTMLBody: "<h1>Hello</h1>",
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_replyTo(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:      "user@test.com",
		ReplyTo: "reply@example.com",
		Subject: "Reply please",
		Body:    "Content",
	}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func mustAtoi(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestSMTPProvider_NewProvider_error(t *testing.T) {
	_, err := NewSMTPProvider("primary", &SMTPConfig{
		Host:     "",
		Port:     0,
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp: create client")
}

func TestSMTPProvider_Send_invalidFromAddress(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)

	p, err := NewSMTPProvider("primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "not-valid-email",
	})
	require.NoError(t, err)

	err = p.Send(context.Background(), &Message{To: "user@test.com", Subject: "Hi", Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp: invalid from address")
}

func TestSMTPProvider_Send_invalidToAddress(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	err := p.Send(context.Background(), &Message{To: "not-valid-email", Subject: "Hi", Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp: invalid to address")
}
```

- [ ] **Step 3: 验证 smtp 文件独立编译 + 测试**

```bash
gofmt -w internal/provider/email/smtp.go internal/provider/email/smtp_test.go
go build ./...
go test -race ./internal/provider/email/...
```

Expected: PASS。注意 sender_test/registry_test 此时还在用旧的 `&AccountProvider{Vendor: "smtp", ...}` mock 形式,所以它们也应通过(因为 AccountProvider 还是 struct)。

- [ ] **Step 4: Commit**

```bash
git add internal/provider/email/smtp.go internal/provider/email/smtp_test.go
git commit -m "feat(provider/email): add flat SMTP vendor impl"
```

---

## Task 3: 创建 Aliyun SMS vendor impl

在 `internal/provider/sms/` 包内新增 `aliyun.go`,定义 `AliyunProvider`。与 Task 2 对称。

**Files:**
- Create: `internal/provider/sms/aliyun.go`
- Create: `internal/provider/sms/aliyun_test.go`

- [ ] **Step 1: 创建 aliyun.go**

```go
package sms

import (
	"context"
	"encoding/json"
	"fmt"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	client "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"

	pb "message-service/gen/message/v1"
)

// AliyunConfig holds the configuration for the Aliyun SMS provider.
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	SignName        string
	RegionID        string `default:"cn-hangzhou"`
}

// AliyunProvider sends SMS via the Aliyun SDK. Implements AccountProvider
// once Task 5 turns AccountProvider into an interface.
type AliyunProvider struct {
	account   string
	config    *AliyunConfig
	smsClient aliyunSmsSender
}

// aliyunSmsSender abstracts the Aliyun SMS SDK client for testability.
type aliyunSmsSender interface {
	SendSmsWithContext(ctx context.Context, req *client.SendSmsRequest, runtime *dara.RuntimeOptions) (*client.SendSmsResponse, error)
}

// NewAliyunProvider creates an Aliyun SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider.
func NewAliyunProvider(account string, config *AliyunConfig) (*AliyunProvider, error) {
	smsClient, err := client.NewClient(&openapi.Config{
		AccessKeyId:     dara.String(config.AccessKeyID),
		AccessKeySecret: dara.String(config.AccessKeySecret),
		RegionId:        dara.String(config.RegionID),
	})
	if err != nil {
		return nil, fmt.Errorf("create aliyun sms client: %w", err)
	}

	return &AliyunProvider{
		account:   account,
		config:    config,
		smsClient: smsClient,
	}, nil
}

// newAliyunProviderWithClient creates an AliyunProvider with a mock client (for testing).
func newAliyunProviderWithClient(account string, config *AliyunConfig, sender aliyunSmsSender) *AliyunProvider {
	return &AliyunProvider{account: account, config: config, smsClient: sender}
}

// Vendor identifies this provider as ALIYUN in the proto enum.
func (p *AliyunProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_ALIYUN }

// Account returns the account name this provider was constructed with.
func (p *AliyunProvider) Account() string { return p.account }

// Send sends an SMS via the Aliyun SDK.
func (p *AliyunProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("aliyun sms: phone number is empty")
	}

	if msg.Template == "" {
		return fmt.Errorf("aliyun sms: template is required")
	}

	req := &client.SendSmsRequest{
		PhoneNumbers: dara.String(msg.To),
		SignName:     dara.String(p.config.SignName),
		TemplateCode: dara.String(msg.Template),
	}

	if msg.Params != nil {
		templateParam, err := json.Marshal(msg.Params)
		if err != nil {
			return fmt.Errorf("marshal template params: %w", err)
		}
		req.TemplateParam = dara.String(string(templateParam))
	}

	resp, err := p.smsClient.SendSmsWithContext(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("aliyun sms send: %w", err)
	}

	code := dara.StringValue(resp.Body.Code)
	if code != "OK" {
		return fmt.Errorf("aliyun sms: code=%s, message=%s",
			code, dara.StringValue(resp.Body.Message))
	}

	return nil
}
```

- [ ] **Step 2: 创建 aliyun_test.go**

平移 go-common 的 `aliyun_test.go`,改动:
1. `package aliyun` → `package sms`
2. 删除 `import "github.com/servekit/go-common/message/sms"`
3. `TestProvider_Name` → `TestAliyunProvider_Vendor`,断言 `pb.SmsVendor_SMS_VENDOR_ALIYUN`
4. `NewProvider(&Config{...})` → `NewAliyunProvider("primary", &AliyunConfig{...})`
5. `newProviderWithClient(&Config{...}, ...)` → `newAliyunProviderWithClient("primary", &AliyunConfig{...}, ...)`
6. `sms.Message` → `Message`(同包)

```go
package sms

import (
	"context"
	"fmt"
	"testing"

	client "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// aliyunMockSender is a mock implementation of aliyunSmsSender.
type aliyunMockSender struct {
	resp *client.SendSmsResponse
	err  error
}

func (m *aliyunMockSender) SendSmsWithContext(_ context.Context, _ *client.SendSmsRequest, _ *dara.RuntimeOptions) (*client.SendSmsResponse, error) {
	return m.resp, m.err
}

func aliyunOkResponse() *client.SendSmsResponse {
	return &client.SendSmsResponse{
		Body: &client.SendSmsResponseBody{
			Code:    dara.String("OK"),
			Message: dara.String("ok"),
		},
	}
}

func aliyunErrResponse(code, msg string) *client.SendSmsResponse {
	return &client.SendSmsResponse{
		Body: &client.SendSmsResponseBody{
			Code:    dara.String(code),
			Message: dara.String(msg),
		},
	}
}

func TestAliyunProvider_Vendor(t *testing.T) {
	p, err := NewAliyunProvider("primary", &AliyunConfig{
		AccessKeyID:     "test",
		AccessKeySecret: "test",
		SignName:        "TestApp",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, p.Vendor())
}

func TestAliyunProvider_Account(t *testing.T) {
	p, err := NewAliyunProvider("primary", &AliyunConfig{
		AccessKeyID:     "test",
		AccessKeySecret: "test",
		SignName:        "TestApp",
	})
	require.NoError(t, err)
	require.Equal(t, "primary", p.Account())
}

func TestAliyunProvider_Send_success(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:       "13800138000",
		Template: "SMS_123",
	})
	require.NoError(t, err)
}

func TestAliyunProvider_Send_withParams(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:       "13800138000",
		Template: "SMS_123",
		Params:   map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}

func TestAliyunProvider_Send_emptyPhone(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{To: "", Template: "SMS_123"})
	require.EqualError(t, err, "aliyun sms: phone number is empty")
}

func TestAliyunProvider_Send_noTemplate(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{To: "13800138000"})
	require.EqualError(t, err, "aliyun sms: template is required")
}

func TestAliyunProvider_Send_sdkError(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{
		err: fmt.Errorf("network timeout"),
	})

	err := p.Send(context.Background(), &Message{
		To:       "13800138000",
		Template: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "aliyun sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestAliyunProvider_Send_businessError(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{SignName: "TestApp"}, &aliyunMockSender{
		resp: aliyunErrResponse("isv.BUSINESS_LIMIT_CONTROL", "frequency limit"),
	})

	err := p.Send(context.Background(), &Message{
		To:       "13800138000",
		Template: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "BUSINESS_LIMIT_CONTROL")
	require.Contains(t, err.Error(), "frequency limit")
}
```

注意:helper 函数名加 `aliyun` 前缀(`aliyunMockSender` / `aliyunOkResponse` / `aliyunErrResponse`),避免后续若加新 SMS vendor 时与同包内其他 vendor 的 mock helper 冲突。

- [ ] **Step 3: 验证 aliyun 文件独立编译 + 测试**

```bash
gofmt -w internal/provider/sms/aliyun.go internal/provider/sms/aliyun_test.go
go build ./...
go test -race -run "Aliyun" ./internal/provider/sms/...
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/provider/sms/aliyun.go internal/provider/sms/aliyun_test.go
git commit -m "feat(provider/sms): add flat Aliyun vendor impl"
```

---

## Task 4: 切换 email 层到本地包(atomic)

把 `internal/provider/email/` 完整切换:sender struct→interface、registry 调用同包 `NewSMTPProvider`、service 改 import、所有测试改 mock。**这一步是 atomic 的,中间状态不编译**,所有改动放一个 commit。

**Files:**
- Modify: `internal/provider/email/sender.go` (全文重写)
- Modify: `internal/provider/email/registry.go` (buildProvider + 类型引用)
- Modify: `internal/service/message/email.go` (import + 反向转换 + 日志)
- Modify: `internal/provider/email/sender_test.go` (mock 改为实现 interface)
- Modify: `internal/provider/email/registry_test.go` (同上)

- [ ] **Step 1: 重写 sender.go**

完整替换 `internal/provider/email/sender.go`:

```go
// Package email provides email sending with provider fallback.
//
// AccountProvider is an interface implemented in this package (currently
// SMTPProvider in smtp.go). Composition (Sender, fallback) and account
// registry live in this package too.
package email

import (
	"context"
	"fmt"
	"time"

	pb "message-service/gen/message/v1"
)

// AccountProvider is the interface vendor implementations satisfy. It carries
// both the proto-enum Vendor identity and the per-account name, so Sender
// and Registry can return enough context for record persistence without an
// external wrapper layer.
type AccountProvider interface {
	Vendor() pb.EmailVendor
	Account() string
	Send(ctx context.Context, msg *Message) error
}

// SendResult captures the outcome of a Send call.
type SendResult struct {
	Message  *Message       // the message that was sent
	Vendor   pb.EmailVendor // vendor that handled the request (or last tried)
	Account  string         // account identifier of the vendor provider used
	Target   string         // recipient email
	Success  bool           // whether the send succeeded
	Error    error          // last error if all providers failed
	Duration time.Duration  // total time across all provider attempts
	Attempts int            // number of providers tried
}

// Sender sends emails with automatic fallback between providers.
type Sender struct {
	providers []AccountProvider
}

// NewSender creates a Sender that tries providers in order.
func NewSender(providers []AccountProvider) *Sender {
	return &Sender{providers: providers}
}

// Send tries providers in order, falling back on failure.
//
// Return contract:
//   - Validation error (empty recipient, no provider): (nil, err)
//   - Success: (result with Success=true, nil)
//   - All providers failed: (result with Success=false, wrapped err)
//   - Context cancelled: (result with Success=false, ctx.Err())
func (s *Sender) Send(ctx context.Context, msg *Message) (*SendResult, error) {
	if msg.To == "" {
		return nil, fmt.Errorf("email: recipient is empty")
	}

	if len(s.providers) == 0 {
		return nil, fmt.Errorf("email: no provider available")
	}

	start := time.Now()
	var lastErr error
	var lastVendor pb.EmailVendor
	var lastAccount string
	attempts := 0

	for _, p := range s.providers {
		if ctx.Err() != nil {
			return &SendResult{
				Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
				Success: false, Error: ctx.Err(),
				Duration: time.Since(start), Attempts: attempts,
			}, ctx.Err()
		}

		attempts++
		lastVendor = p.Vendor()
		lastAccount = p.Account()
		if err := p.Send(ctx, msg); err != nil {
			lastErr = err
			continue
		}

		return &SendResult{
			Message: msg, Vendor: p.Vendor(), Account: p.Account(), Target: msg.To,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}

	return &SendResult{
		Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("email: all providers failed, last error: %w", lastErr)
}
```

- [ ] **Step 2: 重写 registry.go**

完整替换 `internal/provider/email/registry.go`。关键差异:
- `vendors` map 类型 `*AccountProvider` → `AccountProvider`
- `flattenProviders` 入参/返回类型同步
- `buildProvider` 直接调用同包 `NewSMTPProvider`(无需 import 子包,无 import cycle)
- 删除 `smtpprovider` import

```go
package email

import (
	"fmt"
	"sort"

	pb "message-service/gen/message/v1"
)

// Config is the cooked config: vendor keys are proto enums, parsed from YAML
// strings by ParseVendorName during config.Load (fail-fast on unknown vendor).
type Config struct {
	Vendors map[pb.EmailVendor]*VendorConfig
}

// VendorConfig holds accounts under one vendor (e.g. "custom_smtp", "aliyun").
type VendorConfig struct {
	Accounts []*AccountConfig
}

// AccountConfig is a single named account. Carries fields for all supported
// vendors; only the subset matching the parent vendor is used.
//
// fat-struct design: adding a new vendor means adding fields here. This is a
// low-frequency operation, and adding a new vendor requires a new vendor impl
// file in this package anyway.
//
// vendor name in YAML must match pb.EmailVendor's lowercase form:
// "custom_smtp", "aliyun", "tencent", "netease". Unknown names rejected at
// registry construction.
type AccountConfig struct {
	Name     string
	Host     string // SMTP host; required for every vendor
	Port     int    // SMTP submission port (587 STARTTLS, 465 implicit TLS)
	Username string // SMTP
	Password string // SMTP
	From     string // SMTP
}

// AccountRegistry indexes AccountProviders by (vendor, account) and exposes
// both a default fallback sender and per-account senders.
type AccountRegistry struct {
	vendors map[pb.EmailVendor]map[string]AccountProvider
	def     *Sender
}

// NewAccountRegistryFromProviders builds a registry from a pre-built provider
// map keyed by proto enum. The default fallback chain is ordered by vendor
// enum value asc, then account name asc.
//
// This constructor is primarily for testing and advanced use cases. Production
// code should use NewAccountRegistry to construct from Config.
func NewAccountRegistryFromProviders(vendors map[pb.EmailVendor]map[string]AccountProvider) *AccountRegistry {
	r := &AccountRegistry{
		vendors: vendors,
	}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// NewAccountRegistry constructs a registry from cooked config (enum-keyed
// Vendors map). Vendor name strings have already been validated and converted
// to enums by config.Load via ParseVendorName.
//
// Behavior:
//   - Iterates each vendor, calling the corresponding constructor (currently
//     NewSMTPProvider for all current vendors).
//   - Each constructed AccountProvider is stored directly (vendor impls
//     carry their own Vendor/Account identity — no external wrapper).
//   - Duplicate account names within the same vendor are rejected.
//   - Provider construction failures return an error.
//
// The default fallback chain is ordered by vendor enum value asc, then account
// name asc (guaranteed by NewAccountRegistryFromProviders).
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[pb.EmailVendor]map[string]AccountProvider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for vendorEnum, vc := range cfg.Vendors {
		accounts := make(map[string]AccountProvider)
		for _, ac := range vc.Accounts {
			if _, dup := accounts[ac.Name]; dup {
				return nil, fmt.Errorf("email: duplicate account name %q under vendor %s", ac.Name, vendorEnum)
			}
			p, err := buildProvider(vendorEnum, ac)
			if err != nil {
				return nil, fmt.Errorf("email: account %s/%s: %w", vendorEnum, ac.Name, err)
			}
			accounts[ac.Name] = p
		}
		vendors[vendorEnum] = accounts
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// DefaultSender returns the fallback sender containing all providers in the
// order determined at construction time.
func (r *AccountRegistry) DefaultSender() *Sender {
	return r.def
}

// SenderFor selects a sender based on vendor+account.
//
// Behavior:
//   - vendor == UNSPECIFIED && account == "" → DefaultSender (fallback chain)
//   - both set → sender with only that provider (no fallback)
//   - only one set → error (vendor and account must be set together)
//   - unknown vendor → error
//   - unknown account → error
//
// Design tradeoff: no fallback when vendor+account is specified. The caller
// explicitly chose a specific account; semantically "use this account, fail
// if it fails" — easier to debug and audit.
func (r *AccountRegistry) SenderFor(vendor pb.EmailVendor, account string) (*Sender, error) {
	if vendor == pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED && account == "" {
		return r.def, nil
	}
	if vendor == pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("email: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown email vendor %q", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown email account %q under vendor %q", account, vendor)
	}
	return NewSender([]AccountProvider{p}), nil
}

// --- internal helpers ---

// buildProvider dispatches to the corresponding constructor based on vendor
// enum and returns the constructed AccountProvider. The vendor impl carries
// its own (vendor, account) identity. Add a case here when adding a new vendor,
// and add corresponding fields to AccountConfig.
//
// All current vendors use SMTP protocol and share NewSMTPProvider.
// config.Host is required for every vendor — no built-in host table to maintain.
func buildProvider(vendor pb.EmailVendor, ac *AccountConfig) (AccountProvider, error) {
	switch vendor {
	case pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
		pb.EmailVendor_EMAIL_VENDOR_TENCENT,
		pb.EmailVendor_EMAIL_VENDOR_NETEASE:
		if ac.Host == "" {
			return nil, fmt.Errorf("email vendor %s requires explicit host", vendor)
		}
		return NewSMTPProvider(ac.Name, &SMTPConfig{
			Host:     ac.Host,
			Port:     ac.Port,
			Username: ac.Username,
			Password: ac.Password,
			From:     ac.From,
		})
	default:
		return nil, fmt.Errorf("unknown vendor %s", vendor)
	}
}

// flattenProviders expands the nested map into a flat slice ordered by vendor
// enum value asc, then account name asc. The sort is deterministic so the
// default fallback chain stays stable across runs.
func flattenProviders(vendors map[pb.EmailVendor]map[string]AccountProvider) []AccountProvider {
	vendorEnums := make([]pb.EmailVendor, 0, len(vendors))
	for v := range vendors {
		vendorEnums = append(vendorEnums, v)
	}
	sort.Slice(vendorEnums, func(i, j int) bool { return vendorEnums[i] < vendorEnums[j] })

	var out []AccountProvider
	for _, v := range vendorEnums {
		accounts := vendors[v]
		acctNames := make([]string, 0, len(accounts))
		for a := range accounts {
			acctNames = append(acctNames, a)
		}
		sort.Strings(acctNames)
		for _, a := range acctNames {
			out = append(out, accounts[a])
		}
	}
	return out
}
```

- [ ] **Step 3: 改造 service/message/email.go**

5 处改动:

1. **import 块**:删除 `emailcommon "github.com/servekit/go-common/message/email"` 这一行。其他 import 保持不变(包括 `"message-service/internal/provider/email"`)。

2. **第 51 行**:`emailcommon.Message` → `email.Message`

   ```go
   msg := &email.Message{
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
   ```

3. **第 78-79 行**:日志 `result.Vendor` → `result.Vendor.String()`(enum 不能直接 %s)

   ```go
   return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
       "vendor=%s account=%s attempts=%d",
       result.Vendor.String(), result.Account, result.Attempts)
   ```

4. **第 86 行**:`pb.EmailVendor(pb.EmailVendor_value[result.Vendor])` → `result.Vendor`(直接 enum)

   ```go
   Vendor: &pb.SendResponse_EmailVendor{
       EmailVendor: result.Vendor,
   },
   ```

5. **第 291 行**:`pb.EmailVendor_value[result.Vendor]` → `int32(result.Vendor)`(DB 存 int32)

   ```go
   record := &models.MessageEmailRecord{
       ID:             id,
       Vendor:         int32(result.Vendor),
       Account:        result.Account,
       ...
   }
   ```

- [ ] **Step 4: 重写 sender_test.go**

完整替换 `internal/provider/email/sender_test.go`。`testProvider` 改为实现 `AccountProvider` interface:

```go
package email

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// testProvider is a sender-level test AccountProvider that records the last
// sent Message. Distinct from registry_test.go's fakeProvider (counts Send
// calls) — sender tests need to assert which message was forwarded.
type testProvider struct {
	vendor  pb.EmailVendor
	account string
	err     error
	sent    *Message
}

func (p *testProvider) Vendor() pb.EmailVendor { return p.vendor }
func (p *testProvider) Account() string        { return p.account }
func (p *testProvider) Send(_ context.Context, msg *Message) error {
	if p.err != nil {
		return p.err
	}
	p.sent = msg
	return nil
}

func TestSender_Send_singleProvider(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "default"}
	s := NewSender([]AccountProvider{p})

	msg := &Message{To: "user@test.com", Subject: "Hi", Body: "Hello"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, result.Vendor)
	require.Equal(t, "default", result.Account)
	require.Equal(t, 1, result.Attempts)
	if diff := cmp.Diff(msg, p.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_fallback(t *testing.T) {
	p1 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "primary", err: errors.New("smtp down")}
	p2 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "backup"}
	s := NewSender([]AccountProvider{p1, p2})

	msg := &Message{To: "user@test.com", Subject: "Hi", Body: "Hello"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Nil(t, p1.sent)
	if diff := cmp.Diff(msg, p2.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_allFail(t *testing.T) {
	p1 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "primary", err: errors.New("timeout")}
	p2 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "backup", err: errors.New("rate limited")}
	s := NewSender([]AccountProvider{p1, p2})

	result, err := s.Send(context.Background(), &Message{To: "user@test.com", Subject: "Hi", Body: "Hello"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all providers failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Error(t, result.Error)
}

func TestSender_Send_noProvider(t *testing.T) {
	s := NewSender(nil)
	result, err := s.Send(context.Background(), &Message{To: "user@test.com", Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_emptyRecipient(t *testing.T) {
	s := NewSender([]AccountProvider{
		&testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "default"},
	})
	result, err := s.Send(context.Background(), &Message{To: "", Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "recipient is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_cancelledContext(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "default"}
	s := NewSender([]AccountProvider{p})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := s.Send(ctx, &Message{To: "user@test.com", Body: "test"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestSender_Send_recordsVendorAndAccount(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account: "primary"}
	s := NewSender([]AccountProvider{p})

	result, err := s.Send(context.Background(), &Message{To: "user@test.com", Body: "Hello"})
	require.NoError(t, err)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, result.Vendor)
	require.Equal(t, "primary", result.Account)
}
```

- [ ] **Step 5: 重写 registry_test.go**

完整替换 `internal/provider/email/registry_test.go`。`fakeProvider` 实现新 interface,`wrap` helper 简化:

```go
package email

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// fakeProvider is a registry-level test AccountProvider that counts Send
// calls. Distinct from sender_test.go's testProvider (records last sent
// Message) — registry tests need call counts to assert that SenderFor-
// returned Senders only invoke the expected provider.
type fakeProvider struct {
	vendor    pb.EmailVendor
	account   string
	err       error
	sentCount int
}

func (p *fakeProvider) Vendor() pb.EmailVendor { return p.vendor }
func (p *fakeProvider) Account() string        { return p.account }
func (p *fakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

// wrap builds a fakeProvider for the given (vendor enum, account, err).
func wrap(vendor pb.EmailVendor, account string, err error) *fakeProvider {
	return &fakeProvider{vendor: vendor, account: account, err: err}
}

func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)

	require.NotNil(t, r.DefaultSender())
	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "u@x.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

func TestNewAccountRegistryFromProviders_singleVendorAccount(t *testing.T) {
	p := wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": p},
	})

	def := r.DefaultSender()
	require.NotNil(t, def)
	result, err := def.Send(context.Background(), &Message{To: "u@x.com"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, p.Vendor(), "vendor impl should report its own vendor")
	require.Equal(t, 1, p.sentCount, "default sender should have called the only provider")
}

// TestNewAccountRegistryFromProviders_sortOrder verifies that the default
// sender's fallback chain is ordered by vendor enum value asc, then account
// name asc.
//
// Map is intentionally unordered: aliyun/zzz, custom_smtp/aaa, custom_smtp/bbb, aliyun/aaa
// Enum values: CUSTOM_SMTP=1, ALIYUN=2 — so custom_smtp comes first.
// Account sort asc within each vendor: aaa < bbb, aaa < zzz.
// Expected fallback chain: custom_smtp/aaa → custom_smtp/bbb → aliyun/aaa → aliyun/zzz
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	aliyunA := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "aaa", errors.New("aliyunA down"))
	aliyunZ := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "zzz", errors.New("aliyunZ down"))
	smtpA := wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "aaa", errors.New("smtpA down"))
	smtpB := wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "bbb", nil) // first to succeed

	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN:      {"zzz": aliyunZ, "aaa": aliyunA},
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"bbb": smtpB, "aaa": smtpA},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "u@x.com"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, result.Vendor, "should fall through to custom_smtp/bbb")
	require.Equal(t, "bbb", result.Account)
	require.Equal(t, 2, result.Attempts, "should try smtpA then smtpB (aliyun not reached)")

	require.Equal(t, 1, smtpA.sentCount)
	require.Equal(t, 1, smtpB.sentCount)
	require.Equal(t, 0, aliyunA.sentCount, "aliyun comes after custom_smtp; never reached")
	require.Equal(t, 0, aliyunZ.sentCount, "aliyun comes after custom_smtp; never reached")
}

func TestSenderFor_bothEmpty(t *testing.T) {
	p := wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": p},
	})

	got, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED, "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got, "both empty should return DefaultSender")
}

func TestSenderFor_bothSet(t *testing.T) {
	target := wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", errors.New("target down"))
	other := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil) // should not be called

	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": target},
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN:      {"primary": other},
	})

	got, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary")
	require.NoError(t, err)
	require.NotSame(t, r.DefaultSender(), got, "specific selection should NOT be the default fallback sender")

	result, err := got.Send(context.Background(), &Message{To: "u@x.com"})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, 1, result.Attempts, "no fallback — should only try the selected provider")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount, "other provider should not have been tried")
}

func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_partialAccountOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email vendor")
}

func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email account")
}

// TestNewAccountRegistry_indexesByVendorAndAccount verifies that
// NewAccountRegistry stores vendor impls directly so the internal map carries
// (vendor, account) identity via the vendor impl's own methods.
func TestNewAccountRegistry_indexesByVendorAndAccount(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.EmailVendor]*VendorConfig{
			pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {Accounts: []*AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	ap, ok := r.vendors[pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP]["primary"]
	require.True(t, ok, "vendors map should index by (vendor, account)")
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, ap.Vendor())
	require.Equal(t, "primary", ap.Account())
}

func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_customSMTPSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.EmailVendor]*VendorConfig{
			pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {Accounts: []*AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestNewAccountRegistry_requiresHost(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.EmailVendor]*VendorConfig{
			pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {Accounts: []*AccountConfig{
				{Name: "primary", Port: 587, From: "noreply@example.com"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires explicit host")
}

func TestNewAccountRegistry_smtpInvalidPort(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.EmailVendor]*VendorConfig{
			pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {Accounts: []*AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 0, From: "noreply@example.com"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp")
}

func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.EmailVendor]*VendorConfig{
			pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {Accounts: []*AccountConfig{
				{Name: "primary", Host: "a.example.com", Port: 587, From: "noreply@x.com"},
				{Name: "primary", Host: "b.example.com", Port: 587, From: "noreply@y.com"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}
```

- [ ] **Step 6: 验证 email 层编译 + 测试**

```bash
gofmt -w internal/provider/email/ internal/service/message/email.go
goimports -w internal/provider/email/ internal/service/message/email.go
go build ./...
go test -race ./internal/provider/email/... ./internal/service/message/...
```

Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/provider/email/sender.go internal/provider/email/registry.go \
        internal/provider/email/sender_test.go internal/provider/email/registry_test.go \
        internal/service/message/email.go
git commit -m "refactor(provider/email): merge Provider/AccountProvider, switch to local SMTPProvider"
```

---

## Task 5: 切换 sms 层到本地包(atomic)

类似 Task 4,但 SMS 还有 router。`Vendor` 用 `pb.SmsVendor`。

**Files:**
- Modify: `internal/provider/sms/sender.go`
- Modify: `internal/provider/sms/registry.go`
- Modify: `internal/provider/sms/router.go`
- Modify: `internal/provider/sms/router_builder.go`
- Modify: `internal/service/message/sms.go`
- Modify: `internal/provider/sms/sender_test.go`
- Modify: `internal/provider/sms/registry_test.go`
- Modify: `internal/provider/sms/router_test.go`
- Modify: `internal/provider/sms/router_builder_test.go`

- [ ] **Step 1: 重写 sender.go**

完整替换 `internal/provider/sms/sender.go`:

```go
// Package sms provides SMS sending with provider fallback and phone-country
// routing.
//
// AccountProvider is an interface implemented in this package (currently
// AliyunProvider in aliyun.go). Composition (Sender, fallback), account
// registry, and routing live in this package too.
package sms

import (
	"context"
	"fmt"
	"time"

	pb "message-service/gen/message/v1"
)

// AccountProvider is the interface vendor implementations satisfy. It carries
// both the proto-enum Vendor identity and the per-account name, so Sender,
// Router, and Registry can return enough context for record persistence
// without an external wrapper layer.
type AccountProvider interface {
	Vendor() pb.SmsVendor
	Account() string
	Send(ctx context.Context, msg *Message) error
}

// SendResult captures the outcome of a Send call.
type SendResult struct {
	Message  *Message      // the message that was sent
	Vendor   pb.SmsVendor  // vendor that handled the request (or last tried)
	Account  string        // account identifier of the vendor provider used
	Target   string        // recipient phone number
	Success  bool          // whether the send succeeded
	Error    error         // last error if all providers failed
	Duration time.Duration // total time across all provider attempts
	Attempts int           // number of providers tried
}

// Sender sends SMS with automatic fallback between providers.
type Sender struct {
	providers []AccountProvider
}

// NewSender creates a Sender that tries providers in order.
func NewSender(providers []AccountProvider) *Sender {
	return &Sender{providers: providers}
}

// Send tries providers in order, falling back on failure.
//
// Return contract:
//   - Validation error (empty recipient, no provider): (nil, err)
//   - Success: (result with Success=true, nil)
//   - All providers failed: (result with Success=false, wrapped err)
//   - Context cancelled: (result with Success=false, ctx.Err())
func (s *Sender) Send(ctx context.Context, msg *Message) (*SendResult, error) {
	if msg.To == "" {
		return nil, fmt.Errorf("sms: phone number is empty")
	}

	if len(s.providers) == 0 {
		return nil, fmt.Errorf("sms: no provider available")
	}

	start := time.Now()
	var lastErr error
	var lastVendor pb.SmsVendor
	var lastAccount string
	attempts := 0

	for _, p := range s.providers {
		if ctx.Err() != nil {
			return &SendResult{
				Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
				Success: false, Error: ctx.Err(),
				Duration: time.Since(start), Attempts: attempts,
			}, ctx.Err()
		}

		attempts++
		lastVendor = p.Vendor()
		lastAccount = p.Account()
		if err := p.Send(ctx, msg); err != nil {
			lastErr = err
			continue
		}

		return &SendResult{
			Message: msg, Vendor: p.Vendor(), Account: p.Account(), Target: msg.To,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}

	return &SendResult{
		Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("sms: all providers failed, last error: %w", lastErr)
}
```

- [ ] **Step 2: 重写 registry.go**

完整替换 `internal/provider/sms/registry.go`。关键差异:
- `vendors` map 类型 `*AccountProvider` → `AccountProvider`
- `buildProvider` 直接调用同包 `NewAliyunProvider`
- 删除 `aliyunprovider` import

```go
package sms

import (
	"fmt"
	"sort"

	pb "message-service/gen/message/v1"
)

// Config is the cooked config: vendor keys and route target vendors are proto
// enums, parsed from YAML strings by ParseVendorName during config.Load
// (fail-fast on unknown vendor).
//
// DefaultCountry is used by BuildRouter as the default country when parsing
// phone numbers without an international prefix (e.g., "13800138000" with
// DefaultCountry "CN" parses as China).
type Config struct {
	DefaultCountry string `default:"CN"` // ISO 3166-1 alpha-2, default "CN"
	Vendors        map[pb.SmsVendor]*VendorConfig
	Routes         []*RouteConfig
}

// VendorConfig holds accounts under one vendor (e.g., ALIYUN).
type VendorConfig struct {
	Accounts []*AccountConfig
}

// RouteConfig is one country's route targets.
type RouteConfig struct {
	Country string // ISO 3166-1 alpha-2, or "*" for fallback
	Targets []*RouteTarget
}

// RouteTarget references a (vendor, account) pair already defined in Vendors.
type RouteTarget struct {
	Vendor  pb.SmsVendor
	Account string
}

// AccountConfig is a single named SMS account. fat-struct design: adding a
// new vendor means adding fields here.
type AccountConfig struct {
	Name            string
	AccessKeyID     string // aliyun
	AccessKeySecret string // aliyun
	SignName        string // aliyun
	RegionID        string // aliyun
}

// AccountRegistry indexes AccountProviders by (vendor, account) and exposes
// both a default fallback sender and per-account senders.
type AccountRegistry struct {
	vendors map[pb.SmsVendor]map[string]AccountProvider
	def     *Sender
}

// NewAccountRegistryFromProviders builds a registry from a pre-built provider
// map keyed by proto enum. The default fallback chain is ordered by vendor
// enum value asc, then account name asc.
func NewAccountRegistryFromProviders(vendors map[pb.SmsVendor]map[string]AccountProvider) *AccountRegistry {
	r := &AccountRegistry{
		vendors: vendors,
	}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// NewAccountRegistry constructs a registry from cooked config (enum-keyed
// Vendors map). Vendor name strings have already been validated and converted
// to enums by config.Load via ParseVendorName.
//
// Behavior:
//   - Iterates each vendor, calling the corresponding constructor.
//   - Each constructed AccountProvider is stored directly (vendor impls
//     carry their own Vendor/Account identity — no external wrapper).
//   - Duplicate account names within the same vendor are rejected.
//   - Provider construction failures return an error.
//
// DefaultCountry field is preserved for BuildRouter (see Config comment).
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[pb.SmsVendor]map[string]AccountProvider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for vendorEnum, vc := range cfg.Vendors {
		accounts := make(map[string]AccountProvider)
		for _, ac := range vc.Accounts {
			if _, dup := accounts[ac.Name]; dup {
				return nil, fmt.Errorf("sms: duplicate account name %q under vendor %s", ac.Name, vendorEnum)
			}
			p, err := buildProvider(vendorEnum, ac)
			if err != nil {
				return nil, fmt.Errorf("sms: account %s/%s: %w", vendorEnum, ac.Name, err)
			}
			accounts[ac.Name] = p
		}
		vendors[vendorEnum] = accounts
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// DefaultSender returns the fallback sender containing all providers in the
// order determined at construction time.
func (r *AccountRegistry) DefaultSender() *Sender {
	return r.def
}

// SenderFor selects a sender based on vendor+account.
func (r *AccountRegistry) SenderFor(vendor pb.SmsVendor, account string) (*Sender, error) {
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED && account == "" {
		return r.def, nil
	}
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("sms: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown sms vendor %s", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown sms account %q under vendor %s", account, vendor)
	}
	return NewSender([]AccountProvider{p}), nil
}

// --- internal helpers ---

// lookup returns the AccountProvider for (vendor, account). Used by BuildRouter
// to resolve route targets at startup.
func (r *AccountRegistry) lookup(vendor pb.SmsVendor, account string) (AccountProvider, error) {
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("sms: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown sms vendor %s", vendor)
	}
	ap, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown sms account %q under vendor %s", account, vendor)
	}
	return ap, nil
}

// buildProvider dispatches to the corresponding constructor based on vendor
// enum and returns the constructed AccountProvider.
func buildProvider(vendor pb.SmsVendor, ac *AccountConfig) (AccountProvider, error) {
	switch vendor {
	case pb.SmsVendor_SMS_VENDOR_ALIYUN:
		return NewAliyunProvider(ac.Name, &AliyunConfig{
			AccessKeyID:     ac.AccessKeyID,
			AccessKeySecret: ac.AccessKeySecret,
			SignName:        ac.SignName,
			RegionID:        ac.RegionID,
		})
	default:
		return nil, fmt.Errorf("unknown vendor %s", vendor)
	}
}

// flattenProviders expands the nested map into a flat slice ordered by vendor
// enum value asc, then account name asc.
func flattenProviders(vendors map[pb.SmsVendor]map[string]AccountProvider) []AccountProvider {
	vendorEnums := make([]pb.SmsVendor, 0, len(vendors))
	for v := range vendors {
		vendorEnums = append(vendorEnums, v)
	}
	sort.Slice(vendorEnums, func(i, j int) bool { return vendorEnums[i] < vendorEnums[j] })

	var out []AccountProvider
	for _, v := range vendorEnums {
		accounts := vendors[v]
		acctNames := make([]string, 0, len(accounts))
		for a := range accounts {
			acctNames = append(acctNames, a)
		}
		sort.Strings(acctNames)
		for _, a := range acctNames {
			out = append(out, accounts[a])
		}
	}
	return out
}
```

- [ ] **Step 3: 重写 router.go**

完整替换 `internal/provider/sms/router.go`。关键差异:`Route.Targets` 类型 `[]*AccountProvider` → `[]AccountProvider`;`Router.defaultTargets` 和 `Router.routes` 同步;`ap.Vendor`/`ap.Account` 字段访问 → `ap.Vendor()`/`ap.Account()` 方法调用;新增 `pb` import。

```go
package sms

import (
	"context"
	"fmt"
	"time"

	"github.com/nyaruka/phonenumbers"

	pb "message-service/gen/message/v1"
)

// Route maps a country to its SMS route targets.
type Route struct {
	Country string            // ISO 3166-1 alpha-2, e.g., "CN", "US", "HK"
	Targets []AccountProvider // ordered targets with fallback
}

// Router routes SMS to providers based on phone number country.
type Router struct {
	defaultCountry string
	defaultTargets []AccountProvider
	routes         map[string][]AccountProvider
}

// NewRouter creates a Router. defaultCountry is used when phone numbers lack
// an international prefix (e.g., "13800138000" with defaultCountry "CN" parses as China).
func NewRouter(defaultCountry string, defaultTargets []AccountProvider, routes ...Route) *Router {
	m := make(map[string][]AccountProvider, len(routes))
	for _, r := range routes {
		m[r.Country] = r.Targets
	}
	return &Router{
		defaultCountry: defaultCountry,
		defaultTargets: defaultTargets,
		routes:         m,
	}
}

// Send routes the message to the appropriate target based on the recipient phone number.
//
// Return contract mirrors Sender.Send:
//   - Validation / parse error / no target: (nil, err)
//   - Success: (result with Success=true, nil)
//   - All targets failed: (result with Success=false, wrapped err)
//   - Context cancelled: (result with Success=false, ctx.Err())
//
// The ctx.Err() check happens inside the target loop (not at the top) so a
// pre-cancelled context still produces a Success=false SendResult for the
// service layer to persist — matching Sender.Send's behaviour.
func (r *Router) Send(ctx context.Context, msg *Message) (*SendResult, error) {
	if msg.To == "" {
		return nil, fmt.Errorf("sms: phone number is empty")
	}

	num, err := phonenumbers.Parse(msg.To, r.defaultCountry)
	if err != nil {
		return nil, fmt.Errorf("sms: invalid phone number %q: %w", msg.To, err)
	}

	country := phonenumbers.GetRegionCodeForNumber(num)

	targets := r.defaultTargets
	if t, ok := r.routes[country]; ok {
		targets = t
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("sms: no route target for %s (%s)", msg.To, country)
	}

	start := time.Now()
	var lastErr error
	var lastVendor pb.SmsVendor
	var lastAccount string
	attempts := 0
	for _, ap := range targets {
		if ctx.Err() != nil {
			return &SendResult{
				Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
				Success: false, Error: ctx.Err(),
				Duration: time.Since(start), Attempts: attempts,
			}, ctx.Err()
		}
		attempts++
		lastVendor = ap.Vendor()
		lastAccount = ap.Account()
		if err := ap.Send(ctx, msg); err != nil {
			lastErr = err
			continue
		}
		return &SendResult{
			Message: msg, Vendor: ap.Vendor(), Account: ap.Account(), Target: msg.To,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}
	return &SendResult{
		Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("sms: all targets failed for %s, last error: %w", msg.To, lastErr)
}
```

- [ ] **Step 4: 改造 router_builder.go**

`router_builder.go` 只有 `*AccountProvider` → `AccountProvider` 类型引用变化:

```go
package sms

import (
	"fmt"
)

// BuildRouter constructs a Router from Config + AccountRegistry. Each RouteTarget
// must reference a (vendor, account) already defined in Config.Vendors; otherwise
// construction fails (fail-fast at startup).
//
// The country "*" is treated as the default fallback chain — its targets become
// the Router's defaultTargets. Other countries become Route entries. If multiple
// "*" entries exist, the last one wins (undefined behavior — treat as config error).
//
// Returns (nil, nil) if cfg is nil or cfg.Routes is empty — caller decides
// whether that's an error (sendSMS path treats empty routes as misconfiguration
// and rejects vendor/account-empty requests with BadRequest).
func BuildRouter(cfg *Config, reg *AccountRegistry) (*Router, error) {
	if cfg == nil || len(cfg.Routes) == 0 {
		return nil, nil
	}

	defaultCountry := cfg.DefaultCountry
	if defaultCountry == "" {
		defaultCountry = "CN"
	}

	var defaultTargets []AccountProvider
	routes := make([]Route, 0, len(cfg.Routes))

	for _, rc := range cfg.Routes {
		targets := make([]AccountProvider, 0, len(rc.Targets))
		for _, t := range rc.Targets {
			ap, err := reg.lookup(t.Vendor, t.Account)
			if err != nil {
				return nil, fmt.Errorf("sms: route %s: %w", rc.Country, err)
			}
			targets = append(targets, ap)
		}
		if rc.Country == "*" {
			defaultTargets = targets
			continue
		}
		routes = append(routes, Route{Country: rc.Country, Targets: targets})
	}

	return NewRouter(defaultCountry, defaultTargets, routes...), nil
}
```

- [ ] **Step 5: 改造 service/message/sms.go**

5 处改动(与 email 同构):

1. **import 块**:删除 `smscommon "github.com/servekit/go-common/message/sms"` 行。

2. **第 44 行**:`smscommon.Message` → `sms.Message`

   ```go
   msg := &sms.Message{
       To:       req.GetTo(),
       Content:  req.GetContent(),
       Template: req.GetTemplateId(),
       Params:   models.MapStringString(req.GetTemplateParams()),
   }
   ```

3. **第 81-82 行**:日志 `result.Vendor` → `result.Vendor.String()`

   ```go
   return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
       "vendor=%s account=%s attempts=%d",
       result.Vendor.String(), result.Account, result.Attempts)
   ```

4. **第 89 行**:`pb.SmsVendor(pb.SmsVendor_value[result.Vendor])` → `result.Vendor`

   ```go
   Vendor: &pb.SendResponse_SmsVendor{
       SmsVendor: result.Vendor,
   },
   ```

5. **第 290 行**:`pb.SmsVendor_value[result.Vendor]` → `int32(result.Vendor)`

   ```go
   record := &models.MessageSMSRecord{
       ID:             id,
       Vendor:         int32(result.Vendor),
       Account:        result.Account,
       ...
   }
   ```

- [ ] **Step 6: 重写 sender_test.go**

完整替换 `internal/provider/sms/sender_test.go`。`testProvider` 实现 interface:

```go
package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// testProvider is a sender-level test AccountProvider that records the last
// sent Message. Distinct from registry_test.go's fakeProvider (counts Send
// calls).
type testProvider struct {
	vendor  pb.SmsVendor
	account string
	err     error
	sent    *Message
}

func (p *testProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *testProvider) Account() string       { return p.account }
func (p *testProvider) Send(_ context.Context, msg *Message) error {
	if p.err != nil {
		return p.err
	}
	p.sent = msg
	return nil
}

func TestSender_Send_singleProvider(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	msg := &Message{To: "13800138000", Content: "Your code is 123456"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "default", result.Account)
	require.Equal(t, 1, result.Attempts)
	if diff := cmp.Diff(msg, p.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_fallback(t *testing.T) {
	// SMS proto only defines ALIYUN; re-use ALIYUN as the fallback vendor —
	// the Sender logic does not branch on vendor identity.
	p1 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: errors.New("aliyun down")}
	p2 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "backup"}
	s := NewSender([]AccountProvider{p1, p2})

	msg := &Message{To: "13800138000", Content: "Hello"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Nil(t, p1.sent)
	if diff := cmp.Diff(msg, p2.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_allFail(t *testing.T) {
	p1 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: errors.New("timeout")}
	p2 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "backup", err: errors.New("rate limited")}
	s := NewSender([]AccountProvider{p1, p2})

	result, err := s.Send(context.Background(), &Message{To: "13800138000", Content: "Hello"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all providers failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Error(t, result.Error)
}

func TestSender_Send_noProvider(t *testing.T) {
	s := NewSender(nil)
	result, err := s.Send(context.Background(), &Message{To: "13800138000", Content: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_emptyPhone(t *testing.T) {
	s := NewSender([]AccountProvider{
		&testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := s.Send(context.Background(), &Message{To: "", Content: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "phone number is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_cancelledContext(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := s.Send(ctx, &Message{To: "13800138000", Content: "test"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestSender_Send_recordsVendorAndAccount(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary"}
	s := NewSender([]AccountProvider{p})

	result, err := s.Send(context.Background(), &Message{To: "13800138000", Content: "Hello"})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
}
```

- [ ] **Step 7: 重写 registry_test.go**

完整替换 `internal/provider/sms/registry_test.go`:

```go
package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// fakeProvider is a registry-level test AccountProvider that counts Send
// calls. Distinct from sender_test.go's testProvider (records last sent
// Message) and router_test.go's trackProvider (records only whether Send was
// called).
type fakeProvider struct {
	vendor    pb.SmsVendor
	account   string
	err       error
	sentCount int
}

func (p *fakeProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *fakeProvider) Account() string       { return p.account }
func (p *fakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

// wrap builds a fakeProvider for the given (account, err). All SMS fakes use
// ALIYUN as the vendor — SMS proto only defines ALIYUN.
func wrap(account string, err error) *fakeProvider {
	return &fakeProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: account, err: err}
}

func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)

	require.NotNil(t, r.DefaultSender())
	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

func TestNewAccountRegistryFromProviders_singleVendorAccount(t *testing.T) {
	p := wrap("primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": p},
	})

	def := r.DefaultSender()
	require.NotNil(t, def)
	result, err := def.Send(context.Background(), &Message{To: "13800138000"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, p.Vendor(), "vendor impl should report its own vendor")
	require.Equal(t, 1, p.sentCount, "default sender should have called the only provider")
}

// TestNewAccountRegistryFromProviders_sortOrder verifies same-vendor
// multi-account ordering (SMS proto only has ALIYUN; multi-vendor sort is
// exercised in email registry tests).
//
// Map is intentionally unordered: aliyun/zzz, aliyun/aaa, aliyun/bbb
// Expected fallback chain: aliyun/aaa → aliyun/bbb → aliyun/zzz
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	alA := wrap("aaa", errors.New("alA down"))
	alB := wrap("bbb", errors.New("alB down"))
	alZ := wrap("zzz", nil) // first to succeed

	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {
			"zzz": alZ,
			"aaa": alA,
			"bbb": alB,
		},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor, "should fall through to aliyun/zzz after aaa/bbb fail")
	require.Equal(t, "zzz", result.Account)
	require.Equal(t, 3, result.Attempts, "should try aaa → bbb → zzz")

	require.Equal(t, 1, alA.sentCount)
	require.Equal(t, 1, alB.sentCount)
	require.Equal(t, 1, alZ.sentCount)
}

func TestSenderFor_bothEmpty(t *testing.T) {
	p := wrap("primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": p},
	})

	got, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_UNSPECIFIED, "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got, "both empty should return DefaultSender")
}

func TestSenderFor_bothSet(t *testing.T) {
	target := wrap("primary", errors.New("target down"))
	other := wrap("backup", nil) // should not be called

	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {
			"primary": target,
			"backup":  other,
		},
	})

	got, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotSame(t, r.DefaultSender(), got, "specific selection should NOT be the default fallback sender")

	result, err := got.Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, 1, result.Attempts, "no fallback — should only try the selected provider")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount, "other provider should not have been tried")
}

func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_partialAccountOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_UNSPECIFIED, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor(99), "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms vendor")
}

func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms account")
}

// TestNewAccountRegistry_indexesByVendorAndAccount verifies that
// NewAccountRegistry stores vendor impls directly so the internal map carries
// (vendor, account) identity via the vendor impl's own methods.
func TestNewAccountRegistry_indexesByVendorAndAccount(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", AccessKeyID: "xxx", AccessKeySecret: "yyy", SignName: "sign", RegionID: "cn-hangzhou"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	ap, ok := r.vendors[pb.SmsVendor_SMS_VENDOR_ALIYUN]["primary"]
	require.True(t, ok, "vendors map should index by (vendor, account)")
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, ap.Vendor())
	require.Equal(t, "primary", ap.Account())
}

func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_aliyunSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", AccessKeyID: "xxx", AccessKeySecret: "yyy", SignName: "sign", RegionID: "cn-hangzhou"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", AccessKeyID: "a", AccessKeySecret: "b", SignName: "s", RegionID: "cn-hangzhou"},
				{Name: "primary", AccessKeyID: "c", AccessKeySecret: "d", SignName: "s2", RegionID: "cn-hangzhou"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestNewAccountRegistry_defaultCountryPreserved(t *testing.T) {
	cfg := &Config{
		DefaultCountry: "US",
		Vendors:        map[pb.SmsVendor]*VendorConfig{},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.Equal(t, "US", cfg.DefaultCountry, "field should be preserved on the input config")
	_ = r
}
```

- [ ] **Step 8: 重写 router_test.go**

完整替换 `internal/provider/sms/router_test.go`。`trackProvider` 实现 interface:

```go
package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

// trackProvider records whether Send was called.
type trackProvider struct {
	vendor  pb.SmsVendor
	account string
	err     error
	called  bool
}

func (p *trackProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *trackProvider) Account() string       { return p.account }
func (p *trackProvider) Send(_ context.Context, _ *Message) error {
	p.called = true
	return p.err
}

func TestRouter_Send_chinaNumber(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", Content: "test"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_chinaNumberWithoutPlus(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "13800138000", Content: "test"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_internationalNumber(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+819012345678", Content: "test"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, cn.called)
	require.True(t, def.called)
}

func TestRouter_Send_multipleCountries(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	hk := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "hk"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
		Route{Country: "HK", Targets: []AccountProvider{hk}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+85291234567", Content: "test"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, hk.called)
	require.False(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_fallbackWithinCountry(t *testing.T) {
	p1 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: fmt.Errorf("timeout")}
	p2 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "secondary"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{p1, p2}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", Content: "test"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "secondary", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.True(t, p1.called)
	require.True(t, p2.called)
	require.False(t, def.called)
}

func TestRouter_Send_allProvidersFail(t *testing.T) {
	p1 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: fmt.Errorf("timeout")}
	p2 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "secondary", err: fmt.Errorf("rate limit")}

	router := NewRouter("CN", nil,
		Route{Country: "CN", Targets: []AccountProvider{p1, p2}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", Content: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all targets failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "secondary", result.Account)
	require.Equal(t, 2, result.Attempts)
}

func TestRouter_Send_emptyPhone(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := router.Send(context.Background(), &Message{To: "", Content: "test"})
	require.EqualError(t, err, "sms: phone number is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_invalidPhone(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := router.Send(context.Background(), &Message{To: "not-a-number", Content: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid phone number")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_noProvider(t *testing.T) {
	router := NewRouter("CN", nil)
	result, err := router.Send(context.Background(), &Message{To: "+819012345678", Content: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no route target for")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_cancelledContext(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := router.Send(ctx, &Message{To: "+8613800138000", Content: "test"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestRouter_Send_recordsAccount(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	router := NewRouter("CN", nil, Route{Country: "CN", Targets: []AccountProvider{cn}})

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", Content: "test"})
	require.NoError(t, err)
	require.Equal(t, "cn", result.Account)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
}
```

- [ ] **Step 9: 重写 router_builder_test.go**

完整替换 `internal/provider/sms/router_builder_test.go`:

```go
package sms

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	pb "message-service/gen/message/v1"
)

func TestBuildRouter_emptyConfig(t *testing.T) {
	reg := NewAccountRegistryFromProviders(nil)
	r, err := BuildRouter(nil, reg)
	require.NoError(t, err)
	require.Nil(t, r, "empty routes → nil router (caller decides if that's error)")
}

func TestBuildRouter_emptyRoutes(t *testing.T) {
	reg := NewAccountRegistryFromProviders(nil)
	r, err := BuildRouter(&Config{}, reg)
	require.NoError(t, err)
	require.Nil(t, r)
}

func TestBuildRouter_validRoutes(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": cn},
	})
	cfg := &Config{
		DefaultCountry: "CN",
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
			{Country: "*", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
		},
	}

	r, err := BuildRouter(cfg, reg)
	require.NoError(t, err)
	require.NotNil(t, r)

	result, err := r.Send(context.Background(), &Message{To: "+8613800138000", Content: "hi"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "default", result.Account)
}

func TestBuildRouter_unknownAccount(t *testing.T) {
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}},
	})
	cfg := &Config{
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "primary"}}},
		},
	}

	_, err := BuildRouter(cfg, reg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms account")
}

func TestBuildRouter_defaultCountryFallback(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": cn},
	})
	cfg := &Config{
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
		},
	}

	r, err := BuildRouter(cfg, reg)
	require.NoError(t, err)
	require.NotNil(t, r)
	result, err := r.Send(context.Background(), &Message{To: "13800138000", Content: "hi"})
	require.NoError(t, err)
	require.True(t, result.Success)
}
```

- [ ] **Step 10: 验证 sms 层编译 + 测试**

```bash
gofmt -w internal/provider/sms/ internal/service/message/sms.go
goimports -w internal/provider/sms/ internal/service/message/sms.go
go build ./...
go test -race ./internal/provider/sms/... ./internal/service/message/...
```

Expected: 全部 PASS

- [ ] **Step 11: Commit**

```bash
git add internal/provider/sms/ internal/service/message/sms.go
git commit -m "refactor(provider/sms): merge Provider/AccountProvider, switch to local AliyunProvider"
```

---

## Task 6: 删除 go-common/message

确认 message-service 已完全脱离 go-common/message 后,删除整个目录。

**Files:**
- Delete: `go-common/message/` 整个目录

- [ ] **Step 1: 确认 message-service 不再引用 go-common/message**

```bash
grep -rn "go-common/message" /Users/moss/code/base/message-service --include="*.go"
```

Expected: 无输出(零引用)

- [ ] **Step 2: 确认 go-common 内部其他模块也不引用 message**

```bash
grep -rn "go-common/message" /Users/moss/code/base/go-common --include="*.go"
```

Expected: 只剩 `go-common/message/` 目录内部的相互引用。

- [ ] **Step 3: 删除 go-common/message 目录**

```bash
rm -rf /Users/moss/code/base/go-common/message
```

- [ ] **Step 4: 验证 message-service 仍能编译**

```bash
cd /Users/moss/code/base/message-service
go build ./...
```

Expected: PASS(go-common 的 message 子包没了,但 message-service 已经不引用,所以构建不受影响)

- [ ] **Step 5: 在 go-common 仓库提交删除**

```bash
cd /Users/moss/code/base/go-common
git status
git add -A
git commit -m "refactor(message): remove inlined message package

The message package was inlined into message-service; this package had
no other consumers. Removes Provider interface, Message struct, SMTP
and Aliyun vendor implementations."
```

注意:此 commit 在 `go-common` 仓库,message-service 仓库无变化。

- [ ] **Step 6: (可选)清理 go-common/go.mod**

如果 `go-common/go.mod` 有专为 message 引入的依赖(`github.com/wneessen/go-mail`、`github.com/alibabacloud-go/dysmsapi-20170525/v5` 等),用 `go mod tidy` 清理:

```bash
cd /Users/moss/code/base/go-common
go mod tidy
git diff go.mod go.sum
```

如有变化,补充一个 commit:

```bash
git add go.mod go.sum
git commit -m "chore: go mod tidy after removing message package"
```

如无变化,跳过此 step。

---

## Task 7: 最终验证

跑全套质量门禁,确保整个迁移无回归。

- [ ] **Step 1: 全量格式化 + lint**

```bash
cd /Users/moss/code/base/message-service
gofmt -l .
goimports -l .
golangci-lint run ./...
```

Expected: 三个命令都无输出或全部 PASS。

- [ ] **Step 2: 全量构建**

```bash
go build ./...
go vet ./...
```

Expected: PASS

- [ ] **Step 3: 全量测试**

```bash
go test -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
```

Expected: 全部测试 PASS。覆盖率不低于迁移前(参考基线:`go test -race -cover ./...` 在迁移前的数值)。

- [ ] **Step 4: 验证 git 历史**

```bash
git log --oneline -8
```

Expected: 看到 Tasks 1-5 的 5 个 commit(在 message-service 仓库)。Task 6 在 go-common 仓库另有 1-2 个 commit。

- [ ] **Step 5: 跨仓库 grep 终检**

```bash
grep -rn "go-common/message" /Users/moss/code/base --include="*.go"
```

Expected: 无输出。整个 base/ 下再无 `go-common/message` 引用。

---

## Self-Review Checklist

执行完所有 Task 后,过一遍 spec 的每个章节,确认无遗漏:

**Spec 覆盖**:
- §目录结构 → Tasks 1-5 全部覆盖(message.go + smtp.go + aliyun.go + sender + registry + router)
- §AccountProvider 形态 → Task 4/5(struct→interface + Vendor() enum + Account())
- §数据流 → Task 4/5 后,vendor impl 自带 Vendor/Account,registry 直接调用 `NewSMTPProvider`/`NewAliyunProvider`(同包函数)
- §错误处理 → 保持原状,vendor impl 仍用 fmt.Errorf
- §测试策略 → Tasks 2/3 平移原测试 + 加 Vendor/Account 测试;Tasks 4/5 mock 改造
- §兼容性 → Task 6 删除 go-common/message,Task 7 grep 验证零引用
- §实施顺序 → 完全按 spec §实施顺序的 9 步执行(拆为 7 个 task)

**类型一致性**:
- `AccountProvider` 在 sender.go/registry.go/router.go 全部为 interface
- `Sender.providers` 在 email/sms 全部为 `[]AccountProvider`
- `SendResult.Vendor` 在 email 为 `pb.EmailVendor`,在 sms 为 `pb.SmsVendor`
- vendor 构造函数:`NewSMTPProvider(account, *SMTPConfig)`(email 包内)/ `NewAliyunProvider(account, *AliyunConfig)`(sms 包内)
- mock 命名:email 用 `testProvider`(sender_test) + `fakeProvider`(registry_test);sms 用 `testProvider`(sender_test) + `fakeProvider`(registry_test) + `trackProvider`(router_test/router_builder_test)
- vendor 测试 helper 命名加 vendor 前缀避免后续冲突:`aliyunMockSender`/`aliyunOkResponse`/`aliyunErrResponse`(aliyun_test.go)

**Placeholder 扫描**:每个 step 都有完整代码或精确改动描述,无 TODO/TBD。

**类型细节验证**(实施时确认):
- `router.go` 必须加 `pb "message-service/gen/message/v1"` import(因为 `lastVendor pb.SmsVendor`)
- `aliyun.go` 的 struct 字段对齐跑 `gofmt -w` 修正
- service/message/email.go 和 sms.go 的 import 块**只删 go-common/message 行**,保留 `go-common/dbx` 行
- vendor impl 文件不 import 父包(同包内引用 Message / AccountProvider)
