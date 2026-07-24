package email

// Address is one mailbox. Provider-internal type so this package does not
// depend on proto. The service layer converts pb.EmailAddress → Address at
// the boundary; SMTPProvider formats it as RFC 5322 string for go-mail.
type Address struct {
	Email       string
	DisplayName string
}

// Message represents an email to be sent. Address fields are structured —
// SMTPProvider does the RFC 5322 formatting (e.g. "Name <email>" or bare).
// This keeps the way open for future API vendors that take email/name as
// separate fields rather than parsing a formatted string.
type Message struct {
	To       []*Address
	Cc       []*Address
	Bcc      []*Address
	Subject  string
	Body     string
	HTMLBody string
	ReplyTo  *Address

	// From optionally overrides the account's configured From. nil = use
	// provider default. SMTP server is the ultimate arbiter of accepted
	// From addresses (verified sender lists, etc.); the service does not
	// pre-constrain.
	From *Address

	// Template is an optional vendor-side template identifier. Vendors
	// that do not support templating (e.g. SMTP) ignore this field.
	Template string
	// TemplateParams supplies variable substitutions when Template is set.
	// Vendors that do not support templating ignore this field.
	TemplateParams map[string]string

	// Attachments are MIME-mode attachments. The provider embeds each as a
	// MIME part: regular attachment when Inline=false, CID-embedded when
	// Inline=true. Empty slice = no attachments. LINK-mode attachments
	// never reach here — they are rendered into HTMLBody by the service
	// layer before calling Send.
	Attachments []*Attachment
}

// Attachment is a MIME-mode attachment. Provider-internal — service layer
// downloads bytes from URL and constructs this. The provider never sees URLs
// or kinds.
type Attachment struct {
	Filename  string // MIME filename; required
	Content   []byte // raw bytes; required
	MimeType  string // optional; empty = let go-mail infer from filename
	Inline    bool   // true = CID-embedded (<img src="cid:...">), false = regular attachment
	ContentID string // required when Inline=true; the CID value used in HTML
}
