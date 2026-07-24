package models

import (
	"database/sql"
	"time"

	"gorm.io/gorm"
)

// MessageEmailRecord stores a complete record of every email sent through the service.
// The "Message" prefix lets GORM's NamingStrategy auto-derive the table name
// "message_email_records" — no TableName() override needed. The same prefix
// is the single source of truth used by raw SQL in dal and cmd/migrate.
type MessageEmailRecord struct {
	ID      int64  `gorm:"primaryKey"`
	Vendor  int32  `gorm:"not null;default:0;index"`
	Account string `gorm:"size:64;column:account"`
	Scene   int32  `gorm:"not null;default:0;index"`
	Status  int32  `gorm:"not null;default:0;index"`
	Target  string `gorm:"size:255;not null;index"`
	// SenderID identifies the calling business service (e.g. "user-service",
	// "pay-service"). NOT the end-user/admin id — the caller is responsible
	// for recording that in its own audit trail.
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
	Cc             StringSlice     `gorm:"type:jsonb;column:cc"`
	Bcc            StringSlice     `gorm:"type:jsonb;column:bcc"`
	Subject        string          `gorm:"type:text"`
	Content        string          `gorm:"type:text"`
	HTMLBody       string          `gorm:"type:text;column:html_body"`
	ReplyTo        string          `gorm:"size:255;column:reply_to"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`
	ErrorMessage   string          `gorm:"size:1024;column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time       `gorm:"not null;default:now()"`
	DeletedAt      gorm.DeletedAt  `gorm:"index"`
}

// EmailStatsRow is the scan target for raw SQL aggregations over the
// message_email_records table. It is NOT a GORM model — only used as the
// row type for dal.CountEmailStats / dal.ListEmailVendorStats.
type EmailStatsRow struct {
	Vendor int32
	Total  int64
	Sent   int64
	Failed int64
}

// EmailTotalStatsRow is the scan target for the total stats query.
// Not a GORM model.
type EmailTotalStatsRow struct {
	Total  int64
	Sent   int64
	Failed int64
}
