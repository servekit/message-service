package messageservice

import (
	"context"
	"net"
	"sync"
	"testing"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/go-common/tenantctx"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// I2 (④T1): in grpc mode the only credential that may cross a
// consumer → message-service hop is the trusted tenant identity. The dial
// chain must forward the ctx tenant key as x-tenant-key (the exact mirror of
// ForwardActorUnary for x-actor); without it a consumer's send reaches the
// data plane with no credentials and fails closed with ErrAppUnauthorized
// even though the consumer had already resolved and verified its caller.

// tenantCaptureStub records the incoming metadata of the last SendSMS so a
// test can assert what actually crossed the wire.
type tenantCaptureStub struct {
	pb.UnimplementedMessageServiceServer

	mu sync.Mutex
	md metadata.MD
}

func (s *tenantCaptureStub) SendSMS(ctx context.Context, _ *pb.SendSMSRequest) (*pb.SendResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.md, _ = metadata.FromIncomingContext(ctx)
	return &pb.SendResponse{}, nil
}

// newTenantCapturePair stands up a real in-process gRPC server returning the
// capture stub, plus a *Client dialed at it with NewClient's default chain —
// the same chain grpc-mode consumers get from Connect.
func newTenantCapturePair(t *testing.T) (*Client, *tenantCaptureStub) {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	stub := &tenantCaptureStub{}
	gs := grpc.NewServer()
	pb.RegisterMessageServiceServer(gs, stub)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := NewClient(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, stub
}

// TestClient_ForwardsTenantKey: a ctx carrying a trusted tenant key lands on
// the wire as x-tenant-key with the same value — the data plane's dualauth
// resolver (SourceTrusted) then keys the send to that tenant.
func TestClient_ForwardsTenantKey(t *testing.T) {
	c, stub := newTenantCapturePair(t)

	_, err := c.SendSMS(tenantctx.WithTenantKey(context.Background(), "ten_fwdgrpc01"), &pb.SendSMSRequest{})
	require.NoError(t, err)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.NotNil(t, stub.md, "server saw no metadata at all")
	require.Equal(t, []string{"ten_fwdgrpc01"}, stub.md.Get(tenantctx.HeaderTenantKey),
		"the ctx tenant key must cross the wire as x-tenant-key")
}

// TestClient_AnonymousCtxForwardsNoTenantKey: contexts without a tenant key
// forward nothing — the same anonymous-calls-forward-nothing contract as
// ForwardActorUnary. The data plane fails closed on its own.
func TestClient_AnonymousCtxForwardsNoTenantKey(t *testing.T) {
	c, stub := newTenantCapturePair(t)

	_, err := c.SendSMS(context.Background(), &pb.SendSMSRequest{})
	require.NoError(t, err)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.md != nil {
		require.Empty(t, stub.md.Get(tenantctx.HeaderTenantKey),
			"anonymous contexts must not fabricate a tenant key")
	}
}
