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

	pb "github.com/servekit/message-service/gen/message/v1"
)

// AccountProvider is the interface vendor implementations satisfy. It carries
// both the proto-enum Vendor identity and the per-account name, so Sender,
// Router, and Registry can return enough context for record persistence
// without an external wrapper layer.
//
// Each provider exposes two strictly separated send paths:
//   - Send: domestic SMS (template-required, sign-name-based)
//   - SendInternational: international SMS (raw-content-based, sender-ID-based)
//
// Vendors that do not support one path must return a clear "not supported"
// error from that method — the Router/Sender treats this like any other
// per-vendor failure and falls back to the next target in the chain.
type AccountProvider interface {
	Vendor() pb.SmsVendor
	Account() string
	Send(ctx context.Context, msg *Message) error
	SendInternational(ctx context.Context, msg *InternationalMessage) error
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

// SendInternational mirrors Send for the international (raw-content) path.
// It tries providers in order, falling back on failure. Vendors that do not
// support international SMS contribute an error to the failure chain — they
// do not short-circuit the fallback.
func (s *Sender) SendInternational(ctx context.Context, msg *InternationalMessage) (*SendResult, error) {
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
				Message: nil, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
				Success: false, Error: ctx.Err(),
				Duration: time.Since(start), Attempts: attempts,
			}, ctx.Err()
		}

		attempts++
		lastVendor = p.Vendor()
		lastAccount = p.Account()
		if err := p.SendInternational(ctx, msg); err != nil {
			lastErr = err
			continue
		}

		return &SendResult{
			Message: nil, Vendor: p.Vendor(), Account: p.Account(), Target: msg.To,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}

	return &SendResult{
		Message: nil, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("sms: all providers failed, last error: %w", lastErr)
}
