#!/usr/bin/env python3
"""Build and publish commit-pinned TailRelay snapshot prereleases."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
from urllib.error import HTTPError
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]
ARCHES = {"amd64": 62, "arm64": 183}
BINARIES = {f"tailrelay-linux-{arch}": arch for arch in ARCHES}
MANIFEST = "release-manifest.json"
CHECKSUMS = "SHA256SUMS"
ASSETS = set(BINARIES) | {MANIFEST, CHECKSUMS}
TAG_PREFIX = "tailrelay-snapshot-"


def run(*args, cwd=ROOT, capture=True, **kwargs):
    return subprocess.run(list(map(str, args)), cwd=cwd, check=True, text=True,
                          capture_output=capture, **kwargs).stdout


def git(*args):
    return run("git", *args).strip()


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source(allow_dirty=False):
    commit = git("rev-parse", "HEAD")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Snapshot source must be a full Git commit")
    if os.environ.get("GITHUB_SHA", commit) != commit:
        raise ValueError("Checkout differs from the workflow source commit")
    # Runtime code, packaging and workflows all live at the standalone root.
    dirty = bool(git("status", "--porcelain", "--untracked-files=normal"))
    if dirty and not allow_dirty:
        raise ValueError("Commit TailRelay changes first; --allow-dirty is only for local builds")
    base = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", base):
        raise ValueError("TailRelay snapshot VERSION must be major.minor.patch")
    return {"schema_version": 1, "package": "tailrelay", "channel": "snapshot",
            "version": f"{base}+snapshot.{commit[:12]}", "commit": commit,
            "tag": TAG_PREFIX + commit, "dirty": dirty}


def check_binary(path, arch):
    if path.is_symlink() or not path.is_file():
        raise ValueError("Missing regular Linux executable: " + path.name)
    with path.open("rb") as stream:
        header = stream.read(64)
    if (len(header) != 64 or header[:7] != b"\x7fELF\x02\x01\x01" or
            int.from_bytes(header[18:20], "little") != ARCHES[arch]):
        raise ValueError("Unexpected ELF architecture: " + path.name)


def assemble(directory, identity):
    # A fixed inventory prevents private config/state or test binaries being shipped.
    if {p.name for p in directory.iterdir()} != set(BINARIES):
        raise ValueError("Snapshot input must contain exactly the two Linux executables")
    assets = {}
    for name, arch in BINARIES.items():
        path = directory / name
        check_binary(path, arch)
        assets[name] = {"os": "linux", "arch": arch, "sha256": digest(path),
                        "size": path.stat().st_size}
    manifest = dict(identity, assets=assets)
    (directory / MANIFEST).write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    hashes = {name: digest(directory / name) for name in set(BINARIES) | {MANIFEST}}
    (directory / CHECKSUMS).write_text("".join(
        f"{checksum}  {name}\n" for name, checksum in sorted(hashes.items())))
    return manifest


def verify(directory, identity):
    if {p.name for p in directory.iterdir()} != ASSETS:
        raise ValueError("Snapshot bundle must contain exactly four release assets")
    for name in ASSETS:
        if (directory / name).is_symlink() or not (directory / name).is_file():
            raise ValueError("Snapshot assets must be regular files")
    manifest = json.loads((directory / MANIFEST).read_text())
    if {key: value for key, value in manifest.items() if key != "assets"} != identity:
        raise ValueError("Snapshot manifest differs from the checked-out source")
    assets = manifest.get("assets", {})
    if set(assets) != set(BINARIES):
        raise ValueError("Snapshot manifest must describe both Linux architectures")
    for name, arch in BINARIES.items():
        path = directory / name
        check_binary(path, arch)
        expected = {"os": "linux", "arch": arch, "sha256": digest(path),
                    "size": path.stat().st_size}
        if assets[name] != expected:
            raise ValueError("Snapshot binary differs from its manifest: " + name)
    checksums = "".join(f"{digest(directory / name)}  {name}\n"
                        for name in sorted(set(BINARIES) | {MANIFEST}))
    if (directory / CHECKSUMS).read_text() != checksums:
        raise ValueError("Snapshot checksums differ from the bundle")
    return manifest


def build(directory, allow_dirty=False):
    identity = source(allow_dirty)
    directory.parent.mkdir(parents=True, exist_ok=True)
    if directory.exists():
        raise ValueError("Snapshot output already exists; select a new output directory")
    with tempfile.TemporaryDirectory(prefix="tailrelay-snapshot-", dir=directory.parent) as tmp:
        stage = Path(tmp) / "bundle"
        run("make", "linux", f"OUT={stage}", f"VERSION={identity['version']}",
            f"COMMIT={identity['commit']}", cwd=ROOT, capture=False)
        assemble(stage, identity)
        verify(stage, identity)
        shutil.move(stage, directory)
    print(f"Built {identity['tag']}: {directory}")


class GitHub:
    def __init__(self, repository):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("Invalid GitHub repository")
        self.repository = repository
        self.token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
        if not self.token:
            raise ValueError("GH_TOKEN is required for snapshot publication")

    def api(self, path, method="GET", data=None, missing_ok=False):
        url = f"https://api.github.com/repos/{self.repository}"
        if path:
            url += "/" + path
        request = Request(url,
                          data=None if data is None else json.dumps(data).encode(),
                          method=method, headers={
                              "Authorization": "Bearer " + self.token,
                              "Accept": "application/vnd.github+json",
                              "Content-Type": "application/json",
                              "X-GitHub-Api-Version": "2022-11-28",
                              "User-Agent": "tailrelay-snapshot",
                          })
        try:
            with urlopen(request, timeout=60) as response:
                return json.load(response)
        except HTTPError as error:
            status = error.code
            error.close()
            if status == 404 and missing_ok:
                return None
            raise ValueError(f"GitHub {method} {path} failed with HTTP {status}") from None

    def find_release(self, tag):
        release = self.api("releases/tags/" + tag, missing_ok=True)
        if release is not None:
            return release
        # The tag endpoint need not expose drafts. The authenticated release
        # listing includes them, including a partially uploaded earlier attempt.
        page = 1
        while True:
            rows = self.api(f"releases?per_page=100&page={page}")
            matches = [row for row in rows if row["tag_name"] == tag]
            if len(matches) > 1:
                raise ValueError("Multiple releases use the snapshot tag")
            if matches:
                return matches[0]
            if len(rows) < 100:
                return None
            page += 1

    def download(self, release, directory):
        # Asset IDs work for authenticated draft downloads as well as published
        # releases, without a second lookup through the published-tag endpoint.
        for asset in release["assets"]:
            with (directory / asset["name"]).open("xb") as output:
                subprocess.run(["gh", "api", f"repos/{self.repository}/releases/assets/{asset['id']}",
                                "-H", "Accept: application/octet-stream"],
                               check=True, stdout=output, stderr=subprocess.PIPE)

    def upload(self, tag, paths):
        run("gh", "release", "upload", tag, "--repo", self.repository, *paths)


def check_tag(github, identity, create=False):
    tag = identity["tag"]
    observed = github.api("git/ref/tags/" + tag, missing_ok=True)
    if observed is None and create:
        observed = github.api("git/refs", "POST",
                              {"ref": "refs/tags/" + tag, "sha": identity["commit"]})
    if observed is None:
        raise ValueError("Snapshot tag is missing")
    obj = observed.get("object", {})
    if obj.get("type") != "commit" or obj.get("sha") != identity["commit"]:
        raise ValueError("Snapshot tag points at a different source; refusing to move it")


def verify_remote(github, release, hashes, complete):
    names = [asset["name"] for asset in release["assets"]]
    if (len(names) != len(set(names)) or not set(names) <= hashes.keys() or
            (complete and set(names) != hashes.keys())):
        raise ValueError("Existing snapshot has unexpected or missing assets")
    if names:
        with tempfile.TemporaryDirectory(prefix="tailrelay-snapshot-verify-") as tmp:
            directory = Path(tmp)
            github.download(release, directory)
            if {p.name for p in directory.iterdir()} != set(names):
                raise ValueError("Downloaded snapshot inventory differs")
            for name in names:
                if (directory / name).is_symlink() or digest(directory / name) != hashes[name]:
                    raise ValueError("Existing snapshot bytes differ; refusing to replace " + name)
    return set(names)


def publish(directory, repository, github=None):
    identity = source()
    manifest = verify(directory, identity)
    github = github or GitHub(repository)
    metadata = github.api("")
    if metadata.get("full_name", "").lower() != repository.lower():
        raise ValueError("Snapshots must be published to the selected GitHub repository")
    tag, commit = manifest["tag"], manifest["commit"]
    hashes = {name: digest(directory / name) for name in ASSETS}
    marker = f"<!-- tailrelay-snapshot commit={commit} -->"
    release = github.find_release(tag)
    if release is not None:
        if (release["tag_name"] != tag or release["prerelease"] is not True or
                marker not in (release.get("body") or "")):
            raise ValueError("Existing release is not owned by this source snapshot")
    check_tag(github, identity, create=release is None)
    if release is None:
        release = github.api("releases", "POST", {
            "tag_name": tag, "target_commitish": commit, "draft": True, "prerelease": True,
            "make_latest": "false", "name": "TailRelay snapshot " + manifest["version"],
            "body": (f"TailRelay development snapshot from `{commit}`.\n\n"
                     "Linux amd64 and arm64 static executables. Verify SHA256SUMS and "
                     "release-manifest.json before installing. This snapshot is separate "
                     "from the versioned APT channel; publishing does not deploy services.\n\n" + marker),
        })
    present = verify_remote(github, release, hashes, complete=not release["draft"])
    if release["draft"]:
        missing = sorted(ASSETS - present)
        if missing:
            # Never use --clobber: a partial upload remains a resumable draft.
            github.upload(tag, [directory / name for name in missing])
        release = github.api(f"releases/{release['id']}")
        verify_remote(github, release, hashes, complete=True)
        check_tag(github, identity)
        release = github.api(f"releases/{release['id']}", "PATCH",
                             {"draft": False, "prerelease": True, "make_latest": "false"})
    check_tag(github, identity)
    if release["draft"] or release["prerelease"] is not True:
        raise ValueError("Snapshot publication did not complete")
    print(f"Verified snapshot: {release['html_url']}")
    if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(summary, "a") as stream:
            stream.write(f"### TailRelay snapshot\n\n"
                         f"- Source: `{commit}`\n"
                         f"- Version: `{manifest['version']}`\n"
                         f"- Release: {release['html_url']}\n"
                         "- Linux amd64 and arm64; all downloaded assets verified.\n")
    return release


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("build", "verify", "publish"):
        command = sub.add_parser(name)
        command.add_argument("--directory", type=Path, default=ROOT / "bin/snapshot")
    sub.choices["build"].add_argument("--allow-dirty", action="store_true",
                                     help="local validation only; cannot publish")
    sub.choices["publish"].add_argument("--repository", required=True)
    args = parser.parse_args()
    directory = args.directory.resolve()
    if args.command == "build":
        build(directory, args.allow_dirty)
    elif args.command == "verify":
        verify(directory, source())
        print("Verified snapshot bundle")
    else:
        publish(directory, args.repository)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"TailRelay snapshot failed: {error}", file=sys.stderr)
        raise SystemExit(1)
