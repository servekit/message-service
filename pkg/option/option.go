// Package option defines functional options for configuring the message service.
package option

import (
	"github.com/servekit/message-service/pkg/thirdcall"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Option configures a Service instance.
type Option func(*Options)

// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	Redis      *redis.Client
	GIDService thirdcall.GIDService

	EmailPersistence *bool // nil = use yaml/default
	SMSPersistence   *bool
}

// WithDB injects an external database connection. The service will not close it.
func WithDB(db *gorm.DB) Option {
	return func(o *Options) { o.DB = db }
}

// WithRedis injects an external Redis client. The service will not close it.
func WithRedis(client *redis.Client) Option {
	return func(o *Options) { o.Redis = client }
}

// WithGIDService provides a gid-service instance.
// If not set, the service creates one from config.ThirdParty.GID.
func WithGIDService(svc thirdcall.GIDService) Option {
	return func(o *Options) { o.GIDService = svc }
}

// WithEmailPersistence overrides the email persistence toggle. When set,
// takes precedence over yaml config. Use to disable persistence from code:
//
//	messageservice.NewModule(cfg, option.WithEmailPersistence(false))
func WithEmailPersistence(enabled bool) Option {
	return func(o *Options) { o.EmailPersistence = &enabled }
}

// WithSMSPersistence mirrors WithEmailPersistence for the SMS channel.
func WithSMSPersistence(enabled bool) Option {
	return func(o *Options) { o.SMSPersistence = &enabled }
}

// Apply evaluates all options and returns the resolved Options.
func Apply(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
