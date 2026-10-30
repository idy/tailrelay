package tailrelay

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	singlog "github.com/sagernet/sing-box/log"
)

func testConfig(t *testing.T, name string) Config {
	t.Helper()
	base := t.TempDir()
	config, err := (Config{
		Version: 1, Name: name, StateDir: filepath.Join(base, "state"),
		Proxy: ProxyConfig{Server: "203.0.113.10:12324", CredentialsFile: "credentials.json"},
	}).Normalized(base)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestConfigStrictnessAndPaths(t *testing.T) {
	valid := "version: 1\nname: isp-sf\nproxy:\n  server: 203.0.113.10:12324\n  credentials_file: proxy.json\n"
	base := t.TempDir()
	config, err := DecodeConfig(strings.NewReader(valid), base)
	if err != nil {
		t.Fatal(err)
	}
	if config.Tailscale.Hostname != "tailrelay-isp-sf" || config.Tailscale.ListenPort != 0 ||
		config.Proxy.UDP || config.Proxy.CredentialsFile != filepath.Join(base, "proxy.json") {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	for name, yaml := range map[string]string{
		"unknown":    valid + "password: pasted-secret\n",
		"duplicate":  valid + "name: isp-other\n",
		"documents":  valid + "---\n" + valid,
		"bad-proxy":  strings.ReplaceAll(valid, "203.0.113.10:12324", "proxy.example.com:12324"),
		"no-port":    strings.ReplaceAll(valid, ":12324", ":0"),
		"bad-name":   strings.ReplaceAll(valid, "isp-sf", "../isp"),
		"expansion":  valid + "state_dir: /tmp/$HOME\n",
		"too-large":  valid + strings.Repeat("#", maxConfigSize),
		"inline-key": valid + "tailscale:\n  auth_key: pasted-secret\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeConfig(strings.NewReader(yaml), base)
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if strings.Contains(err.Error(), "pasted-secret") {
				t.Fatal("parse error leaked a pasted secret")
			}
		})
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.DNS.Server = "resolver.example.com" },
		func(c *Config) { c.DNS.ServerName = "https://resolver.example.com" },
		func(c *Config) { c.DNS.Path = "https://elsewhere.example.com/dns-query" },
		func(c *Config) { c.DNS.Port = 65536 },
		func(c *Config) { c.Proxy.ConnectTimeout = "500ms" },
		func(c *Config) { c.Tailscale.ListenPort = -1 },
		func(c *Config) { c.Tailscale.Tags = []string{"member@example.com"} },
		func(c *Config) {
			c.Tailscale.AuthKeyEnv = "BOOTSTRAP"
			c.Tailscale.AuthKeyFile = "/etc/key"
		},
	} {
		bad := config
		mutate(&bad)
		if _, err := bad.Normalized(base); err == nil {
			t.Fatalf("invalid configuration was accepted: %+v", bad)
		}
	}
}

func TestCompileIndependentExitNodesAndOfflineCheck(t *testing.T) {
	first, second := testConfig(t, "isp-sf"), testConfig(t, "isp-seattle")
	first.Tailscale.AuthKeyEnv = "TAILRELAY_TEST_BOOTSTRAP"
	t.Setenv(first.Tailscale.AuthKeyEnv, "secret-must-not-appear")
	first.Proxy.CredentialsFile = filepath.Join(t.TempDir(), "does-not-exist")
	for _, config := range []Config{first, second} {
		compiled, err := Compile(config)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(compiled.JSON), "secret-must-not-appear") ||
			strings.Contains(string(compiled.JSON), "auth_key") {
			t.Fatal("render read or included an authentication key")
		}
		var native object
		if err := json.Unmarshal(compiled.JSON, &native); err != nil {
			t.Fatal(err)
		}
		ep := native["endpoints"].([]any)[0].(map[string]any)
		if ep["advertise_exit_node"] != true || ep["system_interface"] != false ||
			ep["hostname"] != config.Tailscale.Hostname ||
			ep["state_directory"] != filepath.Join(config.StateDir, "tailscale") {
			t.Fatalf("bad exit endpoint: %+v", ep)
		}
		if _, exists := native["inbounds"]; exists {
			t.Fatal("relay must not create a public proxy listener")
		}
		if len(native["outbounds"].([]any)) != 1 || native["route"].(map[string]any)["final"] != "isp" {
			t.Fatal("relay must have exactly one required ISP outbound")
		}
		cache := native["experimental"].(map[string]any)["cache_file"].(map[string]any)
		if cache["path"] != filepath.Join(config.StateDir, "cache.db") {
			t.Fatal("embedded cache can escape the instance state directory")
		}
		if TailscaleEnabled {
			if err := Check(config); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(config.StateDir); !os.IsNotExist(err) {
			t.Fatal("offline check/render created persistent state")
		}
	}
	if first.StateDir == second.StateDir || first.Tailscale.Hostname == second.Tailscale.Hostname {
		t.Fatal("instances share identity")
	}
}

