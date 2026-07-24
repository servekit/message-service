package messageservice

import (
	"fmt"

	pb "github.com/servekit/message-service/gen/message/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a gRPC connection to message-service.
// Embeds pb.MessageServiceClient so all RPC methods are directly available.
type Client struct {
	conn *grpc.ClientConn
	pb.MessageServiceClient
}

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
	return &Client{conn: conn, MessageServiceClient: pb.NewMessageServiceClient(conn)}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }
