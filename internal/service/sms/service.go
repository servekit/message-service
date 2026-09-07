// Package sms contains the SMS domain business logic: policy-driven
// SendSMS + SMS record query/persist + CN/intl route-chain dispatch.
// Email lives in a sibling subpackage; the service root holds one of each.
package sms

import (
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/quota"
	"github.com/servekit/message-service/internal/registry"

	"gorm.io/gorm"
)

// Service is the SMS domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db   *gorm.DB
	idem idempotency.Checker
	gid  gidservice.Service
	reg  *registry.Registry
	// quota gates sends against the app's SMS daily limit.
	quota *quota.Checker

	// Per-domain config. caller (service.New) resolves yaml + option
	// overrides before injection.
	persistence bool
}

// New constructs an SMS domain service. idem must be non-nil — service-level
// idempotency is mandatory.
func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid gidservice.Service,
	reg *registry.Registry,
	quotaChecker *quota.Checker,
	persistence bool,
) *Service {
	return &Service{
		db:          db,
		idem:        idem,
		gid:         gid,
		reg:         reg,
		quota:       quotaChecker,
		persistence: persistence,
	}
}
