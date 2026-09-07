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
// implements the generated pb.MessageServiceServer and
// pb.MessageAdminServiceServer interfaces (unary methods without
// grpc.CallOption), so a consumer can hold either backend behind those
// generated interfaces — module mode passes the *Handler, grpc mode passes
// the *Client — with no per-consumer adapter.
//
// The Unimplemented embeds satisfy the interfaces' mustEmbed guards; every
// RPC below shadows them with a real delegation. When a new RPC is added to
// the proto, add its delegation here — until then grpc mode returns
// codes.Unimplemented for it.
type Client struct {
	pb.UnimplementedMessageServiceServer
	pb.UnimplementedMessageAdminServiceServer

	conn *grpc.ClientConn
	cli  pb.MessageServiceClient
	adm  pb.MessageAdminServiceClient
}

// Compile-time assertions: *Client and *Handler expose the same interfaces.
var (
	_ pb.MessageServiceServer       = (*Client)(nil)
	_ pb.MessageAdminServiceServer  = (*Client)(nil)
)

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
	return &Client{
		conn: conn,
		cli:  pb.NewMessageServiceClient(conn),
		adm:  pb.NewMessageAdminServiceClient(conn),
	}, nil
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

// --- admin delegations (MessageAdminService) ---

// CreateApp delegates to the remote message-service admin surface.
func (c *Client) CreateApp(ctx context.Context, in *pb.CreateAppRequest) (*pb.CreateAppResponse, error) {
	return c.adm.CreateApp(ctx, in)
}

// GetApp delegates to the remote message-service admin surface.
func (c *Client) GetApp(ctx context.Context, in *pb.GetAppRequest) (*pb.GetAppResponse, error) {
	return c.adm.GetApp(ctx, in)
}

// UpdateApp delegates to the remote message-service admin surface.
func (c *Client) UpdateApp(ctx context.Context, in *pb.UpdateAppRequest) (*pb.UpdateAppResponse, error) {
	return c.adm.UpdateApp(ctx, in)
}

// RotateAppSecret delegates to the remote message-service admin surface.
func (c *Client) RotateAppSecret(ctx context.Context, in *pb.RotateAppSecretRequest) (*pb.RotateAppSecretResponse, error) {
	return c.adm.RotateAppSecret(ctx, in)
}

// ListApps delegates to the remote message-service admin surface.
func (c *Client) ListApps(ctx context.Context, in *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	return c.adm.ListApps(ctx, in)
}

// DeleteApp delegates to the remote message-service admin surface.
func (c *Client) DeleteApp(ctx context.Context, in *pb.DeleteAppRequest) (*emptypb.Empty, error) {
	return c.adm.DeleteApp(ctx, in)
}

// CreateChannelAccount delegates to the remote message-service admin surface.
func (c *Client) CreateChannelAccount(ctx context.Context, in *pb.CreateChannelAccountRequest) (*pb.CreateChannelAccountResponse, error) {
	return c.adm.CreateChannelAccount(ctx, in)
}

// UpdateChannelAccount delegates to the remote message-service admin surface.
func (c *Client) UpdateChannelAccount(ctx context.Context, in *pb.UpdateChannelAccountRequest) (*pb.UpdateChannelAccountResponse, error) {
	return c.adm.UpdateChannelAccount(ctx, in)
}

// DeleteChannelAccount delegates to the remote message-service admin surface.
func (c *Client) DeleteChannelAccount(ctx context.Context, in *pb.DeleteChannelAccountRequest) (*emptypb.Empty, error) {
	return c.adm.DeleteChannelAccount(ctx, in)
}

// ListChannelAccounts delegates to the remote message-service admin surface.
func (c *Client) ListChannelAccounts(ctx context.Context, in *pb.ListChannelAccountsRequest) (*pb.ListChannelAccountsResponse, error) {
	return c.adm.ListChannelAccounts(ctx, in)
}

// CreateSignature delegates to the remote message-service admin surface.
func (c *Client) CreateSignature(ctx context.Context, in *pb.CreateSignatureRequest) (*pb.CreateSignatureResponse, error) {
	return c.adm.CreateSignature(ctx, in)
}

// UpdateSignature delegates to the remote message-service admin surface.
func (c *Client) UpdateSignature(ctx context.Context, in *pb.UpdateSignatureRequest) (*pb.UpdateSignatureResponse, error) {
	return c.adm.UpdateSignature(ctx, in)
}

// DeleteSignature delegates to the remote message-service admin surface.
func (c *Client) DeleteSignature(ctx context.Context, in *pb.DeleteSignatureRequest) (*emptypb.Empty, error) {
	return c.adm.DeleteSignature(ctx, in)
}

// ListSignatures delegates to the remote message-service admin surface.
func (c *Client) ListSignatures(ctx context.Context, in *pb.ListSignaturesRequest) (*pb.ListSignaturesResponse, error) {
	return c.adm.ListSignatures(ctx, in)
}

// CreateTemplate delegates to the remote message-service admin surface.
func (c *Client) CreateTemplate(ctx context.Context, in *pb.CreateTemplateRequest) (*pb.CreateTemplateResponse, error) {
	return c.adm.CreateTemplate(ctx, in)
}

// UpdateTemplate delegates to the remote message-service admin surface.
func (c *Client) UpdateTemplate(ctx context.Context, in *pb.UpdateTemplateRequest) (*pb.UpdateTemplateResponse, error) {
	return c.adm.UpdateTemplate(ctx, in)
}

// DeleteTemplate delegates to the remote message-service admin surface.
func (c *Client) DeleteTemplate(ctx context.Context, in *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	return c.adm.DeleteTemplate(ctx, in)
}

// ListTemplates delegates to the remote message-service admin surface.
func (c *Client) ListTemplates(ctx context.Context, in *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	return c.adm.ListTemplates(ctx, in)
}

// CreatePolicy delegates to the remote message-service admin surface.
func (c *Client) CreatePolicy(ctx context.Context, in *pb.CreatePolicyRequest) (*pb.CreatePolicyResponse, error) {
	return c.adm.CreatePolicy(ctx, in)
}

// UpdatePolicy delegates to the remote message-service admin surface.
func (c *Client) UpdatePolicy(ctx context.Context, in *pb.UpdatePolicyRequest) (*pb.UpdatePolicyResponse, error) {
	return c.adm.UpdatePolicy(ctx, in)
}

// DeletePolicy delegates to the remote message-service admin surface.
func (c *Client) DeletePolicy(ctx context.Context, in *pb.DeletePolicyRequest) (*emptypb.Empty, error) {
	return c.adm.DeletePolicy(ctx, in)
}

// ListPolicies delegates to the remote message-service admin surface.
func (c *Client) ListPolicies(ctx context.Context, in *pb.ListPoliciesRequest) (*pb.ListPoliciesResponse, error) {
	return c.adm.ListPolicies(ctx, in)
}
