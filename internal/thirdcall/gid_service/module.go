package gid_service

import (
	"context"

	pb "github.com/servekit/gid-service/gen/gid/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
)

type moduleGID struct {
	*gidservice.Handler
}

// NewModule creates a GIDService backed by an in-process gid-service instance.
// cfg is forwarded to gidservice.NewModuleFromConfig verbatim — boundary does
// no field unpacking.
func NewModule(cfg *gidconfig.Config) (*moduleGID, error) {
	svc, err := gidservice.NewModuleFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &moduleGID{Handler: svc}, nil
}

func (m *moduleGID) NextID(ctx context.Context) (int64, error) {
	resp, err := m.Handler.NextID(ctx, &pb.NextIDRequest{})
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}
