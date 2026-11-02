#!/bin/sh
# Public release installer. Run with sh; no repository checkout is required.
set -eu
LC_ALL=C
export LC_ALL
umask 077

project=tailrelay
repository="https://github.com/idy/$project"
version=latest
prefix=/usr/local
prefix_set=0
no_sudo=0
method=auto
install_root=0

say() { printf '%s\n' "$*"; }
die() { printf '%s: %s\n' "$project" "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1"; }
usage() {
    printf '%s\n' \
        'Install TailRelay from a public GitHub release, verified with SHA256SUMS.' \
        'Usage: sh install.sh [options]' \
        '  --version VERSION  vX.Y.Z or X.Y.Z (default: latest; prereleases accepted)' \
        '  --prefix DIR       CLI prefix (default: /usr/local); selects binary install' \
        '  --no-sudo          Never use sudo or packages; default prefix: $HOME/.local' \
        '  --method METHOD    auto, deb, or binary (default: auto)' \
        '  --help             Show this help' \
        '' \
        'Linux auto uses apt-get (or dpkg) when no prefix or --no-sudo is supplied.' \
        'Binary installs write only PREFIX/bin/tailrelay. Configuration is separate.' \
        'With curl piping, pass options using: sh -s -- --no-sudo'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--prefix|--method)
            [ "$#" -ge 2 ] || die "Missing value for $1"
            [ -n "$2" ] || die "Empty value for $1"
            case "$1" in
                --version) version=$2 ;;
                --prefix) prefix=$2; prefix_set=1 ;;
                --method) method=$2 ;;
            esac
            shift 2 ;;
        --no-sudo) no_sudo=1; shift ;;
        --help|-h) usage; exit 0 ;;
        *) die "Unknown option: $1 (use --help)" ;;
    esac
done
case "$method" in auto|deb|binary) ;; *) die "Invalid method: $method" ;; esac

validate_version() {
    # An allowlist keeps tags safe as both URL components and archive filenames.
    case "$1" in ''|*[!A-Za-z0-9.+-]*) return 1 ;; esac
    [ "${#1}" -le 128 ] && printf '%s\n' "$1" |
        grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}
if [ "$version" != latest ]; then
    version=${version#v}
    validate_version "$version" || die 'Invalid version; expected vX.Y.Z or X.Y.Z, optionally with prerelease/build identifiers.'
fi

os=$(uname -s) || die 'Cannot detect operating system.'
case "$os" in
    Linux) goos=linux ;;
    Darwin) goos=darwin ;;
    *) die "Unsupported operating system: $os (Linux and macOS are supported)." ;;
esac
machine=$(uname -m) || die 'Cannot detect architecture.'
case "$machine" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "Unsupported architecture: $machine (amd64 and arm64 are supported)." ;;
esac

if [ "$no_sudo" -eq 1 ] && [ "$prefix_set" -eq 0 ]; then
    [ -n "${HOME:-}" ] || die 'HOME is required for --no-sudo; supply --prefix DIR.'
    prefix="$HOME/.local"
