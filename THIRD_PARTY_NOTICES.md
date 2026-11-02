# Third-party notices

TailRelay is distributed under GPL-3.0-or-later. Its embedded networking core is
[sing-box](https://github.com/SagerNet/sing-box), Copyright 2022 nekohasekai,
licensed under GPL-3.0-or-later; its upstream notice is preserved in
`LICENSES/sing-box.txt`. TailRelay uses its own project name and does not imply
endorsement by the upstream project.

The Tailscale implementation is provided by the
[SagerNet fork](https://github.com/SagerNet/tailscale) of
[Tailscale](https://github.com/tailscale/tailscale). Go dependencies retain their
respective licenses and copyright notices. Exact versions and source locations
are recorded in `go.mod` and `go.sum`. The Go standard library uses the BSD
license preserved in `LICENSES/go.txt` (from the Go distribution's `LICENSE`).

Release builds generate a complete notice file from the license, notice and
copyright files of the modules used by each platform. That generated file, the
project license, and build metadata are included in binary archives and Debian
packages. The full corresponding project source is available in this repository.
