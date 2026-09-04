// Package email contains the email domain business logic: SendEmail +
// email record query/persist + attachment processing. SMS lives in a
// sibling subpackage; the service root holds one of each.
package email

import (
	"net/http"

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
	httpClient    *http.Client

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
	httpClient *http.Client,
) *Service {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: attachment.FetchTimeoutDuration(),
			// Do not follow redirects. Attachment URLs are expected to be
			// direct (e.g. OSS pre-signed). Following redirects would allow a
			// malicious or compromised endpoint to aim the fetch at internal
			// addresses (SSRF — e.g. cloud metadata service).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Service{
		db:            db,
		idem:          idem,
		gid:           gid,
		emailRegistry: emailRegistry,
		httpClient:    httpClient,
		persistence:   persistence,
		attachment:    attachment,
	}
}
