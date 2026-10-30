package tailrelay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxConfigSize = 64 << 10

var (
	deviceLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tagName     = regexp.MustCompile(`^tag:[a-z][a-z0-9-]*$`)
)

// Config contains no login key or proxy password. Relative file paths are
// resolved against the YAML file, independently of the process working directory.
type Config struct {
	Version   int             `yaml:"version"`
	Name      string          `yaml:"name"`
	StateDir  string          `yaml:"state_dir"`
	LogLevel  string          `yaml:"log_level"`
	Tailscale TailscaleConfig `yaml:"tailscale"`
	Proxy     ProxyConfig     `yaml:"proxy"`
	DNS       DNSConfig       `yaml:"dns"`
}

type TailscaleConfig struct {
	Hostname        string   `yaml:"hostname"`
	AuthKeyEnv      string   `yaml:"auth_key_env"`
	AuthKeyFile     string   `yaml:"auth_key_file"`
	Tags            []string `yaml:"tags"`
	ListenPort      int      `yaml:"listen_port"`
	ExpectedTailnet string   `yaml:"expected_tailnet"`
}

type ProxyConfig struct {
	Server          string `yaml:"server"`
	CredentialsFile string `yaml:"credentials_file"`
	UDP             bool   `yaml:"udp"`
	ConnectTimeout  string `yaml:"connect_timeout"`
}

type DNSConfig struct {
	Server     string `yaml:"server"`
	Port       int    `yaml:"port"`
	ServerName string `yaml:"server_name"`
	Path       string `yaml:"path"`
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration file")
	}
	defer file.Close()
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	return DecodeConfig(file, base)
}

func DecodeConfig(reader io.Reader, base string) (Config, error) {
	wire, err := io.ReadAll(io.LimitReader(reader, maxConfigSize+1))
	if err != nil || len(wire) > maxConfigSize {
		return Config{}, errors.New("cannot read configuration or configuration exceeds 64 KiB")
	}
	var config Config
	decoder := yaml.NewDecoder(strings.NewReader(string(wire)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		// Parser errors can contain an accidentally pasted secret.
		return Config{}, errors.New("invalid YAML: use known fields and unique keys")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	return config.Normalized(base)
}

func (c Config) Normalized(base string) (Config, error) {
	if c.Version != 1 {
		return c, errors.New("version must be 1")
	}
	if !deviceLabel.MatchString(c.Name) {
		return c, errors.New("name must be a lowercase DNS label, at most 63 characters")
	}
	if c.Tailscale.Hostname == "" {
		c.Tailscale.Hostname = "tailrelay-" + c.Name
	}
	if !deviceLabel.MatchString(c.Tailscale.Hostname) {
		return c, errors.New("tailscale.hostname must be a lowercase DNS label, at most 63 characters")
	}
	if c.Tailscale.ListenPort < 0 || c.Tailscale.ListenPort > 65535 {
		return c, errors.New("tailscale.listen_port must be 0 (automatic) or 1..65535")
	}
	if c.Tailscale.AuthKeyEnv != "" && !envName.MatchString(c.Tailscale.AuthKeyEnv) {
		return c, errors.New("tailscale.auth_key_env must be an environment variable name")
	}
	if c.Tailscale.AuthKeyFile != "" && c.Tailscale.AuthKeyEnv != "" {
		return c, errors.New("choose either tailscale.auth_key_file or tailscale.auth_key_env")
	}
	for _, tag := range c.Tailscale.Tags {
		if !tagName.MatchString(tag) {
			return c, errors.New("tailscale.tags must contain tag:name entries")
		}
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join("/var/lib/tailrelay", c.Name)
	}
	for _, path := range []*string{&c.StateDir, &c.Proxy.CredentialsFile, &c.Tailscale.AuthKeyFile} {
		if *path == "" {
			continue
		}
		// sing-box expands environment variables in its own state paths.
		// Refuse them here so the checked path and runtime path cannot differ.
		if strings.ContainsAny(*path, "$\x00\r\n") {
			return c, errors.New("file paths cannot contain environment expansions or control characters")
		}
		if !filepath.IsAbs(*path) {
			*path = filepath.Join(base, *path)
		}
		*path = filepath.Clean(*path)
	}
	if !filepath.IsAbs(c.StateDir) || c.StateDir == "/" {
		return c, errors.New("state_dir must resolve to an absolute directory other than /")
	}
	if c.Proxy.CredentialsFile == "" {
		return c, errors.New("proxy.credentials_file is required")
	}
	proxy, err := netip.ParseAddrPort(c.Proxy.Server)
	if err != nil || proxy.Port() == 0 || proxy.Addr().Zone() != "" ||
		proxy.Addr().IsUnspecified() || proxy.Addr().IsMulticast() {
		return c, errors.New("proxy.server must be a literal IPv4:port or [IPv6]:port")
	}
	c.Proxy.Server = netip.AddrPortFrom(proxy.Addr().Unmap(), proxy.Port()).String()
	if c.Proxy.ConnectTimeout == "" {
		c.Proxy.ConnectTimeout = "10s"
	}
	timeout, err := time.ParseDuration(c.Proxy.ConnectTimeout)
	if err != nil || timeout < time.Second || timeout > time.Minute {
		return c, errors.New("proxy.connect_timeout must be between 1s and 1m")
	}
	if c.DNS.Server == "" {
		c.DNS.Server = "1.1.1.1"
	}
	address, err := netip.ParseAddr(c.DNS.Server)
	if err != nil || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() {
		return c, errors.New("dns.server must be a literal IP address")
	}
	c.DNS.Server = address.Unmap().String()
	if c.DNS.Port == 0 {
		c.DNS.Port = 443
	}
	if c.DNS.Port < 1 || c.DNS.Port > 65535 {
		return c, errors.New("dns.port must be 1..65535")
	}
	if c.DNS.ServerName == "" {
		c.DNS.ServerName = "cloudflare-dns.com"
	}
	if !validDNSName(c.DNS.ServerName) {
		return c, errors.New("dns.server_name must be a DNS name for TLS certificate verification")
	}
	if c.DNS.Path == "" {
		c.DNS.Path = "/dns-query"
	}
	path, err := url.Parse(c.DNS.Path)
	if err != nil || path.IsAbs() || path.Host != "" || path.Fragment != "" ||
		!strings.HasPrefix(c.DNS.Path, "/") || strings.ContainsAny(c.DNS.Path, "\r\n") {
		return c, errors.New("dns.path must be an absolute HTTP path without a fragment")
	}
	if c.LogLevel == "" {
		c.LogLevel = "warn"
	}
	if !slices.Contains([]string{"error", "warn", "info", "debug"}, c.LogLevel) {
		return c, errors.New("log_level must be error, warn, info or debug")
	}
	return c, nil
}

func validDNSName(name string) bool {
	if len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !deviceLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func (c Config) String() string {
	return fmt.Sprintf("%s (%s -> %s)", c.Name, c.Tailscale.Hostname, c.Proxy.Server)
}
