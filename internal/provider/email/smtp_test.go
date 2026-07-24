package email

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pb "github.com/servekit/message-service/gen/message/v1"
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
	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account, &SMTPConfig{
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
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, p.Vendor())
}

func TestSMTPProvider_Account(t *testing.T) {
	p := newSMTPTestProvider(t, "secondary", "localhost", 1025)
	require.Equal(t, "secondary", p.Account())
}

func TestSMTPProvider_Send_success(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "Hello", Body: "World"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_emptySubject(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: []*Address{{Email: "user@test.com"}}, Body: "No subject"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_emptyRecipient(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "localhost", 1025)
	err := p.Send(context.Background(), &Message{To: nil, Body: "test"})
	require.EqualError(t, err, "smtp: at least one recipient is required")
}

func TestSMTPProvider_Send_connectionRefused(t *testing.T) {
	p := newSMTPTestProvider(t, "primary", "127.0.0.1", 19999)
	err := p.Send(context.Background(), &Message{To: []*Address{{Email: "user@test.com"}}, Body: "test"})
	require.Error(t, err)
}

func TestSMTPProvider_Send_emptyBody(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "No body"}
	err := p.Send(context.Background(), msg)
	require.NoError(t, err)
}

func TestSMTPProvider_Send_withCc(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:      []*Address{{Email: "user@test.com"}},
		Cc:      []*Address{{Email: "cc1@test.com"}, {Email: "cc2@test.com"}},
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
		To:      []*Address{{Email: "user@test.com"}},
		Bcc:     []*Address{{Email: "bcc@test.com"}},
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
		To:       []*Address{{Email: "user@test.com"}},
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
		To:       []*Address{{Email: "user@test.com"}},
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
		To:      []*Address{{Email: "user@test.com"}},
		ReplyTo: &Address{Email: "reply@example.com"},
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
	_, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", &SMTPConfig{
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

	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "not-valid-email",
	})
	require.NoError(t, err)

	err = p.Send(context.Background(), &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "Hi", Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp: invalid from address")
}

func TestSMTPProvider_vendorPropagation(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)

	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.NoError(t, err)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, p.Vendor(),
		"Vendor() must return the vendor passed to NewSMTPProvider")
}

// TestSMTPProvider_Send_fromDefault verifies that an empty msg.From falls
// back to the provider's configured From. Same trick: configure an invalid
// default; if the fallback works, Send fails.
func TestSMTPProvider_Send_fromDefault(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "not-valid-email", // invalid default
	})
	require.NoError(t, err)

	msg := &Message{
		To:      []*Address{{Email: "user@test.com"}},
		Subject: "Default",
		Body:    "Hello",
		// From intentionally empty — falls back to provider default
	}
	err = p.Send(context.Background(), msg)
	require.Error(t, err, "default must be used (and rejected as invalid)")
	require.Contains(t, err.Error(), "invalid from address")
}

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

// TestSMTPProvider_Send_inlineMissingContentID verifies the inline-attachment
// guard: an Inline=true attachment with empty ContentID must be rejected
// before the SMTP send proceeds (a CID is required to resolve cid: references
// in the HTML body).
func TestSMTPProvider_Send_inlineMissingContentID(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	p := newSMTPTestProvider(t, "primary", host, mustAtoi(port))

	msg := &Message{
		To:       []*Address{{Email: "user@test.com"}},
		Subject:  "x",
		HTMLBody: `<img src="cid:logo">`,
		Attachments: []*Attachment{
			{Filename: "logo.png", Content: []byte{0x89, 'P', 'N', 'G'}, Inline: true, ContentID: ""},
		},
	}
	err := p.Send(context.Background(), msg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ContentID")
}
