// Package send owns the policy-resolution primitives shared by the SMS and
// email send paths: route-chain parsing and ordering, {{param}} rendering,
// template parameter validation, and template content decoding.
package send

import (
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strings"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/go-common/jsonx"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"
)

// Route is one entry of a policy route chain. Chains are ordered: the send
// picks a start by weight, then falls back along the list order.
type Route struct {
	AccountID   int64 `json:"account_id"`
	SignatureID int64 `json:"signature_id"`
	Weight      int32 `json:"weight"`
}

// ParseRoutes decodes a policy's route-chain JSON column.
func ParseRoutes(raw models.RawJSON) ([]Route, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var routes []Route
	if err := jsonx.Unmarshal(raw, &routes); err != nil {
		return nil, fmt.Errorf("parse routes: %w", err)
	}
	return routes, nil
}

// MarshalRoutes encodes a route chain for storage.
func MarshalRoutes(routes []Route) (models.RawJSON, error) {
	if routes == nil {
		return models.RawJSON("[]"), nil
	}
	b, err := jsonx.Marshal(routes)
	if err != nil {
		return nil, fmt.Errorf("marshal routes: %w", err)
	}
	return b, nil
}

// OrderRoutes picks the start entry by weight (higher weight = higher
// pick probability) and returns the chain as [start, rest...in list
// order]. Weights <= 0 count as 1. A nil/empty input returns nil.
func OrderRoutes(routes []Route) []Route {
	if len(routes) == 0 {
		return nil
	}
	effective := make([]Route, len(routes))
	copy(effective, routes)
	for i := range effective {
		if effective[i].Weight <= 0 {
			effective[i].Weight = 1
		}
	}

	total := int64(0)
	for _, r := range effective {
		total += int64(r.Weight)
	}
	pick := rand.Int63n(total)
	startIdx := 0
	for i, r := range effective {
		pick -= int64(r.Weight)
		if pick < 0 {
			startIdx = i
			break
		}
	}

	ordered := make([]Route, 0, len(effective))
	ordered = append(ordered, effective[startIdx])
	for i := range effective {
		if i != startIdx {
			ordered = append(ordered, effective[i])
		}
	}
	return ordered
}

// --- template params ---

