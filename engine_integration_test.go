package tailrelay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
)

// These tests run the real router, restricted ISP outbound and verified DoH
// transport. Only Tailnet packet delivery is replaced by calls at the same
// router boundary used by the embedded endpoint. No login, kernel TUN, host
// routes, public listeners or real ISP account are used.
func startTestEngine(t *testing.T, config Config, certificate []byte, loginOnly bool) *box.Box {
	t.Helper()
	gate := new(forwardingGate)
	gate.setAllowed(!loginOnly)
	return startTestEngineWithGate(t, config, certificate, loginOnly, gate)
}

func startTestEngineWithGate(t *testing.T, config Config, certificate []byte, loginOnly bool, gate *forwardingGate) *box.Box {
	t.Helper()
	// Real instances cannot share a state/cache directory concurrently.
	config.StateDir = filepath.Join(t.TempDir(), "state")
	if err := privateDirectory(config.StateDir); err != nil {
		t.Fatal(err)
	}
	compiled, err := compile(config, loginOnly)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := injectSecrets(compiled, Credentials{Username: "fixture-user", Password: "fixture-password"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var native object
	if err := json.Unmarshal(wire, &native); err != nil {
		t.Fatal(err)
	}
	delete(native, "endpoints")
	if certificate != nil {
		exit := native["dns"].(map[string]any)["servers"].([]any)[1].(map[string]any)
		// Trust only the private fixture CA while retaining TLS validation.
		exit["tls"].(map[string]any)["certificate"] = []string{string(certificate)}
	}
	wire, _ = json.Marshal(native)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, forwardingContextKey{}, gate)
	ctx = engineContext(ctx)
	options, err := parseNative(ctx, wire)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := instance.Start(); err != nil {
		cancel()
		instance.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gate.setAllowed(false)
		cancel()
		if err := instance.Close(); err != nil {
			t.Error(err)
		}
	})
	return instance
}

func flow(destination, network string) adapter.InboundContext {
	return adapter.InboundContext{
		Inbound: "tailnet", InboundType: "tailscale", Network: network,
		Source: M.ParseSocksaddr("100.64.0.2:20000"), Destination: M.ParseSocksaddr(destination),
	}
}

func routedTCP(t *testing.T, instance *box.Box, destination string) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
	})
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	go instance.Router().RouteConnectionEx(ctx, server, flow(destination, "tcp"), nil)
	return client
}

func routedGET(t *testing.T, instance *box.Box, destination string) string {
	t.Helper()
	conn := routedTCP(t, instance, destination)
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: fixture.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(wire)
}

func TestTwoInstancesRouteToDifferentISPs(t *testing.T) {
	destination := "203.0.113.80:80"
	for _, name := range []string{"isp-sf", "isp-seattle"} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, name)
		}))
		t.Cleanup(peer.Close)
		proxy := startSOCKS(t, map[string]string{destination: peer.Listener.Addr().String()}, "", M.Socksaddr{})
		config := testConfig(t, name)
		config.Proxy.Server = proxy.address
		instance := startTestEngine(t, config, nil, false)
		body := routedGET(t, instance, destination)
		if !containsBody(body, name) {
			t.Fatalf("instance %s did not use its own ISP fixture: %s", name, body)
		}
		select {
		case request := <-proxy.requests:
			if request.Destination.String() != destination {
				t.Fatal("ISP received a different destination")
			}
		default:
			t.Fatal("request bypassed the required proxy")
		}
	}
}

func containsBody(response, body string) bool {
	return len(response) >= len(body) && response[len(response)-len(body):] == body
}

