package messageservice

import (
	"errors"

	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/signalx"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/message-service/internal/service"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/handler"
	"github.com/servekit/message-service/pkg/option"
)

// Compile-time assertion: *Server satisfies signalx.Service.
var _ signalx.Service = (*Server)(nil)

// Server wraps a gRPC + HTTP gateway server for message-service.
//
// Holds grpcSrv (gRPC transport) and hdl (the Handler, which itself wraps
// the underlying *service.Service and exposes Start/Stop). There's no
// separate svc field — Handler is the single handle for both RPC dispatch
// and lifecycle.
type Server struct {
	grpcSrv *grpcx.Server
	hdl     *handler.Handler
}

// ServerOption configures a Server instance.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []option.Option
}

// WithServiceOptions forwards options to the service layer.
func WithServiceOptions(opts ...option.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// NewServer constructs a Server with all dependencies wired.
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var so serverOptions
	for _, opt := range opts {
		opt(&so)
	}

	svc, err := service.New(cfg, so.serviceOpts...)
	if err != nil {
		return nil, err
	}

	hdl := handler.New(svc)

	validator, err := protovalidate.New()
	if err != nil {
		return nil, err
	}

	grpcSrv := grpcx.New(
		&grpcx.ServerConfig{
			GRPCAddr: cfg.Server.GRPCAddr,
			// Raise gRPC MaxRecvMsgSize to accommodate inline attachment
			// payloads (attachment.content). The hard cap on inline bytes
			// is enforced at the service layer (EmailConfig.Attachment).
			ServerOptions: []grpc.ServerOption{
				grpc.MaxRecvMsgSize(cfg.Server.MaxRecvMsgSizeBytes),
			},
		},
		func(s *grpc.Server) { pb.RegisterMessageServiceServer(s, hdl) },
		nil, // no HTTP gateway — gRPC-only service
		grpcx.LoggingInterceptor,
		grpcx.ErrorInterceptor,
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)

	return &Server{grpcSrv: grpcSrv, hdl: hdl}, nil
}

// Start starts service internals and the gRPC server without blocking.
//
// On partial failure, started components are rolled back via Stop.
func (s *Server) Start() error {
	if err := s.hdl.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.hdl.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC server and service internals.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.hdl.Stop())
}