// paramPattern matches {{name}} placeholders in template content.
var paramPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9_]*)\s*\}\}`)

// Render substitutes {{name}} placeholders with params. Unknown params
// render as the empty string (missing required params are rejected earlier
// by ValidateParams).
func Render(tpl string, params map[string]string) string {
	if tpl == "" || len(params) == 0 {
		return tpl
	}
	return paramPattern.ReplaceAllStringFunc(tpl, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-2])
		return params[name]
	})
}

// ParseParamSpecs decodes a template's params column.
func ParseParamSpecs(raw models.RawJSON) ([]models.TemplateParamSpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var specs []models.TemplateParamSpec
	if err := jsonx.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("parse template params: %w", err)
	}
	return specs, nil
}

// MarshalParamSpecs encodes param specs for storage.
func MarshalParamSpecs(specs []models.TemplateParamSpec) (models.RawJSON, error) {
	if specs == nil {
		return models.RawJSON("[]"), nil
	}
	b, err := jsonx.Marshal(specs)
	if err != nil {
		return nil, fmt.Errorf("marshal template params: %w", err)
	}
	return b, nil
}

// ValidateParams checks that every required declared param is present.
// Extra params are allowed (ignored at render time).
func ValidateParams(specs []models.TemplateParamSpec, params map[string]string) error {
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		if _, ok := params[spec.Name]; !ok {
			return xcodes.ErrTemplateParamMissing.New(fmt.Sprintf("required template param %q missing", spec.Name))
		}
	}
	return nil
}

// --- template content ---

// EmailContent is the (EMAIL, EMAIL_RENDER) template body.
type EmailContent struct {
	Subject  string `json:"subject"`
	TextBody string `json:"text_body"`
	HTMLBody string `json:"html_body"`
}

// VendorCode is one entry of the (SMS, SMS_VENDOR_CODES) mapping.
type VendorCode struct {
	Vendor      pb.SmsVendor `json:"vendor"`
	TemplateCode string      `json:"template_code"`
}

// SmsContent is the (SMS, SMS_CONTENT) template body.
type SmsContent struct {
	Content string `json:"content"`
}

// contentEnvelope mirrors the proto oneof JSON shape used in the Content
// column: exactly one arm set, matching (channel, kind).
type contentEnvelope struct {
	Email       *EmailContent  `json:"email,omitempty"`
	VendorCodes []VendorCode   `json:"vendor_codes,omitempty"`
	SmsContent  *SmsContent    `json:"sms_content,omitempty"`
}

// MarshalContent encodes a template content document for storage.
func MarshalContent(env *contentEnvelope) (models.RawJSON, error) {
	b, err := jsonx.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal template content: %w", err)
	}
	return b, nil
}

// MarshalEmailContent / MarshalVendorCodes / MarshalSmsContent build the
// stored content document for each template kind. Exposed for the admin
// service; kind pairing is validated there before calling.
func MarshalEmailContent(c *EmailContent) (models.RawJSON, error) {
	return MarshalContent(&contentEnvelope{Email: c})
}

func MarshalVendorCodes(codes []VendorCode) (models.RawJSON, error) {
	return MarshalContent(&contentEnvelope{VendorCodes: codes})
}

func MarshalSmsContent(c *SmsContent) (models.RawJSON, error) {
	return MarshalContent(&contentEnvelope{SmsContent: c})
}

// DecodeEmailContent parses an (EMAIL, EMAIL_RENDER) content column.
func DecodeEmailContent(raw models.RawJSON) (*EmailContent, error) {
	var env contentEnvelope
	if err := jsonx.Unmarshal(raw, &env); err != nil {
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if env.Email == nil {
		return nil, xcodes.ErrInvalidTemplateContent.New("content is not an email template")
	}
	return env.Email, nil
}

// DecodeVendorCodes parses an (SMS, SMS_VENDOR_CODES) content column into a
// vendor → template-code lookup.
func DecodeVendorCodes(raw models.RawJSON) (map[pb.SmsVendor]string, error) {
	var env contentEnvelope
	if err := jsonx.Unmarshal(raw, &env); err != nil {
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if env.VendorCodes == nil {
		return nil, xcodes.ErrInvalidTemplateContent.New("content is not a vendor-codes template")
	}
	codes := make(map[pb.SmsVendor]string, len(env.VendorCodes))
	for _, vc := range env.VendorCodes {
		codes[vc.Vendor] = vc.TemplateCode
	}
	return codes, nil
}

// DecodeSmsContent parses an (SMS, SMS_CONTENT) content column.
func DecodeSmsContent(raw models.RawJSON) (*SmsContent, error) {
	var env contentEnvelope
	if err := jsonx.Unmarshal(raw, &env); err != nil {
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if env.SmsContent == nil {
		return nil, xcodes.ErrInvalidTemplateContent.New("content is not an sms-content template")
	}
	return env.SmsContent, nil
}

// ValidateContentShape checks the content document matches (channel, kind)
// and carries usable data. Used by the admin service on create/update.
func ValidateContentShape(channel pb.TemplateChannel, kind pb.TemplateKind, raw models.RawJSON) error {
	switch {
	case channel == pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL && kind == pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER:
		c, err := DecodeEmailContent(raw)
		if err != nil {
			return err
		}
		if c.Subject == "" || c.TextBody == "" {
			return xcodes.ErrInvalidTemplateContent.New("email template requires subject and text_body")
		}
		return nil
	case channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS && kind == pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES:
		codes, err := DecodeVendorCodes(raw)
		if err != nil {
			return err
		}
		if len(codes) == 0 {
			return xcodes.ErrInvalidTemplateContent.New("vendor-codes template requires at least one code")
		}
		return nil
	case channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS && kind == pb.TemplateKind_TEMPLATE_KIND_SMS_CONTENT:
		c, err := DecodeSmsContent(raw)
		if err != nil {
			return err
		}
		if c.Content == "" {
			return xcodes.ErrInvalidTemplateContent.New("sms-content template requires content")
		}
		return nil
	default:
		return xcodes.ErrInvalidTemplateContent.New(fmt.Sprintf(
			"unsupported template channel/kind pairing: %s/%s", channel, kind))
	}
}

// SortVendorCodes returns the vendor codes in a stable order (for admin
// list rendering).
func SortVendorCodes(codes map[pb.SmsVendor]string) []VendorCode {
	out := make([]VendorCode, 0, len(codes))
	for v, c := range codes {
		out = append(out, VendorCode{Vendor: v, TemplateCode: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
}
