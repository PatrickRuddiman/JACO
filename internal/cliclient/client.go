package cliclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcinsecure "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Client is the CLI's gRPC handle. Built once per command invocation from a
// resolved Context; closes any cached connections via Close.
type Client struct {
	addrs    []string
	creds    credentials.TransportCredentials
	token    string
	serverNm string // TLS ServerName override, empty for default
	logger   *slog.Logger

	mu   sync.Mutex
	conn *grpc.ClientConn // current connection, lazily dialed
	cur  int              // index into addrs that conn corresponds to
}

// WithLogger attaches a logger so the client emits debug lines for server
// selection + retry decisions (issue #38 CLI logging). nil-safe via log().
// Returns c for chaining.
func (c *Client) WithLogger(l *slog.Logger) *Client {
	c.logger = logging.Subsystem(l, "cliclient")
	return c
}

func (c *Client) log() *slog.Logger {
	if c.logger == nil {
		return logging.Discard()
	}
	return c.logger
}

// NewClient builds a Client from a resolved Context. Reads the CA cert from
// disk to build the TLS config.
func NewClient(ctx *Context) (*Client, error) {
	if len(ctx.ServerAddrs) == 0 {
		return nil, fmt.Errorf("Context has no ServerAddrs")
	}
	caPEM, err := os.ReadFile(ctx.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %s: %w", ctx.CACertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA cert PEM at %s did not parse", ctx.CACertPath)
	}
	return &Client{
		addrs: append([]string(nil), ctx.ServerAddrs...),
		creds: credentials.NewTLS(&tls.Config{RootCAs: pool}),
		token: ctx.Token,
		cur:   -1,
	}, nil
}

// InsecureOptions builds a Client that skips TLS verification — only for
// tests. Real CLI invocations go through NewClient.
type InsecureOptions struct {
	Addrs []string
	Token string
}

// NewInsecure builds a Client with insecure transport credentials. Tests
// only; do not use in production.
func NewInsecure(opts InsecureOptions) *Client {
	return &Client{
		addrs: append([]string(nil), opts.Addrs...),
		creds: grpcinsecure.NewCredentials(),
		token: opts.Token,
		cur:   -1,
	}
}

// Close releases the cached connection if any.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.cur = -1
		return err
	}
	return nil
}

// AuthContext returns ctx with the bearer token attached as
// `authorization: Bearer <token>` outgoing metadata.
func (c *Client) AuthContext(ctx context.Context) context.Context {
	if c.token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
}

// Conn returns a gRPC connection to one of the configured addresses. If a
// previous Invoke established a connection, it's reused; otherwise the first
// address is dialed. Streams use this directly — they DO NOT rotate
// mid-stream (per the spec; operator re-runs on stream break).
func (c *Client) Conn() (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	return c.dialLocked(0)
}

// Invoke calls fn against each configured address in order until fn returns
// a non-retryable result or every address has been tried. Retryable errors
// are: codes.Unavailable, codes.DeadlineExceeded, codes.Internal, and any
// gRPC status whose typed pb.Error details or message contain "no_leader".
//
// On success, the connection that worked is cached so subsequent Invoke
// calls (and stream-mode Conn) reuse it.
func (c *Client) Invoke(ctx context.Context, fn func(*grpc.ClientConn) error) error {
	var lastErr error
	for attempt := 0; attempt < len(c.addrs); attempt++ {
		conn, err := c.getOrDial(attempt)
		if err != nil {
			lastErr = err
			continue
		}
		if err := fn(conn); err != nil {
			lastErr = err
			if shouldRotate(err) {
				c.log().Debug("rotating to next endpoint", "attempt", attempt, "error", err)
				c.discardCurrent()
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("all endpoints unreachable: %w", lastErr)
}

// getOrDial returns the cached connection if it matches index; otherwise
// closes any existing connection and dials addrs[index].
func (c *Client) getOrDial(index int) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.cur == index {
		return c.conn, nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.cur = -1
	}
	return c.dialLocked(index)
}

// dialLocked must be called with c.mu held.
func (c *Client) dialLocked(index int) (*grpc.ClientConn, error) {
	if index < 0 || index >= len(c.addrs) {
		return nil, fmt.Errorf("index %d out of range %d", index, len(c.addrs))
	}
	c.log().Debug("selecting server endpoint", "addr", c.addrs[index], "index", index)
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(c.creds)}
	conn, err := grpc.NewClient(c.addrs[index], dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.addrs[index], err)
	}
	c.conn = conn
	c.cur = index
	return conn, nil
}

func (c *Client) discardCurrent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.cur = -1
	}
}

// shouldRotate reports whether err is the kind of failure that warrants
// trying the next configured address.
func shouldRotate(err error) bool {
	if err == nil {
		return false
	}
	sErr, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch sErr.Code() {
	case codes.Unavailable, codes.DeadlineExceeded:
		// Connection / transport problem — try the next address.
		return true
	case codes.Internal:
		// gRPC's transport sometimes uses Internal for opaque transport
		// failures; only rotate if the body looks like no_leader.
	}
	if strings.Contains(sErr.Message(), "no_leader") {
		return true
	}
	for _, d := range sErr.Details() {
		if e, ok := d.(*pb.Error); ok && e.GetCode() == "no_leader" {
			return true
		}
	}
	return false
}

// ErrAllExhausted is the sentinel "every endpoint failed" error returned via
// Invoke. Callers can `errors.Is(err, cliclient.ErrAllExhausted)` to gate on
// it specifically.
var ErrAllExhausted = errors.New("all endpoints unreachable")
