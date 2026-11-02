#!/usr/bin/env python3
"""Build, assemble and publish complete TailRelay multi-platform releases."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import quote, urlparse

ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = {(osname, arch) for osname in ("linux", "darwin") for arch in ("amd64", "arm64")}


def run(*args, env=None):
    return subprocess.check_output(list(map(str, args)), cwd=ROOT, env=env, text=True).strip()


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def metadata(tag=None, allow_dirty=False):
    module = (ROOT / "go.mod").read_text().splitlines()[0].split()[1]
    if module != "github.com/idy/tailrelay":
        raise ValueError("Unexpected TailRelay source module")
    version = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("VERSION must be major.minor.patch")
    if tag is not None and tag != "v" + version:
        raise ValueError("Release tag must match the committed VERSION")
    commit = run("git", "rev-parse", "HEAD")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Release checkout must have a full source commit")
    if os.environ.get("GITHUB_ACTIONS") == "true" and tag:
        if run("git", "rev-parse", "refs/tags/" + tag + "^{commit}") != commit:
            raise ValueError("Release checkout must match the selected tag")
    dirty = bool(run("git", "status", "--porcelain", "--untracked-files=normal"))
    if dirty and not allow_dirty:
        raise ValueError("Commit release inputs first; --allow-dirty is for local validation only")
    repository = os.environ.get("GITHUB_REPOSITORY", "idy/tailrelay")
    if repository != "idy/tailrelay":
        raise ValueError("Unexpected TailRelay release repository")
    return {"schema_version": 1, "project": "tailrelay", "version": version,
            "tag": "v" + version, "commit": commit, "repository": repository, "dirty": dirty}


def expected_assets(project, version):
    names = {f"{project}_{version}_{goos}_{arch}.tar.gz" for goos, arch in PLATFORMS}
    return names | {f"{project}_{version}_linux_{arch}.deb" for arch in ("amd64", "arm64")}


def archive(path, files, epoch):
    with path.open("xb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch) as zipped:
        with tarfile.open(fileobj=zipped, mode="w") as bundle:
            for name, source in files.items():
                data = source.read_bytes()
                info = tarfile.TarInfo(name)
                info.size, info.mtime = len(data), epoch
                info.mode = 0o755 if name == "tailrelay" else 0o644
                info.uid = info.gid = 0
                info.uname = info.gname = "root"
                bundle.addfile(info, io.BytesIO(data))


def build(args):
    identity = metadata(args.tag, args.allow_dirty)
    project, version = identity["project"], identity["version"]
    output = args.directory.resolve()
    output.mkdir(parents=True, exist_ok=True)
    work = ROOT / "bin/release/work" / (args.os + "-" + args.arch)
    work.mkdir(parents=True, exist_ok=True)
    environment = dict(os.environ, GOWORK="off", GOOS=args.os, GOARCH=args.arch,
                       CGO_ENABLED="1" if args.os == "darwin" else "0")
    if args.arch == "amd64":
        environment["GOAMD64"] = "v1"
    else:
        environment["GOARM64"] = "v8.0"
    environment["RELEASE_SOURCE_COMMIT"] = identity["commit"]
    notices = work / "THIRD_PARTY_NOTICES.md"
    subprocess.run([sys.executable, "-I", "-B", str(ROOT / "packaging/licenses.py"),
                    "--os", args.os, "--arch", args.arch, "--output", str(notices)],
                   cwd=ROOT, env=environment, check=True)
    binary = work / project
    flags = f"-s -w -X main.version={version} -X main.commit={identity['commit']}"
    subprocess.run([environment.get("GO", "go"), "build", "-mod=readonly", "-tags", "with_gvisor",
                    "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", str(binary), "./cmd/" + project],
                   cwd=ROOT, env=environment, check=True)
    detail = dict(identity, os=args.os, arch=args.arch)
    build_info = work / "build.json"
    build_info.write_text(json.dumps(detail, indent=2, sort_keys=True) + "\n")
    tarball = output / f"{project}_{version}_{args.os}_{args.arch}.tar.gz"
    archive(tarball, {project: binary, "LICENSE": ROOT / "LICENSE",
                     "THIRD_PARTY_NOTICES.md": notices, "build.json": build_info},
            int(run("git", "show", "-s", "--format=%ct", "HEAD")))
    files = [tarball]
    if args.os == "linux":
        environment["THIRD_PARTY_NOTICES_FILE"] = str(notices)
        subprocess.run(["make", "deb", "GO=" + environment.get("GO", "go"), "PYTHON=" + sys.executable],
                       cwd=ROOT, env=environment, check=True)
        source = ROOT / "bin/deb" / args.arch / f"{project}_{version}_{args.arch}.deb"
        target = output / f"{project}_{version}_linux_{args.arch}.deb"
        with source.open("rb") as incoming, target.open("xb") as outgoing:
            shutil.copyfileobj(incoming, outgoing)
        files.append(target)
    fragment = dict(detail, assets={path.name: {"sha256": digest(path), "size": path.stat().st_size}
                                   for path in files})
    (output / f"release-metadata-{args.os}-{args.arch}.json").write_text(
        json.dumps(fragment, indent=2, sort_keys=True) + "\n")
    print("Built", identity["tag"], args.os, args.arch, "assets:", len(files))


def finalize(directory, tag):
    identity = metadata(tag)
    fragments = [json.loads(path.read_text()) for path in sorted(directory.glob("release-metadata-*.json"))]
    if {(item["os"], item["arch"]) for item in fragments} != PLATFORMS or len(fragments) != len(PLATFORMS):
        raise ValueError("A complete Linux/macOS amd64/arm64 build is required")
    assets = {}
    for item in fragments:
        if any(item.get(key) != value for key, value in identity.items()):
            raise ValueError("Platform artifacts differ from the release source")
        for name, proof in item["assets"].items():
            if name in assets or not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
                raise ValueError("Duplicate or invalid release asset")
            path = directory / name
            if path.is_symlink() or not path.is_file() or path.stat().st_size != proof["size"] or digest(path) != proof["sha256"]:
                raise ValueError("Release artifact hash or size differs: " + name)
            assets[name] = proof
    if set(assets) != expected_assets(identity["project"], identity["version"]):
        raise ValueError("Release assets do not match the complete expected inventory")
    manifest = dict(identity, assets=assets)
    (directory / "release-manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    names = sorted(assets) + ["release-manifest.json"]
    (directory / "SHA256SUMS").write_text("".join(f"{digest(directory / name)}  {name}\n" for name in names))
    return manifest


def api(path, method="GET", data=None, input_file=None, accept=None, output=None):
    arguments = ["gh", "api", path, "--method", method, "-H", "X-GitHub-Api-Version: 2022-11-28"]
    if accept:
        arguments += ["-H", "Accept: " + accept]
    if data is not None:
        arguments += ["--input", "-"]
    if input_file:
        arguments += ["--input", str(input_file), "-H", "Content-Type: application/octet-stream"]
    result = subprocess.run(arguments, input=None if data is None else json.dumps(data).encode(),
                            stdout=output if output else subprocess.PIPE, stderr=subprocess.PIPE, check=True)
    return None if output else json.loads(result.stdout)


def find_release(repository, tag):
    page = 1
    while True:
        releases = api(f"repos/{repository}/releases?per_page=100&page={page}")
        matches = [release for release in releases if release["tag_name"] == tag]
        if len(matches) > 1:
            raise ValueError("Ambiguous release tag")
        if matches:
            return matches[0]
        if len(releases) < 100:
            return None
        page += 1


def inventory(release, selected):
    existing = {asset["name"]: asset for asset in release["assets"]}
    if len(existing) != len(release["assets"]) or set(existing) - selected:
        raise ValueError("Existing release has unexpected or duplicate assets")
    if not release["draft"] and set(existing) != selected:
        raise ValueError("A published release cannot be filled or overwritten")
    return existing


def verify_assets(repository, assets, directory):
    with tempfile.TemporaryDirectory(prefix="release-verify-") as temporary:
        downloaded = Path(temporary) / "asset"
        for asset in assets:
            with downloaded.open("wb") as output:
                api(f"repos/{repository}/releases/assets/{asset['id']}", accept="application/octet-stream", output=output)
            if digest(downloaded) != digest(directory / asset["name"]):
                raise ValueError("Existing or uploaded release bytes differ: " + asset["name"])


def publish(directory, tag):
    manifest = finalize(directory, tag)
    repository, commit = manifest["repository"], manifest["commit"]
    if api(f"repos/{repository}/commits/{quote(tag, safe='')}")["sha"] != commit:
        raise ValueError("Remote tag does not match the release source")
    marker = f"<!-- tailrelay-release commit={commit} -->"
    release = find_release(repository, tag)
    if release is None:
        body = (f"TailRelay {manifest['version']} from [`{commit}`](https://github.com/{repository}/tree/{commit}).\n\n"
                "Linux and macOS CLI archives support amd64 and arm64. Linux Debian packages are included.\n"
                "Verify SHA256SUMS before installation. Each archive includes its license, dependency notices and source metadata.\n\n"
                "Install: `curl -fsSL https://raw.githubusercontent.com/idy/tailrelay/main/install.sh | sh`\n\n" + marker)
        release = api(f"repos/{repository}/releases", "POST", {
            "tag_name": tag, "target_commitish": commit, "draft": True, "prerelease": False,
            "name": "TailRelay " + tag, "body": body, "make_latest": "false"})
    if marker not in (release.get("body") or "") or release["tag_name"] != tag:
        raise ValueError("Existing release is not owned by this source commit")
    selected = set(manifest["assets"]) | {"release-manifest.json", "SHA256SUMS"}
    existing = inventory(release, selected)
    # Verify an interrupted draft before uploading anything else. Never replace bytes.
    verify_assets(repository, existing.values(), directory)
    upload_url = release["upload_url"].split("{", 1)[0]
    parsed = urlparse(upload_url)
    if parsed.scheme != "https" or parsed.netloc != "uploads.github.com":
        raise ValueError("Unexpected GitHub upload host")
    for name in sorted(selected - set(existing)):
        api(upload_url + "?name=" + quote(name, safe=""), "POST", input_file=directory / name)
    release = api(f"repos/{repository}/releases/{release['id']}")
    if set(inventory(release, selected)) != selected:
        raise ValueError("Uploaded release inventory is incomplete")
    verify_assets(repository, release["assets"], directory)
    if release["draft"]:
        release = api(f"repos/{repository}/releases/{release['id']}", "PATCH", {"draft": False, "make_latest": "true"})
    print("Published and verified:", release["html_url"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    builder = commands.add_parser("build")
    builder.add_argument("--os", choices=("linux", "darwin"), required=True)
    builder.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    builder.add_argument("--allow-dirty", action="store_true")
    for name in ("build", "finalize", "publish"):
        target = builder if name == "build" else commands.add_parser(name)
        target.add_argument("--directory", type=Path, default=ROOT / "bin/release/assets")
        target.add_argument("--tag", required=True)
    args = parser.parse_args()
    if args.command == "build":
        build(args)
    elif args.command == "finalize":
        finalize(args.directory, args.tag)
    else:
        publish(args.directory, args.tag)


if __name__ == "__main__":
    main()
