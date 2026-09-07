// Seed: one-shot importer of legacy YAML vendor accounts into the platform
// channel-account pool. Run via `message-service migrate --seed-from-config`
// AFTER the tables exist. Idempotent per account name — existing rows are
// skipped. After seeding, remove the account blocks from YAML: runtime no
// longer reads them.
package handler

import (
	"fmt"
	"log/slog"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/go-common/jsonx"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/xcodes"

	"gorm.io/gorm"
)

// SeedFromConfig imports legacy config-file accounts into
// message_channel_accounts. Names are derived as "<vendor>-<account-name>"
// (lowercased) so they read naturally in the admin UI. Existing names are
// skipped, so re-running is safe.
func SeedFromConfig(db *gorm.DB, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	imported := 0

	// SMS accounts (string-keyed vendors in YAML → SmsVendor enum).
	smsVendorNames := map[string]pb.SmsVendor{
		"aliyun":     pb.SmsVendor_SMS_VENDOR_ALIYUN,
		"tencent":    pb.SmsVendor_SMS_VENDOR_TENCENT,
		"volcengine": pb.SmsVendor_SMS_VENDOR_VOLCENGINE,
		"byteplus":   pb.SmsVendor_SMS_VENDOR_BYTEPLUS,
		"huawei":     pb.SmsVendor_SMS_VENDOR_HUAWEI,
	}
	if cfg.SMS != nil {
		for vendorName, vc := range cfg.SMS.Vendors {
			vendor, ok := smsVendorNames[vendorName]
			if !ok {
				slog.Warn("seed: unknown sms vendor in config, skipped", "vendor", vendorName)
				continue
			}
			if vc == nil {
				continue
			}
			for _, ac := range vc.Accounts {
				if ac == nil {
					continue
				}
				name := fmt.Sprintf("%s-%s", vendorName, ac.Name)
				stored, err := jsonx.Marshal(ac)
				if err != nil {
					return xcodes.ErrInternal.Wrapf(err, "seed: marshal account %s", name)
				}
				if err := seedAccount(db, name, int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), int32(vendor), stored); err != nil {
					return err
				}
				imported++
			}
		}
	}

	// Email accounts (SMTP brand label travels inside the config JSON).
	if cfg.Email != nil {
		for _, ac := range cfg.Email.Config.Accounts {
			if ac == nil {
				continue
			}
			brand := ac.Vendor
			stored, err := jsonx.Marshal(ac)
			if err != nil {
				return xcodes.ErrInternal.Wrapf(err, "seed: marshal email account %s", ac.Name)
			}
			vendor := emailVendorEnum(brand)
			name := fmt.Sprintf("%s-%s", brand, ac.Name)
			if err := seedAccount(db, name, int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL), int32(vendor), stored); err != nil {
				return err
			}
			imported++
		}
	}

	slog.Info("seed: channel accounts imported (existing names skipped)", "imported", imported)
	return nil
}

// seedAccount inserts one account unless the name already exists
// (soft-deleted rows included — a re-seed must not resurrect anything).
func seedAccount(db *gorm.DB, name string, channel, vendor int32, stored []byte) error {
	var exists int64
	if err := db.Unscoped().Model(&models.MessageChannelAccount{}).
		Where("name = ?", name).Count(&exists).Error; err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	if exists > 0 {
		slog.Info("seed: account exists, skipped", "name", name)
		return nil
	}
	record := &models.MessageChannelAccount{
		ID:      0, // auto-increment is fine for the one-shot seed path
		Channel: channel,
		Vendor:  vendor,
		Name:    name,
		Config:  models.RawJSON(stored),
	}
	if err := db.Create(record).Error; err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// emailVendorEnum maps the SMTP brand label to the EmailVendor enum.
func emailVendorEnum(brand string) pb.EmailVendor {
	switch brand {
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	default:
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	}
}