func TestPrivateCredentialsAndStateOwnership(t *testing.T) {
	config := testConfig(t, "isp-sf")
	if err := os.WriteFile(config.Proxy.CredentialsFile, []byte(`{"username":"fixture-user","password":"fixture-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(config.Proxy.CredentialsFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.Proxy.CredentialsFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(config.Proxy.CredentialsFile); err == nil {
		t.Fatal("world-readable credentials accepted")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", filepath.Dir(config.Proxy.CredentialsFile))
	if _, err := LoadCredentials(config.Proxy.CredentialsFile); err == nil {
		t.Fatal("spoofing CREDENTIALS_DIRECTORY accepted an unsafe ordinary file")
	}
	if err := os.Chmod(config.Proxy.CredentialsFile, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(config.Proxy.CredentialsFile), "link")
	if err := os.Symlink(config.Proxy.CredentialsFile, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(link); err == nil {
		t.Fatal("credential symlink accepted")
	}
	for _, bad := range []string{
		`{"username":"a","password":"b","password":"c"}`,
		`{"username":"a","password":"b","extra":"c"}`,
		`{"username":"a","password":""}`,
		`{"username":"a","password":"b"} {}`,
	} {
		if err := os.WriteFile(config.Proxy.CredentialsFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCredentials(config.Proxy.CredentialsFile); err == nil {
			t.Fatal("malformed credentials accepted")
		}
	}
	lease, err := lockState(config)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if duplicate, err := lockState(config); err == nil {
		duplicate.Close()
		t.Fatal("duplicate instance acquired the same state")
	}
	other := config
	other.Name = "isp-other"
	other.StateDir = filepath.Join(t.TempDir(), "independent")
	otherLease, err := lockState(other)
	if err != nil {
		t.Fatal(err)
	}
	otherLease.Close()
	lease.Close()
	other.StateDir = config.StateDir
	if reused, err := lockState(other); err == nil {
		reused.Close()
		t.Fatal("another instance reused a retained device identity")
	}
	if reacquired, err := lockState(config); err != nil {
		t.Fatal(err)
	} else {
		reacquired.Close()
	}
}

func TestStateRejectsSymlinksAndUnsafeModes(t *testing.T) {
	for _, child := range []string{"", "tailscale", "runner.lock", "instance.json"} {
		t.Run("symlink-"+child, func(t *testing.T) {
			config := testConfig(t, "isp-sf")
			if child != "" {
				if err := os.Mkdir(config.StateDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			target := t.TempDir()
			if child == "runner.lock" || child == "instance.json" {
				target = filepath.Join(target, "file")
				if err := os.WriteFile(target, []byte(`{"name":"isp-sf"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(config.StateDir, child)); err != nil {
				t.Fatal(err)
			}
			if lease, err := lockState(config); err == nil {
				lease.Close()
				t.Fatal("state symlink accepted")
			}
		})
	}
	config := testConfig(t, "isp-sf")
	if err := os.Mkdir(config.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if lease, err := lockState(config); err == nil {
		lease.Close()
		t.Fatal("world-readable state directory accepted")
	}
}

func TestAuthAndLogsDoNotPersistSecrets(t *testing.T) {
	t.Setenv("TAILRELAY_TEST_KEY", "")
	if key, err := loadAuthKey(TailscaleConfig{AuthKeyEnv: "TAILRELAY_TEST_KEY"}); err != nil || key != "" {
		t.Fatal("unset bootstrap key should allow retained login state")
	}
	t.Setenv("TAILRELAY_TEST_KEY", "tskey-api-not-a-device-key")
	if _, err := loadAuthKey(TailscaleConfig{AuthKeyEnv: "TAILRELAY_TEST_KEY"}); err == nil {
		t.Fatal("API token accepted as a device auth key")
	}
	var buffer bytes.Buffer
	writer := newSafeLog(&buffer, "debug", "fixture-user", "fixture-password")
	writer.WriteMessage(singlog.LevelDebug, `fixture-user fixture-password tskey-auth-test https://login.tailscale.com/a/private-session`)
	for _, secret := range []string{"fixture-user", "fixture-password", "tskey-auth-test", "private-session"} {
		if strings.Contains(buffer.String(), secret) {
			t.Fatal("log leaked private authentication material")
		}
	}
	writer.WriteMessage(singlog.LevelTrace, "trace-must-stay-filtered")
	if strings.Contains(buffer.String(), "trace-must-stay-filtered") {
		t.Fatal("platform logger ignored configured level")
	}
}

func TestLogRedactionHandlesCredentialsOverlappingURLAndKeySyntax(t *testing.T) {
	for _, secret := range []string{"a", "https", "tailscale", "tskey", "auth", "login"} {
		t.Run(secret, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := newSafeLog(&buffer, "debug", secret)
			writer.WriteMessage(singlog.LevelError,
				`https://login.tailscale.com/a/private-session-token tskey-auth-private-token`)
			for _, private := range []string{"private-session-token", "private-token", "https://"} {
				if strings.Contains(buffer.String(), private) {
					t.Fatalf("overlapping credential damaged complete secret redaction: %s", buffer.String())
				}
			}
		})
	}
}

func TestLogRedactionCoversOverlappingOriginalCredentialMatches(t *testing.T) {
	for _, test := range []struct {
		name, username, password, message, want string
	}{
		{"prefix", "a", "a-secret", "a a-secret", "<redacted> <redacted>\n"},
		{"inside", "secret", "prefix-secret-suffix", "prefix-secret-suffix", "<redacted>\n"},
		{"overlapping-ranges", "a-secret", "secret-secret", "a-secret-secret", "<redacted>\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := newSafeLog(&buffer, "debug", test.username, test.password)
			writer.WriteMessage(singlog.LevelError, test.message)
			if buffer.String() != test.want {
				t.Fatal("credential fragments remain after log redaction")
			}
		})
	}
}
