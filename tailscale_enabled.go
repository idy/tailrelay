//go:build with_gvisor

package tailrelay

import (
	"os"

	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/protocol/tailscale"
	"github.com/sagernet/tailscale/envknob"
)

const TailscaleEnabled = true

func init() {
	// Avoid the port-80 control Noise fast path stalling behind middleboxes.
	if _, configured := os.LookupEnv("TS_FORCE_NOISE_443"); !configured {
		envknob.Setenv("TS_FORCE_NOISE_443", "true")
	}
}

func registerTailscale(endpoints *endpoint.Registry) {
	tailscale.RegisterEndpoint(endpoints)
}
