package models

import (
	"database/sql"
	"time"

	"gorm.io/gorm"
)

// MessageSMSRecord stores a complete record of every SMS sent through the service.
// See MessageEmailRecord for the rationale on the "Message" prefix.
type MessageSMSRecord struct {
	ID         int64  `gorm:"primaryKey"`
	Vendor     int32  `gorm:"not null;default:0;index"`
	Account    string `gorm:"size:64;column:account"`
	Scene      int32  `gorm:"not null;default:0;index"`
	Status     int32  `gorm:"not null;default:0;index"`
	RegionCode string `gorm:"size:2;column:region_code;not null;index"`
	Phone      string `gorm:"size:64;column:phone;not null;index"`
	// AppKey identifies the calling app (the authenticated sender identity
	// resolved from x-app-key credentials at send time).
	AppKey string `gorm:"size:64;column:app_key;index"`
	// SignName is the SMS signature / intl sender ID resolved from the
	// policy route that handled the send.
	SignName       string          `gorm:"size:64;column:sign_name"`
	Content        string          `gorm:"type:text"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:json;column:template_params"`
	ErrorMessage   string          `gorm:"size:1024;column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;autoCreateTime;index"`
	UpdatedAt      time.Time       `gorm:"not null;autoUpdateTime"`
	DeletedAt      gorm.DeletedAt  `gorm:"index"`
}

// SmsStatsRow is the scan target for raw SQL aggregations over the
// message_sms_records table. Not a GORM model — only used by
// dal.CountSMSStats / dal.ListSMSVendorStats.
type SmsStatsRow struct {
	Vendor int32
	Total  int64
	Sent   int64
	Failed int64
}

// SmsTotalStatsRow is the scan target for the total stats query.
// Not a GORM model.
type SmsTotalStatsRow struct {
	Total  int64
	Sent   int64
	Failed int64
}
