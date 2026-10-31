package tailrelay

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func localTUN() *LocalTUNConfig {
	return &LocalTUNConfig{
		Namespace: "letsolo-claude-7", ManagementSubnet: "169.254.201.0/30",
	}
}

func TestLocalTUNCompilationAndValidation(t *testing.T) {
	config := testConfig(t, "claude-7")
	config.LocalTUN = localTUN()
	compiled, err := Compile(config)
	if err != nil {
		t.Fatal(err)
	}
	var native object
	if err := json.Unmarshal(compiled.JSON, &native); err != nil {
		t.Fatal(err)
	}
	tun := native["inbounds"].([]any)[0].(map[string]any)
	if tun["netns"] != "letsolo-claude-7" || tun["interface_name"] != "tr-tun" ||
		tun["auto_route"] != true || tun["dns_mode"] != "disabled" || tun["stack"] != "gvisor" {
		t.Fatalf("unexpected local TUN: %+v", tun)
	}
	endpoint := native["endpoints"].([]any)[0].(map[string]any)
	if endpoint["system_interface"] != false || endpoint["state_directory"] != config.StateDir+"/tailscale" {
		t.Fatal("local TUN changed the retained userspace Tailscale identity")
	}
	login, err := compile(config, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(login.JSON), `"type": "tun"`) {
		t.Fatal("interactive login must not open a local traffic interface")
	}
	for _, mutate := range []func(*LocalTUNConfig){
		func(c *LocalTUNConfig) { c.Namespace = "/proc/1/ns/net" },
		func(c *LocalTUNConfig) { c.Namespace = "../other" },
		func(c *LocalTUNConfig) { c.InterfaceName = "lo" },
		func(c *LocalTUNConfig) { c.InterfaceName = "interface-too-long" },
		func(c *LocalTUNConfig) { c.ManagementSubnet = "0.0.0.0/0" },
		func(c *LocalTUNConfig) { c.ManagementSubnet = "169.254.201.1/30" },
	} {
		bad := config
		bad.LocalTUN = localTUN()
		mutate(bad.LocalTUN)
		if _, err := bad.Normalized(t.TempDir()); err == nil {
			t.Fatal("unsafe namespace configuration was accepted")
		}
	}
}

func TestLocalTUNTrafficSharesTheRestrictedISPAndAuthorizationGate(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("local-tun-through-isp"))
	}))
	defer peer.Close()
	destination := "203.0.113.80:80"
	proxy := startSOCKS(t, map[string]string{destination: peer.Listener.Addr().String()}, "", M.Socksaddr{})
	config := testConfig(t, "claude-7")
	config.Proxy.Server = proxy.address
	config.LocalTUN = localTUN()
	gate := new(forwardingGate)
	gate.setAllowed(true)
	instance := startTestEngineWithGate(t, config, nil, false, gate)
	request := func() string {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		_ = client.SetDeadline(time.Now().Add(3 * time.Second))
		local := flow(destination, "tcp")
		local.Inbound, local.InboundType = "local-tun", "tun"
		go instance.Router().RouteConnectionEx(t.Context(), server, local, nil)
		_, _ = client.Write([]byte("GET / HTTP/1.0\r\nHost: fixture.test\r\n\r\n"))
		wire, _ := io.ReadAll(client)
		return string(wire)
	}
	if !strings.Contains(request(), "local-tun-through-isp") {
		t.Fatal("local TUN traffic did not reach its assigned ISP")
	}
	<-proxy.requests
	gate.setAllowed(false)
	if strings.Contains(request(), "local-tun-through-isp") {
		t.Fatal("local TUN forwarded after Tailnet authorization was revoked")
	}
	select {
	case <-proxy.requests:
		t.Fatal("blocked local traffic opened another ISP socket")
	default:
	}
}

func TestLocalTUNStillRejectsPrivateUDPAndForeignTraffic(t *testing.T) {
	config := testConfig(t, "claude-7")
	config.LocalTUN = localTUN()
	instance := startTestEngine(t, config, nil, false)
	for _, network := range []string{"udp", "icmp"} {
		local := flow("203.0.113.80:443", network)
		local.Inbound, local.InboundType = "local-tun", "tun"
		action := instance.Router().PreMatch(local, nil).Action
		if action != adapter.PreMatchReject && action != adapter.PreMatchDrop {
			t.Fatal("TUN bypassed the restricted protocol policy")
		}
	}
	dns := flow("172.29.0.2:53", "udp")
	dns.Inbound, dns.InboundType = "local-tun", "tun"
	if instance.Router().PreMatch(dns, nil).Action != adapter.PreMatchHijackDNS {
		t.Fatal("local DNS did not use the existing ISP DoH path")
	}
	private := flow("169.254.169.254:80", "tcp")
	private.Inbound, private.InboundType = "local-tun", "tun"
	if instance.Router().PreMatch(private, nil).Action != adapter.PreMatchReject {
		t.Fatal("TUN permitted cloud metadata access")
	}
	foreign := flow("203.0.113.80:80", "tcp")
	foreign.Inbound = "unexpected"
	if instance.Router().PreMatch(foreign, nil).Action != adapter.PreMatchReject {
		t.Fatal("enabling local TUN permitted unknown inbounds")
	}
}
