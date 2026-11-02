package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/idy/tailrelay"
)

var (
	version = "dev"
	commit  = "unknown"
)

const usage = `tailrelay — one Tailscale Exit Node per ISP proxy

Usage:
  tailrelay check   -c instance.yaml
  tailrelay check-credentials -c instance.yaml [--credentials-file FILE]
  tailrelay render  -c instance.yaml
  tailrelay run     -c instance.yaml [--credentials-file FILE] [--state-dir DIR]
  tailrelay login   -c instance.yaml [--state-dir DIR]
  tailrelay status  -c instance.yaml [--state-dir DIR]
  tailrelay version

check/render are offline and never read credentials or join a Tailnet.
login prints a private authorization URL only to an interactive terminal.
status is a saved snapshot: check updated_at and phase for freshness.
`

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tailrelay:", err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintf(os.Stdout, "tailrelay %s (sing-box %s, tailscale=%t, commit=%s)\n", version, tailrelay.CoreVersion, tailrelay.TailscaleEnabled, commit)
		return nil
	case "check", "check-credentials", "render", "run", "login", "status":
	default:
		return errors.New("unknown command; use tailrelay help")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	// flag's default error output repeats untrusted arguments.
	flags.SetOutput(io.Discard)
	configPath := flags.String("c", "/etc/tailrelay/default.yaml", "configuration file")
	stateDir := flags.String("state-dir", "", "override this instance's persistent state directory")
	credentialsFile := flags.String("credentials-file", "", "override the private proxy credentials file")
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid command flags; use tailrelay help")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	config, err := tailrelay.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *stateDir != "" {
		config.StateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
	}
	if *credentialsFile != "" {
		if command != "run" && command != "check-credentials" {
			return errors.New("--credentials-file is only used by run and check-credentials")
		}
		config.Proxy.CredentialsFile, err = filepath.Abs(*credentialsFile)
		if err != nil {
			return err
		}
	}
	switch command {
	case "check-credentials":
		if _, err := tailrelay.LoadCredentials(config.Proxy.CredentialsFile); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Proxy credentials are valid.")
		return nil
	case "check":
		if err := tailrelay.Check(config); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Valid:", config.String())
		return nil
	case "render":
		compiled, err := tailrelay.Compile(config)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(compiled.JSON))
		return err
	case "status":
		status, err := tailrelay.ReadStatus(config)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	run := tailrelay.RunOptions{Log: os.Stderr, LoginOnly: command == "login"}
	if run.LoginOnly {
		info, err := os.Stdout.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return errors.New("login requires an interactive terminal; use a device auth key for unattended services")
		}
		fmt.Fprintln(os.Stderr, "Open the private login URL in your browser and select the correct Tailnet.")
		run.LoginURL = func(url string) { fmt.Fprintln(os.Stdout, url) }
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := tailrelay.Run(ctx, config, run); err != nil {
		return err
	}
	if run.LoginOnly {
		fmt.Fprintln(os.Stdout, "Device identity saved. Start this instance with tailrelay run.")
	}
	return nil
}
