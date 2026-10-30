//go:build !with_gvisor

package tailrelay

import "github.com/sagernet/sing-box/adapter/endpoint"

const TailscaleEnabled = false

func registerTailscale(_ *endpoint.Registry) {}
