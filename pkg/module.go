// Package messageservice provides in-process and gRPC client access to
// message-service.
package messageservice

import (
	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/service"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/handler"
	"github.com/servekit/message-service/pkg/option"
)

// Handler is the in-process entry point. Aliased to *handler.Handler so
// external code references it as messageservice.Handler.
type Handler = handler.Handler

// Compile-time assertion: *Handler satisfies the gRPC server interface.
var _ pb.MessageServiceServer = (*Handler)(nil)

// NewModule constructs an in-process message-service for embedding.
//
// Returns only the Handler — Handler IS the public capability and ALSO
// satisfies signalx.Service (Start/Stop), so module users manage lifecycle
// via the same object they call RPC methods on:
//
//	hdl, err := messageservice.NewModule(cfg, option.WithDB(parentDB))
//	if err != nil { panic(err) }
//	defer hdl.Stop()
//	resp, err := hdl.GetEmail(ctx, &pb.GetEmailRequest{Id: 1})
//
// Resources injected via option.WithDB / WithGIDService are NOT owned by
// the service — parent process keeps ownership.
func NewModule(cfg *config.Config, opts ...option.Option) (*Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return handler.New(svc), nil
}
