package email

import (
	"bytes"
	"context"
	"fmt"
	"net/mail"

	gomail "github.com/wneessen/go-mail"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// SMTPConfig holds the configuration for the SMTP provider.
type SMTPConfig struct {
	Host     string
	Port     int `default:"587"` // STARTTLS submission port; WithPort rejects 0
	Username string
	Password string
	From     string
}

// SMTPProvider sends emails via SMTP using a reusable client.
type SMTPProvider struct {
	vendor  pb.EmailVendor
	account string
	client  *gomail.Client
	from    string
}

// NewSMTPProvider creates an SMTP provider. The mail.Client is initialized
// once and reused across Send calls (each call still dials a new connection
// via DialAndSendWithContext, which is safe for concurrent use).
//
// vendor carries the brand identity (e.g. ALIYUN, TENCENT) so audit and
// selection work without an external wrapper layer.
func NewSMTPProvider(vendor pb.EmailVendor, account string, config *SMTPConfig) (*SMTPProvider, error) {
	client, err := gomail.NewClient(config.Host,
		gomail.WithPort(config.Port),
		gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
		gomail.WithUsername(config.Username),
		gomail.WithPassword(config.Password),
		gomail.WithTLSPortPolicy(gomail.TLSOpportunistic),
	)
	if err != nil {
		return nil, fmt.Errorf("smtp: create client: %w", err)
	}
	return &SMTPProvider{vendor: vendor, account: account, client: client, from: config.From}, nil
}

// Vendor returns the brand this account belongs to (set at construction).
func (p *SMTPProvider) Vendor() pb.EmailVendor { return p.vendor }

// Account returns the account name this provider was constructed with.
func (p *SMTPProvider) Account() string { return p.account }

// Send sends an email via SMTP. Address formatting is delegated to go-mail
// (FromMailAddress / ToMailAddress / etc.) — we only do a thin field copy
// from our Address type to stdlib mail.Address. If display_name needs
// validation (e.g. reject control characters), do it before this point.
func (p *SMTPProvider) Send(ctx context.Context, msg *Message) error {
	if len(msg.To) == 0 {
		return fmt.Errorf("smtp: at least one recipient is required")
	}

	m := gomail.NewMsg()
	if msg.From != nil {
		m.FromMailAddress(toMailAddress(msg.From))
	} else {
		if err := m.From(p.from); err != nil {
			return fmt.Errorf("smtp: invalid from address: %w", err)
		}
	}
	m.ToMailAddress(toMailAddresses(msg.To)...)
	if len(msg.Cc) > 0 {
		m.CcMailAddress(toMailAddresses(msg.Cc)...)
	}
	if len(msg.Bcc) > 0 {
		m.BccMailAddress(toMailAddresses(msg.Bcc)...)
	}
	if msg.ReplyTo != nil {
		m.ReplyToMailAddress(toMailAddress(msg.ReplyTo))
	}

	m.Subject(msg.Subject)

	if msg.HTMLBody != "" {
		m.SetBodyString(gomail.TypeTextHTML, msg.HTMLBody)
		if msg.Body != "" {
			m.AddAlternativeString(gomail.TypeTextPlain, msg.Body)
		}
	} else {
		m.SetBodyString(gomail.TypeTextPlain, msg.Body)
	}

	for _, att := range msg.Attachments {
		if att == nil {
			continue
		}
		reader := bytes.NewReader(att.Content)
		var opts []gomail.FileOption
		if att.MimeType != "" {
			opts = append(opts, gomail.WithFileContentType(gomail.ContentType(att.MimeType)))
		}
		if att.Inline {
			if att.ContentID == "" {
				return fmt.Errorf("smtp: inline attachment %q missing ContentID", att.Filename)
			}
			opts = append(opts, gomail.WithFileContentID(att.ContentID))
			if err := m.EmbedReader(att.Filename, reader, opts...); err != nil {
				return fmt.Errorf("smtp: embed %q: %w", att.Filename, err)
			}
		} else {
			if err := m.AttachReader(att.Filename, reader, opts...); err != nil {
				return fmt.Errorf("smtp: attach %q: %w", att.Filename, err)
			}
		}
	}

	if err := p.client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// --- internal helpers ---

// toMailAddress converts our Address to a stdlib mail.Address — pure field
// copy. Returns nil for nil input so callers can pass optional fields
// through without conditionals.
func toMailAddress(a *Address) *mail.Address {
	if a == nil {
		return nil
	}
	return &mail.Address{Address: a.Email, Name: a.DisplayName}
}

// toMailAddresses applies toMailAddress across a slice, skipping nil.
func toMailAddresses(addrs []*Address) []*mail.Address {
	out := make([]*mail.Address, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		out = append(out, toMailAddress(a))
	}
	return out
}