func TestRouterRejectsPrivateDestinationsUDPAndICMP(t *testing.T) {
	config := testConfig(t, "isp-sf")
	instance := startTestEngine(t, config, nil, false)
	router := instance.Router()
	for _, destination := range []string{
		"127.0.0.1:80", "10.1.1.1:80", "172.16.1.1:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.64.0.3:80", "198.18.0.1:80",
		"[::1]:80", "[fd7a:115c:a1e0::1]:80", "[fe80::1]:80", "[ff02::1]:80",
	} {
		t.Run(destination, func(t *testing.T) {
			result := router.PreMatch(flow(destination, "tcp"), nil)
			if result.Action != adapter.PreMatchReject && result.Action != adapter.PreMatchDrop {
				t.Fatalf("restricted destination is not rejected: %+v", result)
			}
		})
	}
	for _, network := range []string{"udp", "icmp"} {
		result := router.PreMatch(flow("203.0.113.80:443", network), nil)
		if result.Action != adapter.PreMatchReject && result.Action != adapter.PreMatchDrop {
			t.Fatalf("%s is not rejected: %+v", network, result)
		}
	}
	for _, destination := range []string{"8.8.8.8:53", "192.168.1.1:53", "[::1]:53"} {
		if result := router.PreMatch(flow(destination, "udp"), nil); result.Action != adapter.PreMatchHijackDNS {
			t.Fatalf("DNS must be intercepted before general UDP/private rejection: %+v", result)
		}
	}
	foreign := flow("203.0.113.80:80", "tcp")
	foreign.Inbound = "unexpected"
	if result := router.PreMatch(foreign, nil); result.Action != adapter.PreMatchReject {
		t.Fatal("unexpected inbound was not rejected")
	}
	login := startTestEngine(t, config, nil, true)
	if result := login.Router().PreMatch(flow("203.0.113.80:80", "tcp"), nil); result.Action != adapter.PreMatchDrop {
		t.Fatal("interactive login can forward client traffic")
	}
}

type dnsWriter struct {
	replies chan []byte
}

func (w dnsWriter) WritePacket(buffer *buf.Buffer, _ M.Socksaddr) error {
	defer buffer.Release()
	reply := append([]byte(nil), buffer.Bytes()...)
	select {
	case w.replies <- reply:
	default:
	}
	return nil
}

func udpQuery(instance *box.Box, name string, timeout time.Duration) (*dns.Msg, error) {
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(name), dns.TypeA)
	wire, _ := query.Pack()
	writer := dnsWriter{replies: make(chan []byte, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	instance.Router().HijackDNSPacket(ctx, wire, writer, flow("192.168.1.1:53", "udp"))
	select {
	case reply := <-writer.replies:
		message := new(dns.Msg)
		return message, message.Unpack(reply)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDNSUsesVerifiedDoHThroughISPWithoutFallback(t *testing.T) {
	var served atomic.Int32
	doh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/dns-query" || request.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "bad DoH request", http.StatusBadRequest)
			return
		}
		served.Add(1)
		wire, _ := io.ReadAll(io.LimitReader(request.Body, 4096))
		query := new(dns.Msg)
		if query.Unpack(wire) != nil || len(query.Question) != 1 {
			http.Error(w, "bad DNS", http.StatusBadRequest)
			return
		}
		reply := new(dns.Msg)
		reply.SetReply(query)
		reply.RecursionAvailable = true
		reply.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
			A:   net.IPv4(203, 0, 113, 80),
		}}
		wire, _ = reply.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	t.Cleanup(doh.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: doh.Certificate().Raw})
	destination := doh.Listener.Addr().String()
	proxy := startSOCKS(t, map[string]string{destination: destination}, "", M.Socksaddr{})
	config := testConfig(t, "isp-sf")
	config.Proxy.Server = proxy.address
	host, portText, _ := net.SplitHostPort(destination)
	config.DNS.Server, config.DNS.Port, config.DNS.ServerName = host, mustPort(t, portText), "example.com"
	instance := startTestEngine(t, config, certificate, false)

	answer, err := udpQuery(instance, "udp-client.tailrelay.invalid", 3*time.Second)
	if err != nil || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "203.0.113.80" {
		t.Fatalf("UDP DNS did not use ISP DoH: answer=%v err=%v", answer, err)
	}
	conn := routedTCP(t, instance, "8.8.8.8:53")
	query := new(dns.Msg)
	query.SetQuestion("tcp-client.tailrelay.invalid.", dns.TypeA)
	wire, _ := query.Pack()
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(wire)))
	frame = append(frame, wire...)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	wire = make([]byte, binary.BigEndian.Uint16(header))
	if _, err := io.ReadFull(conn, wire); err != nil {
		t.Fatal(err)
	}
	reply := new(dns.Msg)
	if err := reply.Unpack(wire); err != nil || len(reply.Answer) != 1 {
		t.Fatal("TCP DNS did not use ISP DoH")
	}
	conn.Close()
	if served.Load() != 2 {
		t.Fatalf("expected two DoH queries, got %d", served.Load())
	}
	select {
	case request := <-proxy.requests:
		if request.Destination.String() != destination {
			t.Fatal("DoH transport bypassed the ISP")
		}
	default:
		t.Fatal("no proxied DoH connection was observed")
	}

	// The same DoH server remains directly reachable on loopback. A direct
	// fallback would reach it and increment served, which is forbidden.
	proxy.stop()
	answer, err = udpQuery(instance, "uncached-after-outage.tailrelay.invalid", 500*time.Millisecond)
	if err == nil && answer.Rcode == dns.RcodeSuccess {
		t.Fatal("DNS succeeded after the required ISP proxy was stopped")
	}
	if served.Load() != 2 {
		t.Fatal("DNS outage bypassed the ISP and dialed the DoH server directly")
	}
	login := startTestEngine(t, config, certificate, true)
	if answer, err = udpQuery(login, "login-only.tailrelay.invalid", 200*time.Millisecond); err == nil && answer.Rcode == dns.RcodeSuccess {
		t.Fatal("login command served peer DNS")
	}
	if served.Load() != 2 {
		t.Fatal("login command contacted the client DNS upstream")
	}
}