fi
case "$prefix" in /*) ;; *) prefix="$(pwd -P)/$prefix" ;; esac
while [ "$prefix" != / ] && [ "${prefix%/}" != "$prefix" ]; do prefix=${prefix%/}; done

if [ "$method" = deb ]; then
    [ "$goos" = linux ] || die '--method deb is supported only on Linux.'
    [ "$no_sudo" -eq 0 ] || die '--no-sudo cannot install packages; use --method binary.'
    [ "$prefix_set" -eq 0 ] || die '--prefix selects a binary install; use --method binary.'
elif [ "$method" = auto ]; then
    method=binary
    if [ "$goos" = linux ] && [ "$prefix_set" -eq 0 ] && [ "$no_sudo" -eq 0 ]; then
        if command -v apt-get >/dev/null 2>&1 || command -v dpkg >/dev/null 2>&1; then
            method=deb
        fi
    fi
fi
if [ "$method" = deb ]; then
    if command -v apt-get >/dev/null 2>&1; then package_tool=apt-get
    elif command -v dpkg >/dev/null 2>&1; then package_tool=dpkg
    else die 'A Debian install requires apt-get or dpkg; use --method binary.'
    fi
else
    need tar
    need install
fi
need curl
need awk
if command -v sha256sum >/dev/null 2>&1; then hash_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then hash_tool=shasum
else die 'SHA256 verification requires sha256sum or shasum.'
fi

workdir=$(mktemp -d "${TMPDIR:-/tmp}/$project-install.XXXXXXXX") || die 'Cannot create a temporary directory.'
trap 'rm -rf "$workdir"' 0
trap 'exit 1' HUP INT TERM

fetch() {
    # -q disables curlrc credentials/options; all initial and redirect URLs use HTTPS.
    curl -q --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --output "$2" "$1" || die "Download failed: $1"
}
if [ "$version" = latest ]; then
    resolved=$(curl -q --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --output /dev/null --write-out '%{url_effective}' "$repository/releases/latest") || die 'Cannot resolve the latest release.'
    case "$resolved" in
        "$repository/releases/tag/v"*) version=${resolved#"$repository/releases/tag/v"} ;;
        *) die "Unexpected latest release redirect: $resolved" ;;
    esac
    validate_version "$version" || die 'The latest release redirect does not contain a valid version tag.'
fi
tag="v$version"
if [ "$method" = deb ]; then asset="${project}_${version}_linux_${arch}.deb"
else asset="${project}_${version}_${goos}_${arch}.tar.gz"
fi
case "$asset" in ''|*[!A-Za-z0-9._+-]*) die 'Unsafe release asset filename.' ;; esac
base="$repository/releases/download/$tag"
say "Downloading $project $tag ($goos/$arch, $method)..."
fetch "$base/$asset" "$workdir/$asset"
fetch "$base/SHA256SUMS" "$workdir/SHA256SUMS"
expected=$(awk -v name="$asset" '
    $2 == name || $2 == "*" name {
        count++
        if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) bad = 1
        hash = tolower($1)
    }
    END { if (count != 1 || bad) exit 1; print hash }
' "$workdir/SHA256SUMS") || die "SHA256SUMS must contain exactly one valid SHA256 for $asset."
if [ "$hash_tool" = sha256sum ]; then actual=$(sha256sum < "$workdir/$asset")
else actual=$(shasum -a 256 < "$workdir/$asset")
fi
actual=${actual%% *}
[ "$actual" = "$expected" ] || die "SHA256 mismatch for $asset; nothing was installed."

privileged() {
    if [ "$(id -u)" = 0 ]; then "$@"
    elif [ "$no_sudo" -eq 1 ]; then
        die 'Destination is not writable. Use --no-sudo --prefix "$HOME/.local" (or another writable prefix).'
    elif command -v sudo >/dev/null 2>&1; then sudo "$@"
    else die 'sudo is required for this destination. Use --no-sudo --prefix "$HOME/.local" for a user install.'
    fi
}
run_install() {
    if [ "$install_root" -eq 1 ]; then privileged "$@"; else "$@"; fi
}

if [ "$method" = deb ]; then
    # Package maintainer scripts own upgrades; no service/config/device commands run here.
    if [ "$package_tool" = apt-get ]; then
        privileged apt-get install -y --no-install-recommends "$workdir/$asset"
    else
        privileged dpkg -i "$workdir/$asset"
    fi
    say "Installed $project $tag using $package_tool."
    exit 0
fi

# Validate the entire archive before extraction, including duplicate names and types.
tar -tzf "$workdir/$asset" > "$workdir/entries" || die 'Cannot list the CLI archive.'
awk -v name="$project" '
    $0 != name && $0 != "LICENSE" && $0 != "THIRD_PARTY_NOTICES.md" && $0 != "build.json" { bad = 1 }
    { if (++seen[$0] != 1) bad = 1 }
    END { if (bad || NR != 4 || !seen[name] || !seen["LICENSE"] || !seen["THIRD_PARTY_NOTICES.md"] || !seen["build.json"]) exit 1 }
' "$workdir/entries" || die 'Unsafe or unexpected CLI archive entries.'
tar -tvzf "$workdir/$asset" > "$workdir/types" || die 'Cannot inspect CLI archive types.'
awk 'substr($0, 1, 1) != "-" { bad = 1 } END { if (bad || NR != 4) exit 1 }' "$workdir/types" ||
    die 'CLI archive must contain only regular files (no links).'
mkdir "$workdir/extracted"
tar -xzf "$workdir/$asset" -C "$workdir/extracted" "$project" || die 'Cannot extract the CLI executable.'
binary="$workdir/extracted/$project"
[ -f "$binary" ] && [ ! -L "$binary" ] || die 'The CLI executable is not a regular file.'
bin_dir="${prefix%/}/bin"
destination="$bin_dir/$project"
[ ! -L "$destination" ] && [ ! -d "$destination" ] || die "Refusing to overwrite a link or directory: $destination"
parent=$bin_dir
while [ ! -e "$parent" ]; do parent=${parent%/*}; [ -n "$parent" ] || parent=/; done
[ -d "$parent" ] || die "Not a directory: $parent"
if [ ! -w "$parent" ] || { [ -e "$destination" ] && [ ! -w "$destination" ]; }; then install_root=1; fi
if [ "$install_root" -eq 1 ] && [ "$no_sudo" -eq 1 ]; then
    die 'Destination is not writable. Use --no-sudo --prefix "$HOME/.local" (or another writable prefix).'
fi
[ -d "$bin_dir" ] || run_install install -d -m 0755 "$bin_dir"
run_install install -m 0755 "$binary" "$destination"
say "Installed $project $tag at $destination."
