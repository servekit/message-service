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

	pb "github.com/servekit/message-service/gen/message/v1"
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
	if len(msg.To) == 0 {
		return nil, fmt.Errorf("email: recipient is empty")
	}

	if len(s.providers) == 0 {
		return nil, fmt.Errorf("email: no provider available")
	}

	// Target captures the primary recipient (first To entry) for audit
	// logging. Multi-recipient sends keep Target = msg.To[0].Email; the
	// full list travels with Message and is not repeated here.
	target := ""
	if len(msg.To) > 0 && msg.To[0] != nil {
		target = msg.To[0].Email
	}

	start := time.Now()
	var lastErr error
	var lastVendor pb.EmailVendor
	var lastAccount string
	attempts := 0

	for _, p := range s.providers {
		if ctx.Err() != nil {
			return &SendResult{
				Message: msg, Vendor: lastVendor, Account: lastAccount, Target: target,
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
			Message: msg, Vendor: p.Vendor(), Account: p.Account(), Target: target,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}

	return &SendResult{
		Message: msg, Vendor: lastVendor, Account: lastAccount, Target: target,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("email: all providers failed, last error: %w", lastErr)
}
