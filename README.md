# TailRelay

An open-source Tailscale exit node with one explicit, authenticated SOCKS5
upstream per instance. Run separate instances to offer different exits while
keeping each device identity, configuration and provider credentials separate.

[Project page](https://tailrelay.idy.dev) · [Releases](https://github.com/idy/tailrelay/releases) · [Source](https://github.com/idy/tailrelay)

## Install

The installer detects Linux or macOS and amd64 or arm64, downloads a versioned
GitHub Release, and verifies SHA-256 before installation:

```sh
curl -fsSL https://tailrelay.idy.dev/install.sh | sh
```

The same installer is available directly from the public repository:

```sh
curl -fsSL https://raw.githubusercontent.com/idy/tailrelay/main/install.sh | sh
```

Pin a release or install without administrator access:

```sh
curl -fsSL https://raw.githubusercontent.com/idy/tailrelay/main/install.sh | sh -s -- --version 0.1.2 --no-sudo
```

Linux installation prefers a Debian package when apt/dpkg and administrator
access are available. Otherwise the CLI is installed from a tarball. Use
`--method binary --prefix "$HOME/.local" --no-sudo` to choose a user-local CLI.
Installation does not enroll a device, create an instance configuration, start a
new relay or alter routing. Package upgrades retain device state and only
restart instances that were already running.

| Platform | Architectures | Downloads |
| --- | --- | --- |
| Linux | amd64, arm64 | CLI tar.gz and .deb |
| macOS | amd64, arm64 | CLI tar.gz |

The `local_tun` feature and packaged systemd service are Linux-specific.

## Configure an exit

TailRelay embeds sing-box and userspace Tailscale; it does not require a host
`tailscaled`. The default userspace mode needs no host TUN or network
capabilities and leaves host DNS, default routes and firewall unchanged.

Start with [config.example.yaml](config.example.yaml). For example:

```yaml
version: 1
name: isp-a
state_dir: /var/lib/tailrelay/isp-a
tailscale:
  hostname: tailrelay-isp-a
  auth_key_env: TAILRELAY_AUTH_KEY
  listen_port: 0
proxy:
  server: 203.0.113.10:12324
  credentials_file: /etc/tailrelay/isp-a.credentials.json
  udp: false
dns:
  server: 1.1.1.1
  server_name: cloudflare-dns.com
```

Replace the documentation IP with your SOCKS5 provider's literal address. Store
`{"username":"ISP_USERNAME","password":"ISP_PASSWORD"}` in the separate
credentials file with mode `0600` or `0400`, owned by the service user. Do not put
credentials in YAML. Relative paths resolve against the YAML file.

```sh
tailrelay check -c isp-a.yaml
tailrelay render -c isp-a.yaml
```

These offline commands do not read credentials, enroll devices or change
networking. `check-credentials` validates credential-file permissions separately.

On Linux, the Debian package provides `tailrelay-instance` and the
`tailrelay@.service` template. Use a short, distinct instance label:

```sh
sudo tailrelay-instance isp-a
sudo install -m 0644 isp-a.yaml /etc/tailrelay/isp-a.yaml
sudo install -m 0600 isp-a.credentials.json /etc/tailrelay/isp-a.credentials.json
sudo -u tailrelay-isp-a tailrelay login -c /etc/tailrelay/isp-a.yaml
sudo systemctl enable --now tailrelay@isp-a
```

Alternatively configure a non-ephemeral Tailscale device auth key through the
configured environment variable or `tailscale.auth_key_file`. After enrollment,
retain the saved identity and remove bootstrap auth inputs. Approve the device's
exit-node routes and allow clients to use them in your existing Tailnet policy.
Route advertisement and approval are separate operations.

## Forwarding behavior

- Client forwarding is blocked until the device is authorized, unexpired and
  matches `expected_tailnet` when configured. Revocation closes existing flows.
- Client TCP and intercepted TCP/UDP port 53 DNS use the configured authenticated
  SOCKS5 provider. DNS is sent as certificate-verified DoH through that provider.
- Proxy or DNS failure does not fall back to a direct host connection or another
  provider. Anonymous SOCKS5 authentication downgrades are rejected.
- Ordinary UDP is off, including QUIC/STUN, games and voice. Enable `proxy.udp`
  only with a verified UDP ASSOCIATE provider whose relay IP matches its proxy.
  ICMP forwarding is refused.
- Private, loopback, link-local, cloud-metadata, Tailnet, multicast and FakeIP
  destinations are rejected. This is not a subnet router.

These restrictions concern forwarded client traffic. Tailscale control,
WireGuard, DERP/STUN and their bootstrap DNS use the host network. A client's
explicit LAN bypass and DNS channels outside the exit remain outside this scope.

## Local Linux namespace

The optional `local_tun` mode serves applications in a pre-created network
namespace. The deployment layer owns its default-DROP firewall and resolver:

```yaml
local_tun:
  namespace: letsolo-app-a
  interface_name: tr-tun
  management_subnet: 169.254.197.0/30
```

The existing `letsolo-` namespace prefix is retained for compatibility. This
mode explicitly requires `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and `/dev/net/tun`;
the default systemd unit grants none of them. TailRelay creates its TUN only
inside that namespace and does not remove deployment-owned firewall protection
when it stops. Namespace DNS `172.29.0.2` is intercepted and sent through the ISP.

## Build and test

Use Go 1.26.2 or later and Python 3.11 or later. Linux .deb builds also need
`dpkg-deb`, bash and standard checksum tools.

```sh
make test check
make test-packaging PYTHON=python3
python3 -B -m unittest discover -s tests/installer
GOARCH=amd64 make deb
```

The Makefile enables `with_gvisor`. Source tests use loopback fixtures and do not
replace a live Tailnet/provider acceptance test. The privileged namespace test
runs only when explicitly enabled in disposable Linux CI.

## Releases and installation assets

Pushing a `vX.Y.Z` tag matching `VERSION` runs
[the release workflow](.github/workflows/release.yml). Native Linux/macOS
amd64/arm64 builds must all finish before the release is published. It includes:

- `tailrelay_X.Y.Z_{linux,darwin}_{amd64,arm64}.tar.gz`
- `tailrelay_X.Y.Z_linux_{amd64,arm64}.deb`
- `release-manifest.json` and `SHA256SUMS`

Archives include the executable, GPL license, dependency notices and source
metadata. CI verifies the complete asset inventory and downloaded release
hashes before making its draft public. Retries do not overwrite published bytes.
The optional development snapshot tooling in `packaging/snapshot.py` also
supports public repositories. No private repository access is required to
build the project or download releases.

## Website preview

The static project page lives in `site/`. Preview it with
`python3 -m http.server 8080 --directory site`; no build or package installation
is required. The Pages workflow also publishes the root installer as `/install.sh`.

The page follows the browser's English/Chinese language preference and the
system light/dark theme. Header controls override either choice and remember it
for this site's origin. `site/shared/` contains the common idy.dev shell and
preference and release helpers. Its self-hosted fonts use Montserrat for headings,
Monda for text and controls, Lato for notes and status, and Phosphor for icons;
the font licenses are included.

Downloads use GitHub's latest stable release, falling back to the newest published
prerelease only when no stable release exists. Buttons use the actual release
asset URLs; missing assets or API failures retain useful release-page links.
The page remains readable in English without JavaScript. Run the dependency-free
website checks with `node --test tests/website.test.cjs`.

## State and license

Preserve each instance's configuration, account ownership, hostname and
`state_dir` during upgrades. Do not run concurrent processes against one saved
identity or copy it to simultaneously running devices. Package removal/purge
retains operator configuration and identity. `status` is a saved snapshot;
check its timestamp and the service process before treating it as current.

This project is GPL-3.0-or-later. See [LICENSE](LICENSE),
[third-party notices](THIRD_PARTY_NOTICES.md) and `LICENSES/`.
The initial source retains its subtree history from `letsolo/tailrelay`;
server inventories, real provider credentials and production deployment state
are separate from this open-source repository.
