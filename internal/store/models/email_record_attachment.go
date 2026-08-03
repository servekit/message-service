package models

import "time"

// MessageEmailRecordAttachment stores metadata for one attachment on a sent
// email. Bytes are NOT persisted — inline-content attachments leave URL empty,
// URL-sourced attachments record the fetched URL.
//
// The "Message" prefix lets GORM's NamingStrategy auto-derive the table name
// "message_email_record_attachments" — same convention as MessageEmailRecord.
//
// No foreign key to MessageEmailRecord (per CLAUDE.md — relation integrity is
// enforced at the application layer). Cascade deletes are NOT used; the
// service layer deletes attachment rows in the same transaction as the
// parent record when needed.
type MessageEmailRecordAttachment struct {
	ID            int64  `gorm:"primaryKey"`
	EmailRecordID int64  `gorm:"column:email_record_id;not null;index"`
	Filename      string `gorm:"size:255;not null"`
	// URL is empty for inline-content attachments (caller supplied bytes via
	// attachment.content); populated for URL-sourced attachments.
	URL       string    `gorm:"size:2048;column:url"`
	Inline    bool      `gorm:"not null;default:false"`
	MimeType  string    `gorm:"size:127;column:mime_type"`
	SizeBytes int64     `gorm:"column:size_bytes"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime"`
}
