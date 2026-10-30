//go:build with_gvisor && tailrelay_live

// Explicit live acceptance client. It never changes the host's routes or DNS.
// Auth material is supplied on stdin; output contains only public test results.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/tailscale"
	singJSON "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/tailscale/envknob"
)

type request struct {
	AuthKey     string `json:"auth_key"`
	StateDir    string `json:"state_dir"`
	ExitNode    string `json:"exit_node"`
	ExpectedIP  string `json:"expected_ip"`
	WantBlocked bool   `json:"want_blocked"`
}

func main() {
	result, err := run()
	if err != nil {
		if result == nil {
			result = make(map[string]any)
		}
		result["ok"] = false
		result["error"] = err.Error()
		_ = json.NewEncoder(os.Stdout).Encode(result)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

func run() (map[string]any, error) {
	var input request
	if json.NewDecoder(io.LimitReader(os.Stdin, 16384)).Decode(&input) != nil {
		return nil, errors.New("invalid private acceptance input")
	}
	exit, err := netip.ParseAddr(input.ExitNode)
	if err != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(exit) {
		return nil, errors.New("expected a Tailnet Exit Node address")
	}
	if _, err := netip.ParseAddr(input.ExpectedIP); err != nil {
		return nil, errors.New("expected a literal ISP public address")
	}
	input.StateDir, err = prepareAcceptanceState("/var/lib/tailrelay-acceptance", input.StateDir)
	if err != nil {
		return nil, err
	}
	envknob.Setenv("TS_FORCE_NOISE_443", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	endpoints := endpoint.NewRegistry()
	tailscale.RegisterEndpoint(endpoints)
	transports := dns.NewTransportRegistry()
	local.RegisterTransport(transports)
	ctx = box.Context(ctx, inbound.NewRegistry(), outbound.NewRegistry(), endpoints, transports,
		service.NewRegistry(), certificate.NewRegistry())
	native := map[string]any{
		"log": map[string]any{"disabled": true},
		"dns": map[string]any{"servers": []any{map[string]any{"type": "local", "tag": "control"}}, "final": "control"},
		"endpoints": []any{map[string]any{
			"type": "tailscale", "tag": "tailnet", "hostname": "tailrelay-check-bwg",
			"state_directory": input.StateDir, "taildrop_directory": filepath.Join(input.StateDir, "taildrop"),
			"auth_key": input.AuthKey, "system_interface": false, "accept_routes": false,
			"exit_node": input.ExitNode, "domain_resolver": "control",
		}},
		"route": map[string]any{"final": "tailnet", "default_domain_resolver": "control"},
		"experimental": map[string]any{"cache_file": map[string]any{
			"enabled": true, "path": filepath.Join(input.StateDir, "cache.db"),
		}},
	}
	wire, _ := json.Marshal(native)
	var options option.Options
	if singJSON.UnmarshalContext(ctx, wire, &options) != nil {
		return nil, errors.New("cannot parse live acceptance configuration")
	}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		return nil, errors.New("cannot initialize live acceptance client")
	}
	defer instance.Close()
	if err := instance.Start(); err != nil {
		return nil, errors.New("cannot start live acceptance client")
	}
	ep, ok := instance.Endpoint().Get("tailnet")
	if !ok {
		return nil, errors.New("acceptance endpoint is missing")
	}
	tailnet := ep.(adapter.TailscaleEndpoint)
	updates := make(chan *adapter.TailscaleEndpointStatus, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tailnet.SubscribeTailscaleStatus(watchCtx, func(status *adapter.TailscaleEndpointStatus) {
			select {
			case updates <- status:
			case <-watchCtx.Done():
			}
		})
	}()
	defer func() { stopWatch(); <-done }()
	var selfIPs []string
	var selectedExit string
	for {
		select {
		case status := <-updates:
			if status.AuthURL != "" {
				return nil, errors.New("live acceptance device requires authentication")
			}
			if status.BackendState == "Running" && status.Self != nil && status.ExitNode != nil &&
				status.ExitNode.Online && !status.ExitNode.Expired &&
				slices.Contains(status.ExitNode.TailscaleIPs, input.ExitNode) {
				selfIPs = status.Self.TailscaleIPs
				selectedExit = status.ExitNode.HostName
				goto connected
			}
		case <-ctx.Done():
			return nil, errors.New("live acceptance Tailnet connection timed out")
		}
	}
connected:
	dialer := ep.(N.Dialer)
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, M.ParseSocksaddr(address))
			},
			TLSClientConfig:   &tls.Config{ServerName: "cloudflare-dns.com", MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
	}
	query, _ := http.NewRequestWithContext(ctx, "GET", "https://1.1.1.1/cdn-cgi/trace", nil)
	query.Host = "cloudflare-dns.com"
	response, httpErr := httpClient.Do(query)
	ip := ""
	if httpErr == nil {
		body, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			return nil, errors.New("unexpected live HTTP response")
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "ip=") {
				ip = strings.TrimPrefix(line, "ip=")
			}
		}
	}
	dnsOK := false
	message, err := newDNSProbe()
	if err != nil {
		return nil, err
	}
	udp, dnsErr := dialer.DialContext(ctx, "udp", M.ParseSocksaddr("8.8.8.8:53"))
	if dnsErr == nil {
		_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
		wire, _ := message.Pack()
		if _, err := udp.Write(wire); err == nil {
			answer := make([]byte, 4096)
			if count, err := udp.Read(answer); err == nil {
				var result mdns.Msg
				dnsOK = result.Unpack(answer[:count]) == nil && validDNSProbeReply(message, &result)
			} else {
				dnsErr = err
			}
		} else {
			dnsErr = err
		}
		udp.Close()
	}
	result := map[string]any{
		"exit_node": input.ExitNode, "expected_ip": input.ExpectedIP, "client_addresses": selfIPs,
		"http_succeeded": httpErr == nil, "observed_ip": ip, "dns_succeeded": dnsOK,
		"want_blocked":           input.WantBlocked,
		"selected_exit_hostname": selectedExit,
		"dns_query":              message.Question[0].Name,
	}
	if httpErr != nil {
		result["http_error"] = httpErr.Error()
	}
	if dnsErr != nil {
		result["dns_error"] = dnsErr.Error()
	}
	if input.WantBlocked {
		result["ok"] = httpErr != nil && !dnsOK
	} else {
		result["ok"] = httpErr == nil && ip == input.ExpectedIP && dnsOK
	}
	if result["ok"] != true {
		return result, fmt.Errorf("live acceptance failed (HTTP=%t, DNS=%t, observed IP=%s)", httpErr == nil, dnsOK, ip)
	}
	return result, nil
}
