package sms

import (
	"context"
	"fmt"
	"time"

	"github.com/nyaruka/phonenumbers"

	pb "github.com/servekit/api/gen/go/messaging/v1"
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

// SendInternational mirrors Send for the international (raw-content) path.
// Targets are resolved the same way (by phone-derived country); each target's
// SendInternational is called. Vendors that do not support international SMS
// contribute an error to the failure chain — they do not short-circuit the
// fallback.
func (r *Router) SendInternational(ctx context.Context, msg *InternationalMessage) (*SendResult, error) {
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
				Message: nil, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
				Success: false, Error: ctx.Err(),
				Duration: time.Since(start), Attempts: attempts,
			}, ctx.Err()
		}
		attempts++
		lastVendor = ap.Vendor()
		lastAccount = ap.Account()
		if err := ap.SendInternational(ctx, msg); err != nil {
			lastErr = err
			continue
		}
		return &SendResult{
			Message: nil, Vendor: ap.Vendor(), Account: ap.Account(), Target: msg.To,
			Success:  true,
			Duration: time.Since(start), Attempts: attempts,
		}, nil
	}
	return &SendResult{
		Message: nil, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
		Success: false, Error: lastErr,
		Duration: time.Since(start), Attempts: attempts,
	}, fmt.Errorf("sms: all international targets failed for %s, last error: %w", msg.To, lastErr)
}
