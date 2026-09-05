// Package config defines message-service configuration shape and loads it
// via go-common's configx (viper-based, with env-var override).
//
// SMS vendor maps are string-keyed (YAML-native, third-party friendly) and
// converted to enum-keyed form at service.New time. Email accounts carry
// the vendor as a string field on each account, parsed by the email package
// directly — no cook layer.
package config

import (
	"fmt"
	"time"

	"github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/provider/sms"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/redisx"

	gidconfig "github.com/servekit/gid-service/pkg/config"
)

// serviceName identifies this binary in config file lookup (/etc/<name>) and
// the <NAME>_CONFIG env var. envPrefix scopes all env overrides under
// MESSAGE_SERVICE_*.
const (
	serviceName = "message-service"
	envPrefix   = "MESSAGE_SERVICE"
)

// Config is the top-level message-service configuration loaded from YAML.
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Redis      *redisx.Config
	Log        *logging.Config
	Email      *EmailConfig
	SMS        *SMSConfig
	Cron       *cronx.Config
	ThirdParty *ThirdPartyConfig
	// IdempotencyKeyPrefix is the Redis namespace for idempotency keys,
	// shared across both channels. Per-channel TTLs live on EmailConfig /
	// SMSConfig. Defaults to "msg:idem" — change when sharing Redis across
	// services.
	IdempotencyKeyPrefix string `default:"msg:idem" mapstructure:"idempotency_key_prefix"`
}

// EmailConfig is the message-service-level email config. It value-embeds the
// provider-level email.Config (SMTP account definitions) with ,squash so the
// `email.accounts` yaml path decodes directly into email.Config.Accounts, and
// adds the business-level Persistence / IdempotencyTTL / Attachment sub-configs.
//
// The embed must be a VALUE, not a pointer: mapstructure refuses ,squash on a
// pointer field ("unsupported type for squash: ptr"), and without squash an
// anonymous *email.Config would not match the `email.accounts` key — accounts
// would silently never decode and SendEmail would have no accounts at runtime.
//
// yaml shape:
//
//	email:
//	  persistence: true           # → EmailConfig.Persistence
//	  idempotency_ttl: 5m         # → EmailConfig.IdempotencyTTL
//	  accounts:                   # → embedded email.Config.Accounts
//	    - name: ...
//	  attachment:                 # → EmailConfig.Attachment
//	    fetch_timeout: 30s
//	    max_bytes: 10485760
//	    max_inline_bytes: 2097152
//	    max_total_inline_bytes: 5242880
type EmailConfig struct {
	email.Config `mapstructure:",squash"`
	// Persistence controls whether send records are written to the DB.
	// Defaults to true; set false to skip DB writes (sends only).
	Persistence bool `default:"true"`
	// IdempotencyTTL is the Redis idempotency window for the email channel,
	// in time.ParseDuration string form. Defaults to 5m.
	IdempotencyTTL string            `default:"5m" mapstructure:"idempotency_ttl"`
	Attachment     *AttachmentConfig `mapstructure:"attachment"`
}

// SMSConfig is the YAML-friendly form of SMS vendor accounts + routes.
type SMSConfig struct {
	DefaultCountry string `default:"CN"` // ISO 3166-1 alpha-2, default "CN"
	// Persistence controls whether send records are written to the DB.
	// Defaults to true; set false to skip DB writes (sends only).
	Persistence bool `default:"true"`
	// IdempotencyTTL is the Redis idempotency window for the SMS channel,
	// in time.ParseDuration string form. Defaults to 5m.
	IdempotencyTTL string `default:"5m" mapstructure:"idempotency_ttl"`
	// Vendors maps a vendor name to its accounts. Valid keys (parsed to
	// pb.SmsVendor by service.New): "aliyun", "tencent", "volcengine",
	// "byteplus", "huawei".
	Vendors map[string]*sms.VendorConfig
	Routes  []*RouteConfig
}

// RouteConfig is one country's route targets in YAML.
type RouteConfig struct {
	Country string // ISO 3166-1 alpha-2, or "*" for fallback
	Targets []*RouteTarget
}

// RouteTarget references a (vendor, account) pair by string names.
type RouteTarget struct {
	// Vendor is the SMS vendor name; must match a key in SMSConfig.Vendors
	// (currently "aliyun").
	Vendor string
	// Account is the account name under the vendor; must match an
	// AccountConfig.Name in that vendor's config.
	Account string
}

