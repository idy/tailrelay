package tailrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	singlog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"
)

const CoreVersion = "1.14.4"

func engineContext(ctx context.Context) context.Context {
	out := outbound.NewRegistry()
	registerISP(out)
	endpoints := endpoint.NewRegistry()
	registerTailscale(endpoints)
	transports := dns.NewTransportRegistry()
	local.RegisterTransport(transports)
	transport.RegisterHTTPS(transports)
	return box.Context(ctx, inbound.NewRegistry(), out, endpoints, transports, service.NewRegistry(), certificate.NewRegistry())
}

func parseNative(ctx context.Context, wire []byte) (option.Options, error) {
	var options option.Options
	if err := singJSON.UnmarshalContext(ctx, wire, &options); err != nil {
		return options, errors.New("cannot parse embedded core configuration")
	}
	return options, nil
}

// Check initializes but does not start the core. It never reads secrets,
// creates state, opens a listener, logs in, or changes host networking.
func Check(config Config) error {
	if !TailscaleEnabled {
		return errors.New("build tailrelay with -tags with_gvisor")
	}
	compiled, err := Compile(config)
	if err != nil {
		return err
	}
	ctx := engineContext(context.Background())
	options, err := parseNative(ctx, compiled.JSON)
	if err != nil {
		return err
	}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		return fmt.Errorf("initialize embedded core: %w", err)
	}
	return instance.Close()
}

