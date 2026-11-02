#!/usr/bin/env bash
# Build TailRelay for one Linux architecture. Requires Go, Python 3.11+ and dpkg-deb.
set -euo pipefail
cd "$(dirname "$0")/.."
package=tailrelay
go=${GO:-go}
python=${PYTHON:-python3}
version=$("$python" -B packaging/apt-release.py version)
arch=${GOARCH:-$("$go" env GOARCH)}
case "$arch" in
  amd64) export GOAMD64=v1 ;;
  arm64) export GOARM64=v8.0 ;;
  *) echo "Unsupported architecture: $arch" >&2; exit 1 ;;
esac
commit=$(git rev-parse HEAD)
if [[ ! $commit =~ ^[0-9a-f]{40}$ ]] || [ "${GITHUB_SHA:-$commit}" != "$commit" ]; then
  echo 'Package source must match the full workflow commit' >&2
  exit 1
fi
export SOURCE_DATE_EPOCH
SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch"
output=$(pwd)/bin/deb/$arch
mkdir -p "$output"
stage=$(mktemp -d "$output/.tailrelay-deb.XXXXXX")
trap 'rm -rf "$stage"' EXIT
root=$stage/$package
install -d "$root/DEBIAN" "$root/usr/bin" "$root/usr/lib/systemd/system" \
  "$root/usr/share/doc/tailrelay/examples" "$root/usr/sbin"
"$go" build -mod=readonly -tags with_gvisor -trimpath -buildvcs=false \
  -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
  -o "$root/usr/bin/tailrelay" ./cmd/tailrelay
install -m 0644 deploy/tailrelay@.service "$root/usr/lib/systemd/system/tailrelay@.service"
install -m 0644 config.example.yaml "$root/usr/share/doc/tailrelay/examples/instance.yaml"
install -m 0644 README.md "$root/usr/share/doc/tailrelay/README.md"
install -m 0755 packaging/tailrelay-instance "$root/usr/sbin/tailrelay-instance"
for script in postinst prerm postrm; do
  install -m 0755 "packaging/$script" "$root/DEBIAN/$script"
done
chmod 0755 "$root/usr/bin/tailrelay"
printf '{"version":"%s","commit":"%s","architecture":"%s"}\n' "$version" "$commit" "$arch" > "$root/usr/share/doc/tailrelay/build.json"
cat > "$root/DEBIAN/control" <<CONTROL
Package: $package
Version: $version
Architecture: $arch
Maintainer: Yan <nautiloid@idy.dev>
Section: net
Priority: optional
Depends: ca-certificates, adduser, init-system-helpers, systemd (>= 247), iproute2, iptables, acl
Homepage: https://github.com/idy/tunrelay
Description: Independent Tailscale Exit Nodes through required ISP SOCKS5 proxies
CONTROL
# Stable timestamps, ownership and compression allow retrying the same commit.
if touch -h -d "@$SOURCE_DATE_EPOCH" "$root" 2>/dev/null; then
  find "$root" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
else
  timestamp=$(date -u -r "$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S)
  find "$root" -exec touch -h -t "$timestamp" {} +
fi
dpkg-deb --build --root-owner-group --uniform-compression -Zxz --threads-max=1 \
  "$root" "$output/${package}_${version}_${arch}.deb"
(cd "$output" && sha256sum "${package}_${version}_${arch}.deb" > SHA256SUMS)
echo "Package: $output/${package}_${version}_${arch}.deb"
