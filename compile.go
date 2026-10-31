package tailrelay

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
)

type object = map[string]any

type Compiled struct {
	JSON []byte
}

// Compile produces a secret-free description of the actual embedded core.
// This is a TailRelay config (with a restricted custom SOCKS5 outbound), not a
// config for an arbitrary standalone sing-box binary.
func Compile(config Config) (Compiled, error) {
	return compile(config, false)
}

func compile(config Config, loginOnly bool) (Compiled, error) {
	base, err := os.Getwd()
	if err != nil {
		return Compiled{}, err
	}
	config, err = config.Normalized(base)
	if err != nil {
		return Compiled{}, err
	}
	proxy := netip.MustParseAddrPort(config.Proxy.Server)
	allowedInbounds := []string{"tailnet"}
	if config.LocalTUN != nil && !loginOnly {
		allowedInbounds = append(allowedInbounds, "local-tun")
	}
	rules := []object{
		{"inbound": allowedInbounds, "invert": true, "action": "reject"},
		{"network": "icmp", "action": "reject", "method": "drop"},
		{"network": []string{"tcp", "udp"}, "port": 53, "action": "hijack-dns"},
		{"ip_is_private": true, "action": "reject"},
		{"ip_cidr": []string{
			"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16",
			"192.0.0.0/24", "198.18.0.0/15", "224.0.0.0/3",
			"::/96", "::ffff:0:0/96", "64:ff9b::/96",
			"100::/64", "fc00::/7", "fe80::/10", "ff00::/8",
		}, "action": "reject"},
	}
	if !config.Proxy.UDP {
		rules = append(rules, object{"network": "udp", "action": "reject"})
	}
	dnsRules := []object{{"inbound": allowedInbounds, "action": "route", "server": "exit"}}
	if loginOnly {
		// A login command cannot serve client traffic or peer DNS, even if
		// a previous run's routes remain approved in the control plane.
		rules = []object{{"action": "reject", "method": "drop"}}
		dnsRules = []object{{"inbound": "tailnet", "action": "reject"}}
	}
	native := object{
		"log": object{"level": config.LogLevel, "timestamp": true, "output": os.DevNull},
		"endpoints": []object{{
			"type": "tailscale", "tag": "tailnet",
			"hostname":           config.Tailscale.Hostname,
			"state_directory":    filepath.Join(config.StateDir, "tailscale"),
			"taildrop_directory": filepath.Join(config.StateDir, "taildrop"),
			"system_interface":   false, "accept_routes": false, "ephemeral": false,
			"advertise_exit_node": !loginOnly,
			"advertise_tags":      config.Tailscale.Tags,
			"listen_port":         config.Tailscale.ListenPort,
			"domain_resolver":     "control",
		}},
		"outbounds": []object{{
			"type": ispOutboundType, "tag": "isp",
			"server": proxy.Addr().String(), "server_port": proxy.Port(),
			"username": "<runtime>", "password": "<runtime>",
			"udp": config.Proxy.UDP, "connect_timeout": config.Proxy.ConnectTimeout,
		}},
		"dns": object{
			"servers": []object{
				{"type": "local", "tag": "control"},
				{
					"type": "https", "tag": "exit", "server": config.DNS.Server,
					"server_port": config.DNS.Port, "path": config.DNS.Path,
					"tls":    object{"enabled": true, "server_name": config.DNS.ServerName},
					"detour": "isp",
				},
			},
			"rules": dnsRules, "final": "exit", "independent_cache": true,
		},
		"route": object{
			"rules": rules, "final": "isp",
			// Only control-plane hostnames use the host's resolver. Forwarded
			// connections have literal destinations and never use this default.
			"default_domain_resolver": "control",
		},
		"experimental": object{"cache_file": object{
			"enabled": true, "path": filepath.Join(config.StateDir, "cache.db"),
		}},
	}
	if config.LocalTUN != nil && !loginOnly {
		native["inbounds"] = []object{{
			"type": "tun", "tag": "local-tun",
			"interface_name": config.LocalTUN.InterfaceName,
			"netns":          config.LocalTUN.Namespace,
			"address":        []string{"172.29.0.1/30", "fdfe:29::1/126"},
			// The namespace's resolv.conf is deployment-owned. Do not change
			// host DNS or depend on systemd-resolved.
			"dns_mode": "disabled", "stack": "gvisor", "mtu": 1500,
			"auto_route": true, "strict_route": true,
			"route_exclude_address": []string{
				config.LocalTUN.ManagementSubnet, "127.0.0.0/8", "::1/128",
			},
		}}
	}
	wire, err := json.MarshalIndent(native, "", "  ")
	return Compiled{JSON: wire}, err
}

func injectSecrets(compiled Compiled, credentials Credentials, authKey string) ([]byte, error) {
	var native object
	if err := json.Unmarshal(compiled.JSON, &native); err != nil {
		return nil, err
	}
	proxy := native["outbounds"].([]any)[0].(map[string]any)
	proxy["username"], proxy["password"] = credentials.Username, credentials.Password
	if authKey != "" {
		native["endpoints"].([]any)[0].(map[string]any)["auth_key"] = authKey
	}
	return json.Marshal(native)
}
