// Package email contains the email domain business logic: SendEmail +
// email record query/persist + attachment processing. SMS lives in a
// sibling subpackage; the service root holds one of each.
package email

import (
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/message-service/internal/idempotency"
	provemail "github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/pkg/config"

	"gorm.io/gorm"
)

// Service is the email domain service. Resources and per-domain configs are
// injected at construction; the subpackage does not manage their lifecycle.
type Service struct {
	db            *gorm.DB
	idem          idempotency.Checker
	gid           gidservice.Service
	emailRegistry *provemail.AccountRegistry

	// Per-domain configs. caller (service.New) resolves yaml + option
	// overrides before injection.
	persistence bool
	attachment  *config.AttachmentConfig
}

// New constructs an email domain service. idem must be non-nil — service-level
// idempotency is mandatory. attachment must be non-nil — caller (service.New)
// guarantees this since configx allocates nil pointers at Load time.
func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid gidservice.Service,
	emailRegistry *provemail.AccountRegistry,
	persistence bool,
	attachment *config.AttachmentConfig,
) *Service {
	return &Service{
		db:            db,
		idem:          idem,
		gid:           gid,
		emailRegistry: emailRegistry,
		persistence:   persistence,
		attachment:    attachment,
	}
}
