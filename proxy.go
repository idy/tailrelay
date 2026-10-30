package tailrelay

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/varbin"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

const ispOutboundType = "tailrelay-isp"

type ispOptions struct {
	Server         string `json:"server"`
	ServerPort     uint16 `json:"server_port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	UDP            bool   `json:"udp"`
	ConnectTimeout string `json:"connect_timeout"`
}

// ispOutbound has one physical destination: the configured ISP proxy. There
// is no direct-destination dial, hostname lookup, alternate proxy or fallback.
type ispOutbound struct {
	outbound.Adapter
	server      netip.AddrPort
	credentials Credentials
	timeout     time.Duration
	udp         bool
	gate        *forwardingGate
}

func registerISP(registry *outbound.Registry) {
	outbound.Register[ispOptions](registry, ispOutboundType, newISPOutbound)
}

func newISPOutbound(ctx context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options ispOptions) (adapter.Outbound, error) {
	address, err := netip.ParseAddr(options.Server)
	if err != nil || options.ServerPort == 0 {
		return nil, errors.New("ISP proxy requires a literal address and a port")
	}
	credentials := Credentials{Username: options.Username, Password: options.Password}
	if err := credentials.validate(); err != nil {
		return nil, err
	}
	timeout, err := time.ParseDuration(options.ConnectTimeout)
	if err != nil || timeout <= 0 {
		return nil, errors.New("ISP proxy requires a positive connect timeout")
	}
	networks := []string{"tcp"}
	if options.UDP {
		networks = append(networks, "udp")
	}
	gate, _ := ctx.Value(forwardingContextKey{}).(*forwardingGate)
	return &ispOutbound{
		Adapter:     outbound.NewAdapter(ispOutboundType, tag, networks, nil),
		server:      netip.AddrPortFrom(address.Unmap(), options.ServerPort),
		credentials: credentials, timeout: timeout, udp: options.UDP,
		gate: gate,
	}, nil
}

func (p *ispOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (result net.Conn, err error) {
	if !p.gate.isAllowed() {
		return nil, errForwardingBlocked
	}
	if network != "tcp" && (network != "udp" || !p.udp) {
		return nil, errors.New("ISP proxy does not support this network")
	}
	if destination.IsDomain() || !destination.Addr.IsValid() || destination.Port == 0 {
		return nil, errors.New("forwarded destinations must be literal IP addresses with a nonzero port")
	}
	command := byte(socks5.CommandConnect)
	if network == "udp" {
		command = socks5.CommandUDPAssociate
	}
	dialer := &net.Dialer{Timeout: p.timeout, KeepAlive: 30 * time.Second}
	control, err := dialer.DialContext(ctx, "tcp", p.server.String())
	if err != nil {
		return nil, errors.New("cannot connect to ISP proxy")
	}
	control, err = p.gate.track(control)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			control.Close()
		}
	}()
	_ = control.SetDeadline(time.Now().Add(p.timeout))
	stopCancel := context.AfterFunc(ctx, func() { control.Close() })
	defer func() {
		if !stopCancel() {
			if result != nil {
				result.Close()
			}
			result, err = nil, ctx.Err()
		}
	}()
	request := destination
	if command == socks5.CommandUDPAssociate {
		// No destination datagram is sent until SOCKS5 grants the association.
		request = M.Socksaddr{Addr: netip.IPv4Unspecified()}
	}
	bind, err := p.handshake(control, command, request)
	if err != nil {
		return nil, err
	}
	_ = control.SetDeadline(time.Time{})
	if network == "tcp" {
		return control, nil
	}
	// ISP servers may return 0.0.0.0 as "this server". Never resolve or dial
	// a different host supplied in a UDP-associate response.
	if bind.Addr.IsUnspecified() {
		bind.Addr = p.server.Addr()
	}
	if bind.IsDomain() || bind.Addr.Unmap() != p.server.Addr() || bind.Port == 0 {
		return nil, errors.New("ISP UDP relay must use the configured proxy IP")
	}
	udp, err := dialer.DialContext(ctx, "udp", bind.String())
	if err != nil {
		return nil, errors.New("cannot connect to ISP UDP relay")
	}
	return p.gate.track(socks.NewAssociatePacketConn(udp, destination, control))
}

func (p *ispOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, err := p.DialContext(ctx, "udp", destination)
	if err != nil {
		return nil, err
	}
	return conn.(net.PacketConn), nil
}

func (p *ispOutbound) handshake(conn net.Conn, command byte, destination M.Socksaddr) (M.Socksaddr, error) {
	reader := varbin.StubReader(conn)
	if err := socks5.WriteAuthRequest(conn, socks5.AuthRequest{
		Methods: []byte{socks5.AuthTypeUsernamePassword},
	}); err != nil {
		return M.Socksaddr{}, errors.New("ISP SOCKS5 greeting failed")
	}
	response, err := socks5.ReadAuthResponse(reader)
	if err != nil || response.Method != socks5.AuthTypeUsernamePassword {
		return M.Socksaddr{}, errors.New("ISP proxy did not accept required username/password authentication")
	}
	if err := socks5.WriteUsernamePasswordAuthRequest(conn, socks5.UsernamePasswordAuthRequest{
		Username: p.credentials.Username, Password: p.credentials.Password,
	}); err != nil {
		return M.Socksaddr{}, errors.New("ISP SOCKS5 authentication failed")
	}
	auth, err := socks5.ReadUsernamePasswordAuthResponse(reader)
	if err != nil || auth.Status != socks5.UsernamePasswordStatusSuccess {
		return M.Socksaddr{}, errors.New("ISP SOCKS5 authentication failed")
	}
	if err := socks5.WriteRequest(conn, socks5.Request{Command: command, Destination: destination}); err != nil {
		return M.Socksaddr{}, errors.New("ISP SOCKS5 request failed")
	}
	result, err := socks5.ReadResponse(reader)
	if err != nil || result.ReplyCode != socks5.ReplyCodeSuccess {
		return M.Socksaddr{}, errors.New("ISP proxy rejected the destination or UDP association")
	}
	return result.Bind, nil
}
