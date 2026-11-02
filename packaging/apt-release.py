#!/usr/bin/env python3
"""Check/publish TailRelay's committed version. Only `publish` can write to GCP."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import time
from urllib.error import HTTPError
from urllib.parse import unquote, urljoin, urlparse
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ("tailrelay",)
ARCHITECTURES = ("amd64", "arm64")


def read_version(path=ROOT / "VERSION"):
    version = path.read_text().strip()
    # Deliberately use a filename/linker-safe subset of Debian versions, without
    # epochs: major.minor.patch[~prerelease or +metadata][-debian_revision].
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:[+~][0-9A-Za-z.+~]+)?(?:-[1-9][0-9]*)?", version):
        raise ValueError("VERSION must be major.minor.patch[~prerelease][+metadata][-revision]")
    return version


def expected_files(version):
    return {f"{package}_{version}_{arch}.deb": (package, arch)
            for package in PACKAGES for arch in ARCHITECTURES}


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


class Registry:
    def __init__(self, project, location, repository):
        self.project, self.location, self.repository = project, location, repository
        self.parent = f"projects/{project}/locations/{location}/repositories/{repository}"
        self.flags = [f"--project={project}", f"--location={location}", f"--repository={repository}"]
        self.apt_url = f"https://{location}-apt.pkg.dev/projects/{project}/"

    def inventory(self, version):
        result = {}
        for package in PACKAGES:
            # gcloud follows every page. A missing package/version returns [],
            # whereas auth/network/repository errors must stop the release.
            output = subprocess.check_output([
                "gcloud", "artifacts", "files", "list", *self.flags,
                f"--package={package}", f"--version={version}", "--format=json",
            ], text=True)
            for item in json.loads(output):
                owner = f"{self.parent}/packages/{package}/versions/{version}"
                if unquote(item.get("owner", "")) != owner:
                    raise ValueError("Unexpected Artifact Registry file owner")
                name = PurePosixPath(unquote(item["name"].split("/files/", 1)[1])).name
                # Artifact Registry adds a content MD5 to the stored filename.
                match = re.fullmatch(re.escape(f"{package}_{version}_") +
                                     r"(amd64|arm64)(?:_[0-9a-f]{32})?\.deb", name)
                if not match:
                    raise ValueError(f"Unexpected package filename: {name}")
                key = f"{package}_{version}_{match[1]}.deb"
                hashes = [h["value"] for h in item["hashes"] if h["type"] == "SHA256"]
                if len(hashes) != 1 or key in result:
                    raise ValueError(f"Missing hash or duplicate package/version/architecture: {key}")
                # gcloud >= 536.0.0 renders file hashes as hexadecimal; older
                # versions expose the API's (possibly URL-safe) Base64 value.
                value = hashes[0]
                digest = (bytes.fromhex(value) if re.fullmatch(r"[0-9a-fA-F]{64}", value)
                          else base64.b64decode(value, altchars=b"-_", validate=True))
                if len(digest) != 32:
                    raise ValueError(f"Invalid SHA256: {key}")
                result[key] = digest.hex()
        return result

    def upload(self, path):
        subprocess.run([
            "gcloud", "artifacts", "apt", "upload", self.repository,
            f"--project={self.project}", f"--location={self.location}",
            f"--source={path}", "--quiet",
        ], check=True)


def local_packages(directory, version):
    paths = {}
    for path in directory.rglob("*.deb"):
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"Package must be a regular file: {path.name}")
        if path.name in paths:
            raise ValueError(f"Duplicate local artifact: {path.name}")
        paths[path.name] = path
    expected = expected_files(version)
    if paths.keys() != expected.keys():
        raise ValueError("Release requires exactly one tailrelay package for each of amd64 and arm64")
    for name, (package, arch) in expected.items():
        for field, value in (("Package", package), ("Version", version), ("Architecture", arch)):
            actual = subprocess.check_output(["dpkg-deb", "--field", str(paths[name]), field], text=True).strip()
            if actual != value:
                raise ValueError(f"Wrong {field} in {name}: {actual}")
    return paths


def ensure_matches(existing, hashes):
    for name, digest in existing.items():
        if hashes.get(name) != digest:
            raise ValueError(f"Published bytes differ for {name}; rerun the original commit or bump VERSION")


def index_entries(text):
    for paragraph in text.strip().split("\n\n"):
        fields = {}
        for line in paragraph.splitlines():
            if line and not line[0].isspace() and ": " in line:
                key, value = line.split(": ", 1)
                fields[key] = value
        if fields:
            yield fields


def verify_apt(registry, version, hashes, download=True):
    """Wait for the public APT index, then download and hash the actual packages."""
    pending = dict(hashes)
    for attempt in range(21):
        for arch in ARCHITECTURES:
            index = f"{registry.apt_url}dists/{registry.repository}/main/binary-{arch}/Packages"
            try:
                with urlopen(index, timeout=30) as response:
                    entries = list(index_entries(response.read().decode()))
            except HTTPError as error:
                error.close()
                if error.code != 404:
                    raise
                continue  # A new architecture may not have an index yet.
            for item in entries:
                name = f"{item.get('Package')}_{item.get('Version')}_{item.get('Architecture')}.deb"
                if name not in pending:
                    continue
                if item["SHA256"] != pending[name]:
                    raise ValueError(f"APT index content conflict: {name}")
                url = urljoin(registry.apt_url, item["Filename"])
                if urlparse(url).netloc != urlparse(registry.apt_url).netloc or urlparse(url).scheme != "https":
                    raise ValueError("Unexpected APT download origin")
                if download:
                    with urlopen(url, timeout=60) as response:
                        digest = hashlib.file_digest(response, "sha256").hexdigest()
                    if digest != pending[name]:
                        raise ValueError(f"APT download content conflict: {name}")
                print(f"APT {'download' if download else 'index'} verified: {name}", flush=True)
                del pending[name]
        if not pending:
            return
        if attempt < 20:
            print(f"Waiting for APT index: {', '.join(sorted(pending))}", flush=True)
            time.sleep(15)
    raise ValueError("APT index did not expose every package within five minutes")


def publish(registry, directory, version):
    paths = local_packages(directory, version)
    hashes = {name: sha256(path) for name, path in paths.items()}
    existing = registry.inventory(version)  # Recheck under the workflow's release lock.
    if existing.keys() == hashes.keys():
        print(f"Version {version} already exists for all packages/architectures; skipping uploads")
        verify_apt(registry, version, existing)
        return
    # Preflight all existing bytes BEFORE the first write. Partial releases from
    # a different commit must not combine binaries with the same version label.
    ensure_matches(existing, hashes)
    for name in sorted(paths.keys() - existing.keys()):
        registry.upload(paths[name])
    for attempt in range(13):
        existing = registry.inventory(version)
        ensure_matches(existing, hashes)
        if existing.keys() == hashes.keys():
            verify_apt(registry, version, hashes)
            return
        if attempt < 12:
            time.sleep(5)
    raise ValueError("Upload finished but Artifact Registry did not expose every package")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("version", "check", "publish"))
    parser.add_argument("--version-file", type=Path, default=ROOT / "VERSION")
    parser.add_argument("--project", default="nautiloid-testing")
    parser.add_argument("--location", default="us-west2")
    parser.add_argument("--repository", default="ppa")
    parser.add_argument("--directory", type=Path, default=ROOT / "bin/deb")
    args = parser.parse_args()
    version = read_version(args.version_file)
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a") as stream:
            stream.write(f"version={version}\n")
    if args.command == "version":
        print(version)
        return
    registry = Registry(args.project, args.location, args.repository)
    if args.command == "check":
        existing = registry.inventory(version)
        missing = sorted(expected_files(version).keys() - existing.keys())
        print(json.dumps({"version": version, "missing": missing}, indent=2))
        if output := os.environ.get("GITHUB_OUTPUT"):
            with open(output, "a") as stream:
                stream.write(f"missing={str(bool(missing)).lower()}\n")
        if not missing:
            # Recover a release that uploaded successfully but failed validation:
            # verify the original public bytes without rebuilding/overwriting.
            verify_apt(registry, version, existing)
    else:
        publish(registry, args.directory, version)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"APT release failed: {error}", file=sys.stderr)
        sys.exit(1)
