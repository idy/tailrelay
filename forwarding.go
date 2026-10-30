package tailrelay

import (
	"errors"
	"net"
	"sync"

	"github.com/sagernet/sing-box/adapter"
)

type forwardingContextKey struct{}

var errForwardingBlocked = errors.New("client forwarding is blocked until the expected Tailnet is verified")

// Each engine starts denied, including when it loads an already enrolled
// identity. Control-plane connectivity never needs the ISP outbound.
type forwardingGate struct {
	mu          sync.Mutex
	allowed     bool
	connections map[*forwardingConn]struct{}
}

func (g *forwardingGate) isAllowed() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowed
}

func (g *forwardingGate) setAllowed(allowed bool) {
	g.mu.Lock()
	g.allowed = allowed
	var closing []*forwardingConn
	if !allowed {
		for conn := range g.connections {
			closing = append(closing, conn)
		}
		g.connections = nil
	}
	g.mu.Unlock()
	// Closing outside the lock also removes pooled DoH and active TCP/UDP
	// connections; they cannot continue after authorization is revoked.
	for _, conn := range closing {
		_ = conn.Close()
	}
}

func (g *forwardingGate) observe(expected string, loginOnly bool, status *adapter.TailscaleEndpointStatus) error {
	ready := status != nil && status.BackendState == "Running" &&
		status.AuthURL == "" && status.Self != nil && !status.Self.Expired
	var err error
	if ready && expected != "" && status.NetworkName != expected {
		ready = false
		err = errors.New("device joined an unexpected Tailnet; check the login account and expected_tailnet")
	}
	g.setAllowed(ready && !loginOnly)
	return err
}

func (g *forwardingGate) track(conn net.Conn) (net.Conn, error) {
	g.mu.Lock()
	if !g.allowed {
		g.mu.Unlock()
		_ = conn.Close()
		return nil, errForwardingBlocked
	}
	tracked := &forwardingConn{Conn: conn, gate: g}
	if g.connections == nil {
		g.connections = make(map[*forwardingConn]struct{})
	}
	g.connections[tracked] = struct{}{}
	g.mu.Unlock()
	if packet, ok := conn.(net.PacketConn); ok {
		return &forwardingPacketConn{forwardingConn: tracked, packet: packet}, nil
	}
	return tracked, nil
}

type forwardingConn struct {
	net.Conn
	gate      *forwardingGate
	closeOnce sync.Once
	closeErr  error
}

func (c *forwardingConn) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return errors.ErrUnsupported
}

func (c *forwardingConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (c *forwardingConn) Close() error {
	c.closeOnce.Do(func() {
		c.gate.mu.Lock()
		delete(c.gate.connections, c)
		c.gate.mu.Unlock()
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

type forwardingPacketConn struct {
	*forwardingConn
	packet net.PacketConn
}

func (c *forwardingPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	return c.packet.ReadFrom(buffer)
}

func (c *forwardingPacketConn) WriteTo(buffer []byte, destination net.Addr) (int, error) {
	return c.packet.WriteTo(buffer, destination)
}
