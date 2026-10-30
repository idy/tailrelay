package tailrelay

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/varbin"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

// A private loopback fixture. It only dials explicitly supplied loopback
// targets, regardless of the destination requested by the relay.
type socksFixture struct {
	address   string
	requests  chan socks5.Request
	greetings chan struct{}
	stop      func()
}

func startSOCKS(t *testing.T, routes map[string]string, mode string, udpBind M.Socksaddr) socksFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan socks5.Request, 64)
	greetings := make(chan struct{}, 64)
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				defer func() {
					mu.Lock()
					delete(connections, conn)
					mu.Unlock()
				}()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := varbin.StubReader(conn)
				auth, err := socks5.ReadAuthRequest(reader)
				if err != nil || len(auth.Methods) != 1 || auth.Methods[0] != socks5.AuthTypeUsernamePassword {
					return
				}
				greetings <- struct{}{}
				if mode == "stall" {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				method := byte(socks5.AuthTypeUsernamePassword)
				if mode == "downgrade" {
					method = socks5.AuthTypeNotRequired
				}
				if socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: method}) != nil || mode == "downgrade" {
					return
				}
				login, err := socks5.ReadUsernamePasswordAuthRequest(reader)
				if err != nil {
					return
				}
				authStatus := byte(socks5.UsernamePasswordStatusSuccess)
				if login.Username != "fixture-user" || login.Password != "fixture-password" || mode == "auth-failure" {
					authStatus = socks5.UsernamePasswordStatusFailure
				}
				if socks5.WriteUsernamePasswordAuthResponse(conn, socks5.UsernamePasswordAuthResponse{Status: authStatus}) != nil ||
					authStatus != socks5.UsernamePasswordStatusSuccess {
					return
				}
				request, err := socks5.ReadRequest(reader)
				if err != nil {
					return
				}
				requests <- request
				if request.Command == socks5.CommandUDPAssociate {
					if socks5.WriteResponse(conn, socks5.Response{Bind: udpBind}) != nil {
						return
					}
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				target, allowed := routes[request.Destination.String()]
				if !allowed || request.Command != socks5.CommandConnect {
					_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeNotAllowed, Bind: M.ParseSocksaddr("0.0.0.0:0")})
					return
				}
				peer, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				defer peer.Close()
				if socks5.WriteResponse(conn, socks5.Response{Bind: M.SocksaddrFromNet(peer.LocalAddr())}) != nil {
					return
				}
				_ = conn.SetDeadline(time.Time{})
				copied := make(chan struct{})
				go func() {
					_, _ = io.Copy(conn, peer)
					_ = conn.(*net.TCPConn).CloseWrite()
					close(copied)
				}()
				_, _ = io.Copy(peer, conn)
				_ = peer.(*net.TCPConn).CloseWrite()
				<-copied
			}()
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			listener.Close()
			mu.Lock()
			for conn := range connections {
				conn.Close()
			}
			mu.Unlock()
			workers.Wait()
		})
	}
	t.Cleanup(stop)
	return socksFixture{address: listener.Addr().String(), requests: requests, greetings: greetings, stop: stop}
}

func fixtureOutbound(t *testing.T, address string, udp bool) *ispOutbound {
	t.Helper()
	endpoint := netip.MustParseAddrPort(address)
	gate := new(forwardingGate)
	gate.setAllowed(true)
	ctx := context.WithValue(context.Background(), forwardingContextKey{}, gate)
	out, err := newISPOutbound(ctx, nil, nil, "isp", ispOptions{
		Server: endpoint.Addr().String(), ServerPort: endpoint.Port(), UDP: udp,
		Username: "fixture-user", Password: "fixture-password", ConnectTimeout: "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*ispOutbound)
}

func TestProxyRejectsAuthFailureAndDowngrade(t *testing.T) {
	for _, mode := range []string{"auth-failure", "downgrade"} {
		t.Run(mode, func(t *testing.T) {
			fixture := startSOCKS(t, nil, mode, M.Socksaddr{})
			proxy := fixtureOutbound(t, fixture.address, false)
			conn, err := proxy.DialContext(context.Background(), "tcp", M.ParseSocksaddr("203.0.113.80:443"))
			if err == nil {
				conn.Close()
				t.Fatal("failed/downgraded authentication accepted")
			}
			for _, secret := range []string{"fixture-user", "fixture-password"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("proxy error contains credentials")
				}
			}
			select {
			case <-fixture.requests:
				t.Fatal("destination request sent without required authentication")
			default:
			}
		})
	}
}

func TestUnavailableProxyNeverDialsDirectDestination(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	fixture := startSOCKS(t, nil, "", M.Socksaddr{})
	proxy := fixtureOutbound(t, fixture.address, false)
	fixture.stop()
	conn, err := proxy.DialContext(context.Background(), "tcp", M.ParseSocksaddr(target.Addr().String()))
	if err == nil {
		conn.Close()
		t.Fatal("proxy outage fell back to the reachable direct target")
	}
	_ = target.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if conn, err := target.Accept(); err == nil {
		conn.Close()
		t.Fatal("direct destination received a connection during proxy outage")
	}
}

func TestProxyHandshakeCancellation(t *testing.T) {
	fixture := startSOCKS(t, nil, "stall", M.Socksaddr{})
	proxy := fixtureOutbound(t, fixture.address, false)
	proxy.timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := proxy.DialContext(ctx, "tcp", M.ParseSocksaddr("203.0.113.80:443"))
		done <- err
	}()
	select {
	case <-fixture.greetings:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("client did not reach the stalled SOCKS5 handshake")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled handshake succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation left the handshake blocked")
	}
}

func TestUDPRequiresOptInAndSameISPAddress(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, from, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(buffer[:n], from)
		}
	}()
	destination := M.ParseSocksaddr("203.0.113.80:443")
	fixture := startSOCKS(t, nil, "", M.SocksaddrFromNet(udp.LocalAddr()))
	disabled := fixtureOutbound(t, fixture.address, false)
	if conn, err := disabled.DialContext(context.Background(), "udp", destination); err == nil {
		conn.Close()
		t.Fatal("ordinary UDP was enabled without opt-in")
	}
	enabled := fixtureOutbound(t, fixture.address, true)
	conn, err := enabled.DialContext(context.Background(), "udp", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("via-isp-only")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4096)
	n, err := conn.Read(payload)
	if err != nil || string(payload[:n]) != "via-isp-only" {
		t.Fatalf("UDP association did not carry the packet: n=%d err=%v", n, err)
	}
	enabled.gate.setAllowed(false)
	if _, err := conn.Write([]byte("must-not-forward")); err == nil {
		t.Fatal("UDP association survived authorization revocation")
	}
	foreign := startSOCKS(t, nil, "", M.ParseSocksaddr("203.0.113.99:12345"))
	if conn, err := fixtureOutbound(t, foreign.address, true).DialContext(context.Background(), "udp", destination); err == nil {
		conn.Close()
		t.Fatal("proxy redirected datagrams to a different physical egress")
	}
}
