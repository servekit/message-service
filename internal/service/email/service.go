// Package email contains the email domain business logic: policy-driven
// SendEmail + email record query/persist + attachment processing. SMS lives
// in a sibling subpackage; the service root holds one of each.
package email

import (
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/quota"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/pkg/config"

	"gorm.io/gorm"
)

// Service is the email domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db   *gorm.DB
	idem idempotency.Checker
	gid  gidservice.Service
	reg  *registry.Registry
	// quota gates sends against the app's email daily limit.
	quota *quota.Checker

	// Per-domain config. caller (service.New) resolves yaml + option
	// overrides before injection.
	persistence bool
	attachment  *config.AttachmentConfig
}

// New constructs an email domain service. idem must be non-nil —
// service-level idempotency is mandatory. attachment must be non-nil —
// caller (service.New) guarantees this since configx allocates nil
// pointers at Load time.
func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid gidservice.Service,
	reg *registry.Registry,
	quotaChecker *quota.Checker,
	persistence bool,
	attachment *config.AttachmentConfig,
) *Service {
	return &Service{
		db:          db,
		idem:        idem,
		gid:         gid,
		reg:         reg,
		quota:       quotaChecker,
		persistence: persistence,
		attachment:  attachment,
	}
}