// ServerConfig holds gRPC and HTTP server addresses plus transport limits.
type ServerConfig struct {
	GRPCAddr string `default:":19092"`
	HTTPAddr string `default:":18082"`
	// MaxRecvMsgSizeBytes is the gRPC server MaxRecvMsgSize. Raised from
	// gRPC's 4MB default to accommodate inline attachment.content payloads.
	MaxRecvMsgSizeBytes int `default:"10485760" mapstructure:"max_recv_msg_size_bytes"`
}

// ThirdPartyConfig groups configuration for external service integrations.
type ThirdPartyConfig struct {
	GID *RemoteServiceConfig[*gidconfig.Config]
}

// RemoteServiceConfig is the shared third_party.<name> section shape,
// aliased from go-common so Mode is the configx.Mode enum.
type RemoteServiceConfig[T any] = configx.RemoteServiceConfig[T]

// DefaultIdempotencyTTL is the fallback per-channel TTL when the yaml field
// is empty or unparseable, or when a module-mode caller constructs an
// EmailConfig / SMSConfig without going through Load. Exported so non-config
// callers can share the same default.
const DefaultIdempotencyTTL = 5 * time.Minute

// AttachmentConfig controls inline-content attachment limits. url-sourced
// attachments are pure references — never fetched — so they carry no fetch
// timeout or size cap here. Defaults come from `default:` tags — configx
// wires them via viper.SetDefault at Load time, so callers can read fields
// directly without XxxOr() helpers.
type AttachmentConfig struct {
	// MaxInlineBytes caps a single attachment.content payload.
	MaxInlineBytes int64 `default:"2097152" mapstructure:"max_inline_bytes"`
	// MaxTotalInlineBytes caps the sum of all attachment.content payloads in
	// one SendEmailRequest. Leaves headroom under ServerConfig.MaxRecvMsgSizeBytes.
	MaxTotalInlineBytes int64 `default:"5242880" mapstructure:"max_total_inline_bytes"`
}

// DefaultIdempotencyKeyPrefix is the Redis namespace used when
// IdempotencyKeyPrefix is unset. "msg" aligns with the wider message-service
// Redis namespace; "idem" distinguishes idempotency keys from future msg:*
// uses (rate limit, cache). Exported so module-mode callers that skip Load
// can reference the same default.
const DefaultIdempotencyKeyPrefix = "msg:idem"

// IdempotencyTTLDuration parses cfg.IdempotencyTTL into a time.Duration,
// falling back to DefaultIdempotencyTTL on empty / unparseable input.
// Defensive only — configx fills the default at Load time, so this is for
// module-mode callers that construct EmailConfig without going through Load.
func (e *EmailConfig) IdempotencyTTLDuration() time.Duration {
	if e == nil || e.IdempotencyTTL == "" {
		return DefaultIdempotencyTTL
	}
	if d, err := time.ParseDuration(e.IdempotencyTTL); err == nil {
		return d
	}
	return DefaultIdempotencyTTL
}

// IdempotencyTTLDuration mirrors EmailConfig.IdempotencyTTLDuration.
func (s *SMSConfig) IdempotencyTTLDuration() time.Duration {
	if s == nil || s.IdempotencyTTL == "" {
		return DefaultIdempotencyTTL
	}
	if d, err := time.ParseDuration(s.IdempotencyTTL); err == nil {
		return d
	}
	return DefaultIdempotencyTTL
}

// Validate checks required configuration fields and cross-field invariants.
func (c *Config) Validate() error {
	if c.ThirdParty == nil || c.ThirdParty.GID == nil {
		return fmt.Errorf("third_party.gid is required")
	}
	gid := c.ThirdParty.GID
	if gid.Mode == "" {
		return fmt.Errorf("third_party.gid.mode is required (module or grpc)")
	}
	switch gid.Mode {
	case "module":
		if err := gid.Config.ValidateSnowflake(); err != nil {
			return fmt.Errorf("third_party.gid.config.snowflake: %w", err)
		}
	case "grpc":
		if gid.Target == "" {
			return fmt.Errorf("third_party.gid.target is required for grpc mode")
		}
	default:
		return fmt.Errorf("third_party.gid.mode must be module or grpc, got %q", gid.Mode)
	}
	return nil
}

// Load reads the message-service config from YAML (and env overrides),
// applies defaults, and runs Validate. Returns a string-keyed Config;
// conversion to the internal enum-keyed form happens at service.New.
//
// WithExpandEnv enables ${VAR} expansion in YAML string values — used by
// the Docker deployment (config.docker.yaml) where every field is a
// ${MESSAGE_SERVICE_*} reference.
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName(serviceName),
		configx.WithEnvPrefix(envPrefix),
		configx.WithExpandEnv(),
	); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
