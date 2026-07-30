// Package option defines functional options for configuring the message service.
package option

import (
	"github.com/redis/go-redis/v9"
	gidservice "github.com/servekit/gid-service/pkg"
	"gorm.io/gorm"
)

// Option configures a Service instance.
type Option func(*Options)

// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	Redis      *redis.Client
	GIDHandler *gidservice.Handler

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

// WithGIDHandler injects a raw gid-service Handler. message-service wraps it
// internally into its GIDService; callers do not need to know that interface.
// Required when third_party.gid.mode=module (a parent process embeds this
// service and owns the Handler); in grpc mode the service dials gid-service
// itself and this option is unused.
func WithGIDHandler(h *gidservice.Handler) Option {
	return func(o *Options) { o.GIDHandler = h }
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
