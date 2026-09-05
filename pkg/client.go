package messageservice

import (
	"context"
	"fmt"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	pb "github.com/servekit/api/gen/go/messaging/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client is a gRPC client for message-service shaped like *Handler: it
// implements the generated pb.MessageServiceServer interface (unary methods
// without grpc.CallOption), so a consumer can hold either backend behind that
// one generated interface — module mode passes the *Handler, grpc mode passes
// the *Client — with no per-consumer adapter.
//
// The UnimplementedMessageServiceServer embed satisfies the interface's
// mustEmbed guard; every RPC below shadows it with a real delegation. When a
// new RPC is added to the proto, add its delegation here — until then grpc
// mode returns codes.Unimplemented for it.
type Client struct {
	pb.UnimplementedMessageServiceServer

	conn *grpc.ClientConn
	cli  pb.MessageServiceClient
}

// Compile-time assertion: *Client and *Handler expose the same interface.
var _ pb.MessageServiceServer = (*Client)(nil)

// NewClient creates a Client connected to the given target.
// If no dial options are provided, it uses insecure credentials by default.
func NewClient(target string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	return &Client{conn: conn, cli: pb.NewMessageServiceClient(conn)}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ping delegates to the remote message-service.
func (c *Client) Ping(ctx context.Context, in *emptypb.Empty) (*commonv1.Pong, error) {
	return c.cli.Ping(ctx, in)
}

// SendEmail delegates to the remote message-service.
func (c *Client) SendEmail(ctx context.Context, in *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return c.cli.SendEmail(ctx, in)
}

// SendSMS delegates to the remote message-service.
func (c *Client) SendSMS(ctx context.Context, in *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return c.cli.SendSMS(ctx, in)
}

// GetEmail delegates to the remote message-service.
func (c *Client) GetEmail(ctx context.Context, in *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return c.cli.GetEmail(ctx, in)
}

// ListEmails delegates to the remote message-service.
func (c *Client) ListEmails(ctx context.Context, in *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return c.cli.ListEmails(ctx, in)
}

// ListEmailsByCursor delegates to the remote message-service.
func (c *Client) ListEmailsByCursor(ctx context.Context, in *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	return c.cli.ListEmailsByCursor(ctx, in)
}

// GetEmailStats delegates to the remote message-service.
func (c *Client) GetEmailStats(ctx context.Context, in *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return c.cli.GetEmailStats(ctx, in)
}

// GetSMS delegates to the remote message-service.
func (c *Client) GetSMS(ctx context.Context, in *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return c.cli.GetSMS(ctx, in)
}

// ListSMS delegates to the remote message-service.
func (c *Client) ListSMS(ctx context.Context, in *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return c.cli.ListSMS(ctx, in)
}

// ListSMSByCursor delegates to the remote message-service.
func (c *Client) ListSMSByCursor(ctx context.Context, in *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	return c.cli.ListSMSByCursor(ctx, in)
}

// GetSMSStats delegates to the remote message-service.
func (c *Client) GetSMSStats(ctx context.Context, in *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return c.cli.GetSMSStats(ctx, in)
}

// ListSMSRegions delegates to the remote message-service.
func (c *Client) ListSMSRegions(ctx context.Context, in *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	return c.cli.ListSMSRegions(ctx, in)
}

// ListSMSSenders delegates to the remote message-service.
func (c *Client) ListSMSSenders(ctx context.Context, in *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	return c.cli.ListSMSSenders(ctx, in)
}

// ListEmailSenders delegates to the remote message-service.
func (c *Client) ListEmailSenders(ctx context.Context, in *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	return c.cli.ListEmailSenders(ctx, in)
}
