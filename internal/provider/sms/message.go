package sms

// Message is the input for a domestic SMS send. Domestic vendors (Aliyun,
// Tencent, Volcengine, Huawei) require a pre-registered template and reject
// raw content for regulatory reasons. SignName is the per-message signature
// (e.g. "阿里云") — domestic vendors require it on every send; the caller
// owns it because signatures are scene-specific (a verification-code signature
// differs from a marketing signature).
type Message struct {
	To             string
	SignName       string
	TemplateID     string
	TemplateParams map[string]string
}

// InternationalMessage is the input for an international SMS send. The shape
// is flexible because international vendors split into two camps:
//
//   - Raw-content vendors (Aliyun SendMessageToGlobe, Twilio, AWS SNS):
//     the body is forwarded verbatim — set Content.
//   - Template-based vendors (Byteplus, Tencent with intl SdkAppid):
//     they require a vendor pre-registered template — set TemplateID and
//     TemplateParams.
//
// Caller sets EITHER Content OR TemplateID+TemplateParams, not both. The
// vendor adapter picks whichever its SDK supports; setting both is a caller
// bug and the vendor may pick either at its discretion.
//
// SignName here is the sender ID / "From" field — its meaning is vendor- and
// region-specific (some regions require pre-registered alphabetic sender
// IDs, others allow arbitrary numbers).
type InternationalMessage struct {
	To             string
	SignName       string
	Content        string
	TemplateID     string
	TemplateParams map[string]string
}