func mustPort(t *testing.T, port string) int {
	t.Helper()
	result, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestUDPOptInChangesOnlyISPTransport(t *testing.T) {
	config := testConfig(t, "isp-sf")
	config.Proxy.UDP = true
	instance := startTestEngine(t, config, nil, false)
	result := instance.Router().PreMatch(flow("203.0.113.80:443", "udp"), nil)
	if result.Action == adapter.PreMatchDrop || result.Action == adapter.PreMatchReject || result.Action == adapter.PreMatchBypass {
		t.Fatalf("explicit UDP opt-in still blocks or bypasses the ISP: %+v", result)
	}
	// Public targets may need the connection-routing slow path because a
	// SOCKS outbound is not a raw-IP tun.Port.
	isp, ok := instance.Outbound().Outbound("isp")
	if !ok || isp.Type() != ispOutboundType {
		t.Fatal("opt-in replaced the required ISP transport")
	}
}

func TestDNSRejectsIncorrectTLSIdentity(t *testing.T) {
	var served atomic.Int32
	doh := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served.Add(1)
	}))
	doh.Config.ErrorLog = log.New(io.Discard, "", 0)
	doh.StartTLS()
	t.Cleanup(doh.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: doh.Certificate().Raw})
	address := doh.Listener.Addr().String()
	proxy := startSOCKS(t, map[string]string{address: address}, "", M.Socksaddr{})
	config := testConfig(t, "isp-sf")
	config.Proxy.Server = proxy.address
	host, port, _ := net.SplitHostPort(address)
	config.DNS.Server, config.DNS.Port = host, mustPort(t, port)
	config.DNS.ServerName = "incorrect-identity.tailrelay.invalid"
	instance := startTestEngine(t, config, certificate, false)
	answer, err := udpQuery(instance, "verify-tls.tailrelay.invalid", 500*time.Millisecond)
	if err == nil && answer.Rcode == dns.RcodeSuccess {
		t.Fatal("DNS succeeded with an incorrect TLS server identity")
	}
	if served.Load() != 0 {
		t.Fatal("DNS query was sent before verifying the TLS certificate identity")
	}
	select {
	case <-proxy.requests:
	default:
		t.Fatal("TLS verification test never reached the ISP proxy")
	}
}
