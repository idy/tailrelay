# TailRelay

TailRelay runs one independent Tailscale Exit Node per authenticated ISP SOCKS5
proxy. Multiple instances on one Linux host can offer separate exits, each with
its own hostname, credentials and persistent device identity.

Project site: [tailrelay.idy.dev](https://tailrelay.idy.dev) (planned).
Source, CI and authenticated release downloads live in the private
[idy/tailrelay](https://github.com/idy/tailrelay) repository. The Go module is
`github.com/idy/tailrelay`; the command, Debian package, service accounts and
systemd units keep the `tailrelay` name.

TailRelay embeds sing-box 1.14.4 and userspace Tailscale. Its default mode needs
no host `tailscaled`, kernel TUN or network capabilities and does not change the
host's default route, DNS, IP forwarding or firewall. An optional `local_tun`
mode supplies transparent egress inside an existing, isolated Linux network
namespace.

## Forwarding behavior

- Client forwarding starts blocked. TailRelay checks the saved or newly enrolled
  Tailscale identity, device expiry and optional `expected_tailnet` before
  allowing ISP connections. Losing authorization closes existing TCP, UDP and
  DoH connections and blocks new ones.
- The configured ISP proxy is the only client egress. Proxy connection,
  authentication or destination failures do not fall back to the host network
  or another ISP. SOCKS5 username/password authentication is required; anonymous
  authentication downgrades are rejected.
- TCP/UDP port 53 queries and Tailscale peer DNS use certificate-verified DoH
  through that same ISP. There is no local DNS fallback.
- Ordinary UDP is disabled by default, including QUIC, STUN, game and voice
  traffic. Enable `proxy.udp` only after confirming SOCKS5 UDP ASSOCIATE support.
  The UDP relay IP must equal the configured proxy IP. ICMP forwarding is denied.
- Both IPv4 and IPv6 Exit Node routes are advertised. IPv6 destinations still
  require upstream SOCKS5 IPv6 support. This service does not support every IP
  protocol or provide subnet routing.
- Client access to loopback, private, link-local, cloud metadata, Tailnet,
  multicast and FakeIP addresses is denied. DNS interception happens before
  these destination checks.

These restrictions apply to client forwarding. Tailscale control connections,
WireGuard, DERP/STUN and the system DNS needed to reach control services use the
host network. Client-selected LAN bypass routes and DNS channels outside the
Exit Node remain outside TailRelay's control.

## Build and offline validation

Use Go 1.26.2. Python 3.11 or newer is needed for packaging and snapshot tooling;
CI uses Python 3.12. Debian package builds additionally need `dpkg-deb` and
`sha256sum`. Run commands from this repository's root:

```sh
make test-packaging
make test check linux
```

The required `with_gvisor` build tag is set by the Makefile. Outputs are
`bin/tailrelay`, `bin/tailrelay-linux-amd64` and
`bin/tailrelay-linux-arm64`. The Linux binaries are static, with amd64/v1 and
arm64/v8.0 baselines.

```sh
bin/tailrelay check -c config.example.yaml
bin/tailrelay render -c config.example.yaml
```

`check` and `render` do not read credentials, create state, enroll a device or
change networking. Rendered credentials use `<runtime>` placeholders. The
`tailrelay-isp` outbound in the rendered configuration is registered by
TailRelay and cannot be run by a stock `sing-box` command.

Source tests use loopback fixtures. They cover two independent ISP exits,
proxied TLS DoH, authorization gating and revocation, proxy failure, anonymous
authentication rejection, explicit UDP support, private destination rejection,
configuration permissions and state locks. Tests compiled with the
`tailrelay_live` tag run offline probe tests; a live Tailnet acceptance run is a
separate, explicitly configured operator action. The privileged namespace test
runs only in disposable Linux CI runners with `TAILRELAY_PRIVILEGED_TEST=1`.

## Commit-pinned snapshots

[The TailRelay workflow](.github/workflows/tailrelay.yml) runs Linux and macOS
checks, verifies the real Linux network namespace TUN lifecycle, and uploads an
Actions artifact for 14 days. A successful push or manual run on the default
branch can then create a private GitHub snapshot prerelease. Pull requests
build and test without publishing. Snapshot publication rejects a public
GitHub repository.

Each `tailrelay-snapshot-<full-source-commit>` release contains exactly:

- `tailrelay-linux-amd64`
- `tailrelay-linux-arm64`
- `release-manifest.json`, containing source commit, snapshot version, OS,
  architecture, size and SHA256 for each executable
- `SHA256SUMS`, covering both executables and the manifest

`tailrelay version` includes `VERSION+snapshot.<12-character-commit>` and the
full source commit. Publication creates a draft, verifies downloaded remote
assets, then marks it as a prerelease without changing GitHub's Latest release.
A retry uploads only missing draft assets. Conflicting assets or a moved tag
cause a failure; published bytes are never overwritten.

Authenticate `gh` with access to the private repository, then select a source
commit and download its snapshot:

```sh
SNAPSHOT_COMMIT='replace-with-the-full-source-commit'
gh release download "tailrelay-snapshot-$SNAPSHOT_COMMIT" \
  --repo idy/tailrelay --dir bin/downloaded-snapshot
(cd bin/downloaded-snapshot && shasum -a 256 --check SHA256SUMS)
```

Record the chosen commit and hashes in deployment inputs. Publishing a snapshot
does not deploy or restart a service. A clean checkout can run `make snapshot`.
For local validation of uncommitted work, use:

```sh
python3 -B packaging/snapshot.py build --allow-dirty --directory bin/local-snapshot
```

A bundle marked dirty cannot be published. Snapshot source checks cover the
whole standalone checkout, including root Go files, packaging and workflows.

## Debian packages and APT enablement

The self-contained builder and publisher live in `packaging/`:

```sh
GOARCH=amd64 make deb
GOARCH=arm64 make deb
```

Packages and checksums appear in `bin/deb/<arch>/`. Each package embeds its
version, architecture and full source commit in
`/usr/share/doc/tailrelay/build.json` and its executable. Stable timestamps,
root ownership and fixed compression preserve retry reproducibility. The only
example YAML is under `/usr/share/doc/tailrelay/examples/`; no operator
configuration, credentials or device identity is shipped.

[The APT workflow](.github/workflows/tailrelay-apt.yml) validates `VERSION` and
runs native amd64/arm64 race tests, configuration checks, and package
install/upgrade/remove/purge tests. It retains Debian artifacts and
`SHA256SUMS` for 30 days. Builds and lifecycle tests run even while APT
publication is disabled.

All GCP OIDC authentication, APT inventory checks and APT publication are gated
by the repository variable **`APT_PUBLISH_ENABLED` being exactly `true`**.
Leave it unset or false until the cloud Workload Identity Federation policy
has explicitly trusted `idy/tailrelay` and its default branch, and an operator
has reviewed the existing publisher service account and registry permissions.
The workflow retains the existing OIDC provider, service account and APT target
`nautiloid-testing / us-west2 / ppa`; extraction does not configure cloud trust
or require a stored cloud key. Pull requests, non-default branches and reusable
workflow calls with `snapshot: true` never inspect or publish APT.

After that review, enabling the variable allows default-branch runs to inspect
the committed `VERSION` and publish missing architectures. An already complete
version is downloaded and verified without rebuilding or uploading. A partial
retry must match existing package hashes before the first write; otherwise
retry the original source commit or bump root-level `VERSION`. Do not reuse a
published version for changed source. The APT channel exposes package downloads
through the existing public registry; private source, operator configuration
and device state are not uploaded. Workflow summaries distinguish build-only
validation, existing verified releases and completed publication.

## Configure an instance

Copy `config.example.yaml` to an instance file such as `isp-a.yaml`. Every
instance needs a distinct `name`, `state_dir` and Tailscale hostname:

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

Replace the documentation address `203.0.113.10` with the ISP's actual SOCKS5
IP. Proxy hostnames are rejected to avoid a bootstrap DNS dependency. The DoH
IP must match its TLS `server_name`; the default is Cloudflare DoH. Relative
file paths resolve against the YAML directory.

Keep proxy credentials in a separate regular JSON file with mode `0600` or
`0400`, owned by the invoking user:

```json
{"username":"ISP_USERNAME","password":"ISP_PASSWORD"}
```

Unknown YAML fields, duplicate keys, multiple YAML documents, inline secrets,
invalid addresses and unsafe state permissions are rejected. systemd
`LoadCredential` is the explicit exception for root-owned `0400`/`0440`
credentials provided through its private read-only mount and service-user ACL.
`tailrelay check-credentials` validates credentials without connecting to the
network or creating state.

Optional settings include `tailscale.auth_key_file` instead of
`auth_key_env`, `tailscale.tags: [tag:tailrelay]`,
`tailscale.expected_tailnet: example.com`, `proxy.connect_timeout: 10s` and
`dns.port: 443` / `dns.path: /dns-query`. Omitting both auth-key settings uses
the retained device identity. Tailnet tags require corresponding policy
permission. Set `expected_tailnet` to the name reported by `tailrelay status`.
`listen_port: 0` allocates a separate UDP port to each process; fixed ports must
be unique and allowed by the host's existing firewall policy if direct
Tailscale connections are needed.

## Linux systemd instances

Install the appropriate package on a host with systemd 247 or newer:

```sh
VERSION=$(cat VERSION)
sudo apt-get install "./bin/deb/amd64/tailrelay_${VERSION}_amd64.deb"
```

Hosts already configured for the APT source can use `sudo apt-get install
tailrelay` after that version is published. First installation starts no
instances and enrolls no devices. The instance helper prepares a persistent
unprivileged account and state directory without starting or logging in:

```sh
sudo tailrelay-instance isp-a
sudo install -m 0644 isp-a.yaml /etc/tailrelay/isp-a.yaml
sudo install -m 0600 isp-a.credentials.json /etc/tailrelay/isp-a.credentials.json
```

The unit is `tailrelay@isp-a.service`, the account is `tailrelay-isp-a` and the
state directory is `/var/lib/tailrelay/isp-a`. The instance label must fit the
helper's 22-character limit. systemd passes root-managed ISP credentials using
`LoadCredential`.

For server enrollment, create a non-ephemeral Tailscale device auth key for the
intended Tailnet with the needed tags and approval settings. Store
`TAILRELAY_AUTH_KEY=...` in `/etc/tailrelay/isp-a.env` using a secure editor and
mode `0600`. It is a device auth key, not a Tailscale API access token. After
successful enrollment, remove the bootstrap environment file and restart the
instance to confirm that saved identity works.

Alternatively, omit the auth key, stop that instance's service, and enroll from
an interactive terminal as its service user:

```sh
sudo -u tailrelay-isp-a /usr/bin/tailrelay login -c /etc/tailrelay/isp-a.yaml
```

Open the private authorization URL shown in that terminal and select the
correct Tailnet. `login` saves identity and exits without forwarding traffic,
reading ISP credentials or creating a local TUN. It does not open a browser or
save the login URL in status files.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now tailrelay@isp-a
sudo systemctl status tailrelay@isp-a
sudo journalctl -u tailrelay@isp-a -n 50 --no-pager
```

Repeat with a different label and state directory for another ISP. Services
hold an exclusive state lock, so two processes cannot share one identity.
Stopping or restarting an instance does not clear its login state.

Approve the advertised IPv4/IPv6 Exit Node routes in the Tailscale admin
console, or merge an appropriate `autoApprovers.exitNode` policy into the
existing Tailnet policy. Clients also need permission to use the Internet
exit. Select the matching hostname in a native Tailscale client or a compatible
client such as TailSplit. Route advertisement, admin approval and client
permission are separate checks.

Read a saved status snapshot as the instance user:

```sh
sudo -u tailrelay-isp-a /usr/bin/tailrelay status -c /etc/tailrelay/isp-a.yaml
```

Status updates every five seconds. Check `updated_at`, `phase` and the service
process; a crash can leave a stale snapshot. `tailnet_connected` does not prove
that the ISP works, routes are approved or the observed public IP is correct.
Live acceptance should be performed separately on an authorized client using
the chosen Exit Node, comparing its public IP with the intended ISP. External
`ping` is unsuitable because ICMP forwarding is denied.

## Optional local network namespace

Existing instances can also serve transparent local traffic inside a
pre-created namespace:

```yaml
local_tun:
  namespace: letsolo-app-a
  interface_name: tr-tun
  management_subnet: 169.254.197.0/30
```

The deployment layer must create that namespace and install its independent
default-DROP firewall before TailRelay starts. The `letsolo-` namespace prefix
remains required for compatibility; renaming this repository does not change
namespace validation. TailRelay creates the TUN and routes only inside the
specified namespace. Control connections, ISP sockets and persistent identity
continue to use their existing host paths.

This mode explicitly requires `CAP_SYS_ADMIN` to enter the existing network
namespace, `CAP_NET_ADMIN` and access to `/dev/net/tun`. The packaged default
unit grants none of these; any instance-specific systemd override and firewall
setup belong to deployment. Namespace DNS is `172.29.0.2`, intercepted by the
TUN and sent through the ISP to DoH. Deployment manages the namespace's
`resolv.conf`; TailRelay does not write host DNS. Local traffic uses the same
proxy, DNS restrictions and authorization gate as Tailnet traffic. Stopping
TailRelay removes its TUN but leaves deployment-owned DROP rules in place.

## Upgrades, state preservation and extraction origin

Back up `/etc/tailrelay/` and `/var/lib/tailrelay/<instance>` with their original
ownership and permissions before moving a service. Preserve each instance's
configuration, ISP credentials, state directory, hostname, service account and
systemd overrides. Do not delete state or run concurrent copies of one device
identity. A reinstall onto a different host needs the state restored under its
corresponding service account. Expired or revoked identities still need fresh
authorization.

Package upgrades restart only already active instances; arrange a maintenance
window. Remove and purge retain operator configuration, credentials, service
accounts and identity. Binary replacement should preserve state and occur with
the affected instance stopped. This extraction does not change deployed
services, their accounts or paths.

This repository preserves the Git subtree history of `tailrelay/` from
[`idy/letsolo`](https://github.com/idy/letsolo/tree/524201acd2b92443f50e60854ef31a4659ddb6d1/tailrelay)
at source commit `524201acd2b92443f50e60854ef31a4659ddb6d1`.
Shared Debian/APT tooling and the two TailRelay workflows were adapted into
this repository. Builds no longer require a sibling monorepo checkout.
