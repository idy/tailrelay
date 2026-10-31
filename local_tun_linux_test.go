//go:build linux

package tailrelay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

func TestLocalTUNNetworkNamespace(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("TAILRELAY_PRIVILEGED_TEST") != "1" {
		t.Skip("explicit root namespace test; run with TAILRELAY_PRIVILEGED_TEST=1")
	}
	namespace := fmt.Sprintf("letsolo-tun-test-%d", os.Getpid())
	run := func(args ...string) []byte {
		t.Helper()
		output, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, output)
		}
		return output
	}
	hostRoutes := string(run("ip", "route", "show", "table", "main"))
	run("ip", "netns", "add", namespace)
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "del", namespace).Run()
		if after := string(run("ip", "route", "show", "table", "main")); after != hostRoutes {
			t.Error("namespace TUN changed the host's main routing table")
		}
	})
	run("ip", "-n", namespace, "link", "set", "lo", "up")
	for _, tool := range []string{"iptables", "ip6tables"} {
		run("ip", "netns", "exec", namespace, tool, "-P", "OUTPUT", "DROP")
		run("ip", "netns", "exec", namespace, tool, "-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT")
		run("ip", "netns", "exec", namespace, tool, "-A", "OUTPUT", "-o", "tr-tun", "-j", "ACCEPT")
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("kernel-tun-through-isp"))
	}))
	t.Cleanup(peer.Close)
	destination := "203.0.113.80:80"
	proxy := startSOCKS(t, map[string]string{destination: peer.Listener.Addr().String()}, "", M.Socksaddr{})
	config := testConfig(t, "kernel-tun")
	config.Proxy.Server = proxy.address
	config.LocalTUN = &LocalTUNConfig{Namespace: namespace, ManagementSubnet: "169.254.240.0/30"}
	gate := new(forwardingGate)
	gate.setAllowed(true)
	instance := startTestEngineMode(t, config, nil, false, gate, true)
	request := func() ([]byte, error) {
		command := exec.Command("ip", "netns", "exec", namespace, "python3", "-c",
			`import urllib.request; print(urllib.request.build_opener(urllib.request.ProxyHandler({})).open("http://203.0.113.80/", timeout=3).read().decode())`)
		return command.CombinedOutput()
	}
	var body []byte
	var err error
	for deadline := time.Now().Add(8 * time.Second); ; {
		body, err = request()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(body), "kernel-tun-through-isp") {
		t.Fatalf("proxy-free namespace request failed: %v: %s", err, body)
	}
	gate.setAllowed(false)
	if _, err := request(); err == nil {
		t.Fatal("namespace traffic survived authorization revocation")
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := request(); err == nil {
		t.Fatal("namespace traffic escaped after TUN shutdown")
	}
	if output := string(run("ip", "netns", "exec", namespace, "iptables", "-S", "OUTPUT")); !strings.Contains(output, "-P OUTPUT DROP") {
		t.Fatal("TUN shutdown removed the independent firewall policy")
	}
}