// Status is safe to persist. It deliberately excludes login URLs, device
// private keys, proxy credentials, account lists and authentication tokens.
type Status struct {
	Name              string    `json:"name"`
	Hostname          string    `json:"hostname"`
	PID               int       `json:"pid"`
	Phase             string    `json:"phase"`
	BackendState      string    `json:"backend_state,omitempty"`
	Tailnet           string    `json:"tailnet,omitempty"`
	Addresses         []string  `json:"addresses,omitempty"`
	AuthRequired      bool      `json:"auth_required"`
	TailnetConnected  bool      `json:"tailnet_connected"`
	AdvertiseExitNode bool      `json:"advertise_exit_node"`
	Proxy             string    `json:"proxy"`
	UDP               bool      `json:"udp"`
	StartedAt         time.Time `json:"started_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type RunOptions struct {
	LoginOnly bool
	Log       io.Writer
	// LoginURL is called only by an explicitly requested interactive login.
	// It must not write the URL to persistent logs or a status file.
	LoginURL func(string)
}

func Run(ctx context.Context, config Config, run RunOptions) (runErr error) {
	if !TailscaleEnabled {
		return errors.New("build tailrelay with -tags with_gvisor")
	}
	base, err := os.Getwd()
	if err != nil {
		return err
	}
	config, err = config.Normalized(base)
	if err != nil {
		return err
	}
	credentials := Credentials{Username: "<login-only>", Password: "<login-only>"}
	if !run.LoginOnly {
		credentials, err = LoadCredentials(config.Proxy.CredentialsFile)
		if err != nil {
			return err
		}
	}
	authKey, err := loadAuthKey(config.Tailscale)
	if err != nil {
		return err
	}
	compiled, err := compile(config, run.LoginOnly)
	if err != nil {
		return err
	}
	wire, err := injectSecrets(compiled, credentials, authKey)
	if err != nil {
		return errors.New("cannot prepare runtime configuration")
	}
	lease, err := lockState(config)
	if err != nil {
		return err
	}
	defer lease.Close()
	status := Status{
		Name: config.Name, Hostname: config.Tailscale.Hostname, PID: os.Getpid(),
		Phase: "starting", AdvertiseExitNode: !run.LoginOnly,
		Proxy: config.Proxy.Server, UDP: config.Proxy.UDP, StartedAt: time.Now().UTC(),
	}
	save := func() error {
		status.UpdatedAt = time.Now().UTC()
		wire, _ := json.MarshalIndent(status, "", "  ")
		if err := atomicWrite(filepath.Join(config.StateDir, "status.json"), wire); err != nil {
			return errors.New("cannot write private status file")
		}
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	defer func() {
		status.Phase, status.TailnetConnected, status.AdvertiseExitNode = "stopped", false, false
		if runErr != nil {
			status.Phase = "failed"
		}
		runErr = errors.Join(runErr, save())
	}()
	engineCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	gate := new(forwardingGate)
	defer gate.setAllowed(false)
	engineCtx = context.WithValue(engineCtx, forwardingContextKey{}, gate)
	engineCtx = engineContext(engineCtx)
	options, err := parseNative(engineCtx, wire)
	if err != nil {
		return err
	}
	writer := newSafeLog(run.Log, config.LogLevel, credentials.Username, credentials.Password, authKey)
	instance, err := box.New(box.Options{Context: engineCtx, Options: options, PlatformLogWriter: writer})
	if err != nil {
		return errors.New("cannot initialize embedded core")
	}
	defer func() {
		gate.setAllowed(false)
		cancel()
		if err := instance.Close(); err != nil {
			runErr = errors.Join(runErr, errors.New("cannot close embedded core cleanly"))
		}
	}()
	if err := instance.Start(); err != nil {
		writer.WriteMessage(singlog.LevelError, "embedded core startup: "+err.Error())
		return errors.New("cannot start embedded core; check logs and Tailscale connectivity")
	}
	ep, exists := instance.Endpoint().Get("tailnet")
	if !exists {
		return errors.New("embedded Tailscale endpoint is missing")
	}
	tailnet, ok := ep.(adapter.TailscaleEndpoint)
	if !ok {
		return errors.New("embedded endpoint does not support Tailscale status")
	}
	events := make(chan *adapter.TailscaleEndpointStatus, 1)
	watchCtx, stopWatch := context.WithCancel(engineCtx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		_ = tailnet.SubscribeTailscaleStatus(watchCtx, func(current *adapter.TailscaleEndpointStatus) {
			select {
			case events <- current:
			case <-watchCtx.Done():
			}
		})
	}()
	defer func() {
		stopWatch()
		<-watchDone
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	enrollmentTimeout := 2 * time.Minute
	if run.LoginOnly {
		enrollmentTimeout = 10 * time.Minute
	}
	enrollmentTimer := time.NewTimer(enrollmentTimeout)
	defer enrollmentTimer.Stop()
	enrollmentDeadline := enrollmentTimer.C
	lastURL := ""
	for {
		select {
		case <-ctx.Done():
			if run.LoginOnly {
				return ctx.Err()
			}
			return nil
		case <-watchDone:
			if ctx.Err() != nil {
				if run.LoginOnly {
					return ctx.Err()
				}
				return nil
			}
			return errors.New("Tailscale status monitoring stopped")
		case <-enrollmentDeadline:
			return errors.New("Tailscale enrollment timed out; check network connectivity, device authentication and the selected Tailnet")
		case current := <-events:
			if err := gate.observe(config.Tailscale.ExpectedTailnet, run.LoginOnly, current); err != nil {
				return err
			}
			status.BackendState, status.Tailnet = current.BackendState, current.NetworkName
			status.AuthRequired = current.AuthURL != "" || current.BackendState == "NeedsLogin"
			status.TailnetConnected = current.BackendState == "Running" && current.Self != nil && !status.AuthRequired
			status.Phase = "waiting-for-tailnet"
			status.Addresses = nil
			if current.Self != nil {
				status.Addresses = append([]string(nil), current.Self.TailscaleIPs...)
				if current.Self.Expired {
					status.TailnetConnected = false
					status.AuthRequired = true
				}
			}
			if status.TailnetConnected {
				enrollmentTimer.Stop()
				enrollmentDeadline = nil
				// Connected says nothing about ISP health or route approval.
				status.Phase = "tailnet-connected"
			}
			if status.AuthRequired {
				status.Phase = "authentication-required"
			}
			if err := save(); err != nil {
				return err
			}
			if run.LoginOnly {
				if current.AuthURL != "" && current.AuthURL != lastURL && run.LoginURL != nil {
					run.LoginURL(current.AuthURL)
					lastURL = current.AuthURL
				}
				if status.TailnetConnected {
					return nil
				}
			} else if current.AuthURL != "" || (current.Self != nil && current.Self.Expired) {
				// NeedsLogin can be transient while tsnet is using a bootstrap
				// key. Wait for that registration instead of killing it early.
				return errors.New("Tailscale login required: stop this instance and use `tailrelay login` with the same state directory, or supply an auth key")
			}
		case <-ticker.C:
			if err := save(); err != nil {
				return err
			}
		}
	}
}

func ReadStatus(config Config) (Status, error) {
	wire, err := readPrivate(filepath.Join(config.StateDir, "status.json"), 16<<10)
	if err != nil {
		return Status{}, errors.New("no readable private status file; check the state directory and service user")
	}
	var status Status
	if json.Unmarshal(wire, &status) != nil || status.Name != config.Name {
		return Status{}, errors.New("invalid status file or status belongs to another instance")
	}
	return status, nil
}

var (
	authURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+/(?:a/|auth/|register|login)[^\s"'<>]*`)
	keyPattern     = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+`)
)

type safeLog struct {
	mu      sync.Mutex
	writer  io.Writer
	level   singlog.Level
	secrets []string
}

func newSafeLog(writer io.Writer, level string, secrets ...string) *safeLog {
	if writer == nil {
		writer = io.Discard
	}
	parsed, _ := singlog.ParseLevel(level)
	return &safeLog{writer: writer, level: parsed, secrets: secrets}
}

func (w *safeLog) WriteMessage(level singlog.Level, message string) {
	if level > w.level {
		return
	}
	message = redactAuthMaterial(message, w.secrets)
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintln(w.writer, message)
}

func redactAuthMaterial(message string, secrets []string) string {
	var changes []int32
	mark := func(start, end int) {
		if changes == nil {
			changes = make([]int32, len(message)+1)
		}
		changes[start]++
		changes[end]--
	}
	for _, pattern := range []*regexp.Regexp{authURLPattern, keyPattern} {
		for _, match := range pattern.FindAllStringIndex(message, -1) {
			mark(match[0], match[1])
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for offset := 0; offset < len(message); {
			index := strings.Index(message[offset:], secret)
			if index < 0 {
				break
			}
			start := offset + index
			mark(start, start+len(secret))
			offset = start + 1
		}
	}
	if changes == nil {
		return message
	}
	// Compute the union of matches in the original message. Sequential
	// replacement can hide a longer or overlapping secret from the next pass.
	var redacted strings.Builder
	var active int32
	last := 0
	for index, change := range changes {
		previous := active
		active += change
		if previous == 0 && active > 0 {
			redacted.WriteString(message[last:index])
			redacted.WriteString("<redacted>")
		} else if previous > 0 && active == 0 {
			last = index
		}
	}
	redacted.WriteString(message[last:])
	return redacted.String()
}
