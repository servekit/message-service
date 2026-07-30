// Package sms contains the SMS domain business logic: SendSMS + SMS record
// query/persist + domestic/international routing. Email lives in a sibling
// subpackage; the service root holds one of each.
package sms

import (
	"github.com/servekit/message-service/internal/idempotency"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	gid_service "github.com/servekit/message-service/internal/thirdcall/gid_service"

	"gorm.io/gorm"
)

// Service is the SMS domain service. Resources and per-domain configs are
// injected at construction; the subpackage does not manage their lifecycle.
type Service struct {
	db          *gorm.DB
	idem        idempotency.Checker
	gid         gid_service.GIDService
	smsRegistry *provesms.AccountRegistry
	smsRouter   *provesms.Router // nil when no routes configured

	// Per-domain config. caller (service.New) resolves yaml + option
	// overrides before injection.
	persistence bool
}

// New constructs an SMS domain service. idem must be non-nil — service-level
// idempotency is mandatory. smsRouter may be nil — when nil, SendSMS without
// explicit vendor+account returns BadRequest ("sms routes not configured").
func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid gid_service.GIDService,
	smsRegistry *provesms.AccountRegistry,
	smsRouter *provesms.Router,
	persistence bool,
) *Service {
	return &Service{
		db:          db,
		idem:        idem,
		gid:         gid,
		smsRegistry: smsRegistry,
		smsRouter:   smsRouter,
		persistence: persistence,
	}
}
