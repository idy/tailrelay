#!/usr/bin/env python3
"""Collect the license notices of modules compiled for a release platform."""
import argparse
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def command(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True)


def collect(goos, goarch, output):
    environment = dict(os.environ, GOWORK="off", GOOS=goos, GOARCH=goarch,
                       CGO_ENABLED="1" if goos == "darwin" else "0")
    go = environment.get("GO", "go")
    stream = command(go, "list", "-mod=readonly", "-deps", "-json", "-tags", "with_gvisor", "./cmd/...", env=environment)
    decoder, modules, position = json.JSONDecoder(), {}, 0
    while position < len(stream):
        if stream[position].isspace():
            position += 1
            continue
        package, position = decoder.raw_decode(stream, position)
        module = package.get("Module")
        if module and not module.get("Main"):
            module = module.get("Replace", module)
            modules[module["Path"]] = module
    goroot = Path(command(go, "env", "GOROOT", env=environment).strip())
    # Homebrew may keep the SDK license beside libexec rather than in GOROOT.
    go_license = next((path for path in (goroot / "LICENSE", goroot.parent / "LICENSE",
                                         ROOT / "LICENSES/go.txt") if path.is_file()), None)
    if go_license is None:
        raise ValueError("No Go standard library license found")
    sections = ["# Third-party notices", "", (ROOT / "THIRD_PARTY_NOTICES.md").read_text(),
                "## Go standard library", "", go_license.read_text()]
    for name, module in sorted(modules.items()):
        directory = Path(module["Dir"])
        notices = sorted(path for path in directory.iterdir()
                         if path.is_file() and path.name.lower().startswith(
                             ("license", "licence", "copying", "notice", "authors", "patents")))
        if not notices:
            raise ValueError("No upstream license notice found for " + name)
        sections += ["", f"## {name} {module.get('Version', '')}", ""]
        for path in notices:
            sections += [f"### {path.name}", "", path.read_text(errors="replace"), ""]
    data = ROOT / "internal/gfwlistdns/data"
    if data.is_dir():
        sections += ["## Embedded GFWList", "", (data / "README.md").read_text(),
                     "", (data / "COPYING.txt").read_text()]
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("\n".join(sections).rstrip() + "\n")
    print(f"Collected notices for {len(modules)} modules: {output}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--os", choices=("linux", "darwin"), required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    collect(args.os, args.arch, args.output)


if __name__ == "__main__":
    main()
