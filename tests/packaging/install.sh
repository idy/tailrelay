#!/usr/bin/env bash
# Run only as root in a disposable container or CI runner.
set -euo pipefail
test "$(id -u)" = 0
directory=$(realpath "${1:?usage: install.sh package-directory}")
version=$(cat "$(dirname "$0")/../../VERSION")
commit=$(git -C "$(dirname "$0")/../.." rev-parse HEAD)
arch=$(dpkg --print-architecture)
(cd "$directory" && sha256sum --check SHA256SUMS)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
test ! -e /usr/bin/tailrelay
test ! -e /etc/tailrelay
deb=$directory/tailrelay_${version}_${arch}.deb
test "$(dpkg-deb --field "$deb" Package)" = tailrelay
test "$(dpkg-deb --field "$deb" Version)" = "$version"
test "$(dpkg-deb --field "$deb" Architecture)" = "$arch"
dpkg-deb --raw-extract "$deb" "$stage/package"
test ! -e "$stage/package/etc/tailrelay"
test ! -e "$stage/package/var/lib/tailrelay"
test -f "$stage/package/usr/lib/systemd/system/tailrelay@.service"
python3 -B - "$stage/package/usr/share/doc/tailrelay/build.json" "$version" "$arch" "$commit" <<'PYTHON'
import json
import sys
from pathlib import Path

path, version, arch, commit = sys.argv[1:]
metadata = json.loads(Path(path).read_text())
assert metadata == {"version": version, "architecture": arch, "commit": commit}, metadata
PYTHON
env -i PATH=/usr/bin:/bin "$stage/package/usr/bin/tailrelay" version |
    grep -F "tailrelay $version " | grep -F "commit=$commit"
dpkg --compare-versions "$version~upgradetest" lt "$version"
sed -i "s/^Version: .*/Version: $version~upgradetest/" "$stage/package/DEBIAN/control"
dpkg-deb --build --root-owner-group "$stage/package" "$stage/old.deb"
dpkg --install "$stage/old.deb"
test ! -e /etc/tailrelay/default.yaml
test ! -e /var/lib/tailrelay/package-test
tailrelay-instance package-test
test "$(id -u tailrelay-package-test)" != 0
instance_uid=$(id -u tailrelay-package-test)
test "$(stat -c %a /var/lib/tailrelay/package-test)" = 700
printf 'operator configuration\n' >/etc/tailrelay/package-test.yaml
printf '{"username":"fixture","password":"private"}\n' >/etc/tailrelay/package-test.credentials.json
chmod 0600 /etc/tailrelay/package-test.credentials.json
runuser -u tailrelay-package-test -- sh -c 'printf "retained device identity\n" > /var/lib/tailrelay/package-test/identity-test'
sha256sum /etc/tailrelay/package-test* /var/lib/tailrelay/package-test/identity-test >"$stage/retained.sha256"
dpkg --install "$deb"
dpkg --install "$deb"
tailrelay-instance package-test
sha256sum --check "$stage/retained.sha256"
test "$(id -u tailrelay-package-test)" = "$instance_uid"
tailrelay version | grep -F "tailrelay $version "
if [ -d /run/systemd/system ]; then
    systemd-analyze verify /usr/lib/systemd/system/tailrelay@.service
    systemd-run --unit=tailrelay-package-credential-check --wait --pipe --collect \
        -p User=tailrelay-package-test -p Group=tailrelay-package-test \
        -p LoadCredential=proxy:/etc/tailrelay/package-test.credentials.json \
        /bin/sh -c 'exec /usr/bin/tailrelay check-credentials \
        -c /usr/share/doc/tailrelay/examples/instance.yaml --credentials-file "$CREDENTIALS_DIRECTORY/proxy"'
    test "$(systemctl list-units --type=service --state=active --no-legend 'tailrelay@*.service' | wc -l)" = 0
fi
dpkg --remove tailrelay
test ! -e /usr/bin/tailrelay
sha256sum --check "$stage/retained.sha256"
dpkg --purge tailrelay
sha256sum --check "$stage/retained.sha256"
test "$(id -u tailrelay-package-test)" = "$instance_uid"
echo "Package install, upgrade, remove and purge retain the device identity: $version $arch"
