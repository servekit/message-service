package messageservice

import (
	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// Service is how a consumer holds message-service regardless of backend: the
// in-process *Handler (module mode) and the gRPC *Client both satisfy it. It
// embeds the generated server interfaces so the method set tracks the proto
// automatically — no hand-maintained method list here. The admin surface is
// part of the same handle (both are implemented by internal/service.Service).
type Service interface {
	pb.MessageServiceServer
	pb.MessageAdminServiceServer
}
