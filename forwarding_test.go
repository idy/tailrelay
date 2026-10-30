package tailrelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestForwardingPreservesEOFBeforeTheResponse(t *testing.T) {
	target, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	upstreamDone := make(chan error, 1)
	go func() {
		conn, err := target.AcceptTCP()
		if err != nil {
			upstreamDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		request, err := io.ReadAll(conn)
		if err == nil {
			_, err = fmt.Fprint(conn, "response:"+string(request))
		}
		upstreamDone <- err
	}()
	destination := "203.0.113.80:80"
	proxy := startSOCKS(t, map[string]string{destination: target.Addr().String()}, "", M.Socksaddr{})
	config := testConfig(t, "half-close")
	config.Proxy.Server = proxy.address
	instance := startTestEngine(t, config, nil, false)

	// Use real TCP on both sides of the router so EOF can be delivered as
	// a write-side FIN without closing the response direction.
	inbound, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbound.Close() })
	accepted := make(chan *net.TCPConn, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		conn, err := inbound.AcceptTCP()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
		instance.Router().RouteConnectionEx(ctx, conn, flow(destination, "tcp"), nil)
	}()
	client, err := net.DialTCP("tcp", nil, inbound.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("cannot accept the routed client")
	}
	defer server.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprint(client, "request"); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil || string(response) != "response:request" {
		t.Fatalf("EOF-terminated exchange lost its response: response=%q err=%v", response, err)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatal(err)
	}
}

func TestStartupGateBlocksRouterTCPAndDNSBeforeTailnetVerification(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "verified-isp")
	}))
	t.Cleanup(peer.Close)
	destination := "203.0.113.80:80"
	proxy := startSOCKS(t, map[string]string{destination: peer.Listener.Addr().String()}, "", M.Socksaddr{})
	config := testConfig(t, "startup-gate")
	config.Proxy.Server = proxy.address
	gate := new(forwardingGate)
	instance := startTestEngineWithGate(t, config, nil, false, gate)
	assertBlocked := func() {
		t.Helper()
		conn := routedTCP(t, instance, destination)
		_, _ = fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: fixture.test\r\n\r\n")
		body, _ := io.ReadAll(conn)
		conn.Close()
		if containsBody(string(body), "verified-isp") {
			t.Fatal("unverified Tailnet forwarded TCP")
		}
		reply, err := udpQuery(instance, "unverified.tailrelay.invalid", 200*time.Millisecond)
		if err == nil && reply.Rcode == dns.RcodeSuccess {
			t.Fatal("unverified Tailnet forwarded DNS")
		}
		select {
		case <-proxy.greetings:
			t.Fatal("unverified Tailnet opened an ISP connection")
		default:
		}
	}
	assertBlocked()
	status := &adapter.TailscaleEndpointStatus{
		BackendState: "Running", NetworkName: "expected.test", Self: &adapter.TailscalePeer{},
	}
	if err := gate.observe("expected.test", false, status); err != nil {
		t.Fatal(err)
	}
	if body := routedGET(t, instance, destination); !containsBody(body, "verified-isp") {
		t.Fatal("verified Tailnet did not forward through its ISP")
	}
	<-proxy.greetings

	// Keep a real TCP connection open across the status change. Revocation
	// must also close pooled DoH/HTTP and other already established traffic.
	out, _ := instance.Outbound().Outbound("isp")
	conn, err := out.DialContext(context.Background(), "tcp", M.ParseSocksaddr(destination))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-proxy.greetings
	status.NetworkName = "wrong.test"
	if err := gate.observe("expected.test", false, status); err == nil {
		t.Fatal("retained wrong-Tailnet identity was accepted")
	}
	if _, err := conn.Write([]byte("must-not-forward")); err == nil {
		t.Fatal("active connection survived authorization revocation")
	}
	assertBlocked()
}

func TestGateRequiresRunningUnexpiredSelfAndNeverEnablesLoginForwarding(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    *adapter.TailscaleEndpointStatus
		loginOnly bool
	}{
		{name: "no-status"},
		{name: "no-self", status: &adapter.TailscaleEndpointStatus{BackendState: "Running", NetworkName: "expected.test"}},
		{name: "stopped", status: &adapter.TailscaleEndpointStatus{BackendState: "Stopped", Self: &adapter.TailscalePeer{}}},
		{name: "expired", status: &adapter.TailscaleEndpointStatus{BackendState: "Running", Self: &adapter.TailscalePeer{Expired: true}}},
		{name: "login-required", status: &adapter.TailscaleEndpointStatus{BackendState: "Running", AuthURL: "private", Self: &adapter.TailscalePeer{}}},
		{name: "login-only", loginOnly: true, status: &adapter.TailscaleEndpointStatus{BackendState: "Running", NetworkName: "expected.test", Self: &adapter.TailscalePeer{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := new(forwardingGate)
			gate.setAllowed(true)
			client, peer := net.Pipe()
			defer peer.Close()
			tracked, err := gate.track(client)
			if err != nil {
				t.Fatal(err)
			}
			defer tracked.Close()
			if err := gate.observe("expected.test", test.loginOnly, test.status); err != nil {
				t.Fatal(err)
			}
			if gate.isAllowed() {
				t.Fatal("unverified state enabled forwarding")
			}
			if _, err := tracked.Write([]byte("blocked")); err == nil {
				t.Fatal("unverified state retained an active connection")
			}
		})
	}
}

func TestOutboundWithoutAnEngineGateCannotOpenAnISPSocket(t *testing.T) {
	fixture := startSOCKS(t, nil, "", M.Socksaddr{})
	proxy := fixtureOutbound(t, fixture.address, true)
	proxy.gate = nil
	for _, network := range []string{"tcp", "udp"} {
		if conn, err := proxy.DialContext(context.Background(), network, M.ParseSocksaddr("203.0.113.80:443")); err == nil {
			conn.Close()
			t.Fatalf("ungated %s outbound opened a connection", network)
		}
	}
	select {
	case <-fixture.greetings:
		t.Fatal("ungated outbound connected to the ISP")
	default:
	}
}
