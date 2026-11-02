"""Execute the standalone installer with isolated releases and mocked system tools."""

import hashlib
import io
import json
import os
from pathlib import Path
import plistlib
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile

PROJECT = "tailrelay"
ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "install.sh"
VERSION = "1.2.3"
BINARY = b"fixture executable; never run\n"

# Executable tools live separately from release fixtures and extracted archives.
# PATH contains only this directory: actual sudo, apt, and network cannot run.
MOCK_TOOL = r'''
import hashlib, json, os, pathlib, plistlib, shutil, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ["TEST_ROOT"]).resolve()
with open(os.environ["MOCK_EVENTS"], "a") as log:
    log.write(json.dumps([name, args]) + "\n")
def local(value):
    p = pathlib.Path(value).resolve()
    if p != root and root not in p.parents:
        raise SystemExit("Mock refused a write outside the test directory: " + str(p))
    return p
if name == "curl":
    assert args[0] == "-q", "curlrc must be disabled"
    assert args[args.index("--proto") + 1] == "=https"
    assert args[args.index("--proto-redir") + 1] == "=https"
    assert not any(x in args for x in ("-H", "--header", "--user", "-u"))
    url = args[-1]
    assert url.startswith("https://github.com/idy/"), url
    if url.endswith("/releases/latest"):
        assert args[args.index("--write-out") + 1] == "%{url_effective}"
        print(os.environ["MOCK_LATEST"], end="")
    else:
        source = root / "release-fixtures" / url.rsplit("/", 1)[1]
        if not source.is_file():
            raise SystemExit(22)
        shutil.copyfile(source, local(args[args.index("--output") + 1]))
elif name == "uname":
    print(os.environ["MOCK_OS"] if args == ["-s"] else os.environ["MOCK_ARCH"])
elif name == "id":
    assert args == ["-u"]
    print(os.environ.get("MOCK_UID", "1000"))
elif name == "sudo":
    assert args[0] in ("apt-get", "dpkg", "install", "mktemp", "mv", "rm", "ditto", "cp")
    raise SystemExit(subprocess.run(args, env=os.environ).returncode)
elif name in ("apt-get", "dpkg"):
    assert (args[:3] == ["install", "-y", "--no-install-recommends"] if name == "apt-get" else args[:1] == ["-i"])
    assert local(args[-1]).is_file() and args[-1].endswith(".deb")
elif name == "install":
    is_dir = "-d" in args
    mode_at = args.index("-m")
    mode = int(args[mode_at + 1], 8)
    paths = args[mode_at + 2:]
    if is_dir:
        assert len(paths) == 1
        target = local(paths[0])
        target.mkdir(parents=True, exist_ok=True)
    else:
        assert len(paths) == 2
        target = local(paths[1])
        shutil.copyfile(local(paths[0]), target)
    target.chmod(mode)
elif name == "sha256sum":
    assert not args
    print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest() + "  -")
elif name == "plutil":
    assert args[:1] == ["-extract"] and args[2:5] == ["raw", "-o", "-"]
    with open(local(args[-1]), "rb") as stream:
        print(plistlib.load(stream)[args[1]])
elif name == "ps":
    assert args == ["-axo", "command="]
    print(os.environ.get("MOCK_PS", ""), end="")
elif name == "ditto":
    if args[:2] == ["-x", "-k"]:
        # Native unzip preserves executable permissions in the fixture ZIP.
        real = json.loads(os.environ["REAL_TOOLS"])["unzip"]
        raise SystemExit(subprocess.run([real, "-q", str(local(args[2])), "-d", str(local(args[3]))]).returncode)
    assert len(args) == 2
    shutil.copytree(local(args[0]), local(args[1]), copy_function=shutil.copy2)
else:
    real = json.loads(os.environ["REAL_TOOLS"])
    if name not in real:
        raise SystemExit("Forbidden system mutation: " + name)
    if name in ("rm", "mv", "cp", "mkdir", "mktemp"):
        for arg in args:
            if not arg.startswith("-"):
                local(arg)
    if (name == "mv" and os.environ.get("MOCK_FAIL_ACTIVATE") == "1"
            and args[0].endswith("/TailSplit.app") and "/.tailsplit-install." in args[0]):
        raise SystemExit("Simulated app activation failure")
    os.execv(real[name], [real[name], *args])
'''


class InstallerSandbox(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix=f"{PROJECT}-installer-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.tools = self.root / "mock-bin"
        self.fixtures = self.root / "release-fixtures"
        self.home = self.root / "home with spaces"
        self.runtime = self.root / "runtime"
        self.cwd = self.root / "invocation"
        for directory in (self.tools, self.fixtures, self.home, self.runtime, self.cwd):
            directory.mkdir()
        self.events_file = self.root / "events.jsonl"
        self.events_file.touch()
        real_names = ("awk", "grep", "tar", "gzip", "unzip", "shasum", "find", "mktemp", "rm", "mkdir", "mv", "cp")
        real = {name: shutil.which(name) for name in real_names}
        self.assertTrue(all(real.values()), real)
        mocked = ("curl", "uname", "id", "sudo", "apt-get", "dpkg", "install", "sha256sum", "plutil", "ps", "ditto")
        forbidden = ("xattr", "kill", "killall", "pkill", "open", "launchctl", "systemctl", "service", "ip", "route", "networksetup", "tailscale")
        for name in (*real_names, *mocked, *forbidden):
            path = self.tools / name
            path.write_text(f"#!{sys.executable} -I\n" + MOCK_TOOL)
            path.chmod(0o755)
        self.env = {
            "PATH": str(self.tools), "HOME": str(self.home), "TMPDIR": str(self.runtime),
            "LC_ALL": "C", "TEST_ROOT": str(self.root), "MOCK_EVENTS": str(self.events_file),
            "REAL_TOOLS": json.dumps(real), "MOCK_OS": "Linux", "MOCK_ARCH": "x86_64",
            "MOCK_LATEST": f"https://github.com/idy/{PROJECT}/releases/tag/v{VERSION}",
        }
        self.prefix = self.root / "custom prefix"

    def run_installer(self, *args, stdin=False):
        if stdin:
            command = ["/bin/sh", "-s", "--", *map(str, args)]
            source = SCRIPT.read_text()
        else:
            command = ["/bin/sh", str(SCRIPT), *map(str, args)]
            source = None
        result = subprocess.run(command, input=source, text=True, capture_output=True,
                                cwd=self.cwd, env=self.env, timeout=30)
        self.assertEqual(list(self.runtime.iterdir()), [], "download/extraction temporary directory leaked")
        return result

    def events(self, name=None):
        events = [json.loads(line) for line in self.events_file.read_text().splitlines()]
        return [args for tool, args in events if tool == name] if name else events

    def assert_success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_no_install(self):
        for tool in ("install", "sudo", "apt-get", "dpkg", "ditto"):
            self.assertEqual(self.events(tool), [], tool)

    def checksums(self, asset, text=None):
        if text is None:
            text = f"{hashlib.sha256((self.fixtures / asset).read_bytes()).hexdigest()}  {asset}\n"
        (self.fixtures / "SHA256SUMS").write_text(text)

    def cli_asset(self, goos="linux", arch="amd64", version=VERSION, extra=None, link=None, duplicate=False):
        name = f"{PROJECT}_{version}_{goos}_{arch}.tar.gz"
        entries = [(PROJECT, BINARY), ("LICENSE", b"license\n"),
                   ("THIRD_PARTY_NOTICES.md", b"notices\n"), ("build.json", b"{}\n")]
        if extra:
            entries.append((extra, b"must not escape\n"))
        if duplicate:
            entries.append((PROJECT, BINARY))
        with tarfile.open(self.fixtures / name, "w:gz", format=tarfile.USTAR_FORMAT) as archive:
            for entry, content in entries:
                info = tarfile.TarInfo(entry)
                info.mode = 0o755 if entry == PROJECT else 0o644
                if link and entry == PROJECT:
                    info.type = tarfile.SYMTYPE if link == "symlink" else tarfile.LNKTYPE
                    info.linkname = "LICENSE"
                    archive.addfile(info)
                else:
                    info.size = len(content)
                    archive.addfile(info, io.BytesIO(content))
        self.checksums(name)
        return name

    def deb_asset(self, arch="amd64"):
        name = f"{PROJECT}_{VERSION}_linux_{arch}.deb"
        (self.fixtures / name).write_bytes(b"mock Debian package; never pass to a real package manager\n")
        self.checksums(name)
        return name

    def assert_selected(self, name, version=VERSION):
        urls = [args[-1] for args in self.events("curl")]
        self.assertIn(f"https://github.com/idy/{PROJECT}/releases/download/v{version}/{name}", urls)
        self.assertIn(f"https://github.com/idy/{PROJECT}/releases/download/v{version}/SHA256SUMS", urls)


class CLIInstallerTests(InstallerSandbox):
    def test_platform_architecture_matrix_and_explicit_prefix(self):
        for os_name, machine, goos, arch in (
            ("Linux", "x86_64", "linux", "amd64"),
            ("Linux", "aarch64", "linux", "arm64"),
            ("Linux", "arm64", "linux", "arm64"),
            ("Darwin", "arm64", "darwin", "arm64"),
            ("Darwin", "x86_64", "darwin", "amd64"),
        ):
            with self.subTest(os=os_name, machine=machine):
                self.events_file.write_text("")
                self.env.update(MOCK_OS=os_name, MOCK_ARCH=machine)
                name = self.cli_asset(goos, arch)
                result = self.run_installer("--version", f"v{VERSION}", "--prefix", self.prefix)
                self.assert_success(result)
                self.assert_selected(name)
                executable = self.prefix / "bin" / PROJECT
                self.assertEqual(executable.read_bytes(), BINARY)
                self.assertEqual(stat.S_IMODE(executable.stat().st_mode), 0o755)
                self.assertEqual(self.events("sudo"), [])
                self.assertEqual(self.events("apt-get"), [])

    def test_latest_redirect_and_piped_sh_with_no_sudo_default(self):
        name = self.cli_asset()
        result = self.run_installer("--no-sudo", stdin=True)
        self.assert_success(result)
        self.assert_selected(name)
        self.assertEqual(self.events("curl")[0][-1], f"https://github.com/idy/{PROJECT}/releases/latest")
        self.assertEqual((self.home / ".local/bin" / PROJECT).read_bytes(), BINARY)
        self.assertEqual(self.events("sudo"), [])
        self.assertEqual(self.events("apt-get"), [])
        self.assertEqual(self.events("dpkg"), [])

    def test_unprefixed_prerelease_version_and_binary_method(self):
        version = "2.3.4-rc.1+build.7"
        name = self.cli_asset(version=version)
        self.assert_success(self.run_installer("--version", version, "--method", "binary", "--no-sudo"))
        self.assert_selected(name, version)
        self.assertFalse(any(args[-1].endswith("/latest") for args in self.events("curl")))

    def test_custom_prefix_is_respected_with_no_sudo(self):
        self.cli_asset()
        self.assert_success(self.run_installer("--version", VERSION, "--no-sudo", "--prefix", self.prefix))
        self.assertTrue((self.prefix / "bin" / PROJECT).is_file())
        self.assertFalse((self.home / ".local").exists())
        self.assertEqual(self.events("sudo"), [])

    def test_no_sudo_refuses_unwritable_prefix_without_elevation(self):
        self.cli_asset()
        self.prefix.mkdir()
        self.prefix.chmod(0o555)
        self.addCleanup(self.prefix.chmod, 0o755)
        if os.access(self.prefix, os.W_OK):
            self.skipTest("The test process can write read-only directories (for example, root).")
        result = self.run_installer("--version", VERSION, "--no-sudo", "--prefix", self.prefix)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Destination is not writable", result.stderr)
        self.assertFalse((self.prefix / "bin").exists())
        self.assert_no_install()

    def test_relative_prefix_with_spaces(self):
        self.cli_asset()
        self.assert_success(self.run_installer("--version", VERSION, "--prefix", "relative prefix"))
        self.assertTrue((self.cwd / "relative prefix/bin" / PROJECT).is_file())

    def test_linux_auto_prefers_apt_local_deb_with_dependencies(self):
        name = self.deb_asset()
        self.assert_success(self.run_installer("--version", VERSION))
        self.assert_selected(name)
        args = self.events("apt-get")[0]
        self.assertEqual(args[:3], ["install", "-y", "--no-install-recommends"])
        self.assertTrue(Path(args[-1]).is_absolute())
        self.assertEqual(self.events("sudo")[0][0], "apt-get")
        self.assertEqual(self.events("dpkg"), [])
        tools = [name for name, _ in self.events()]
        self.assertLess(tools.index("sha256sum"), tools.index("sudo"))
        self.assertFalse(any(name in tools for name in ("systemctl", "service", "ip", "route")))

    def test_linux_arm64_dpkg_fallback(self):
        (self.tools / "apt-get").unlink()
        self.env["MOCK_ARCH"] = "aarch64"
        name = self.deb_asset("arm64")
        self.assert_success(self.run_installer("--version", VERSION, "--method", "deb"))
        self.assert_selected(name)
        self.assertEqual(self.events("dpkg")[0][0], "-i")
        self.assertEqual(self.events("sudo")[0][0], "dpkg")

    def test_root_package_install_does_not_invoke_sudo(self):
        self.env["MOCK_UID"] = "0"
        self.deb_asset()
        self.assert_success(self.run_installer("--version", VERSION))
        self.assertEqual(len(self.events("apt-get")), 1)
        self.assertEqual(self.events("sudo"), [])

    def test_missing_sudo_explains_user_install_remedy(self):
        (self.tools / "sudo").unlink()
        self.deb_asset()
        result = self.run_installer("--version", VERSION)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--no-sudo --prefix "$HOME/.local"', result.stderr)
        self.assertEqual(self.events("apt-get"), [])

    def test_no_sudo_rejects_explicit_deb_even_for_root(self):
        self.env["MOCK_UID"] = "0"
        result = self.run_installer("--version", VERSION, "--method", "deb", "--no-sudo")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--no-sudo cannot install packages", result.stderr)
        self.assertEqual(self.events("curl"), [])
        self.assert_no_install()

    def test_deb_rejects_custom_prefix(self):
        result = self.run_installer("--version", VERSION, "--method", "deb", "--prefix", self.prefix)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--prefix selects a binary install", result.stderr)
        self.assert_no_install()

    def test_checksum_mismatch_blocks_every_install(self):
        for method in ("binary", "deb"):
            with self.subTest(method=method):
                self.events_file.write_text("")
                name = self.cli_asset() if method == "binary" else self.deb_asset()
                self.checksums(name, "0" * 64 + f"  {name}\n")
                args = ["--method", method, "--version", VERSION]
                if method == "binary":
                    args += ["--prefix", str(self.prefix)]
                result = self.run_installer(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("SHA256 mismatch", result.stderr)
                self.assert_no_install()
                self.assertEqual(self.events("tar"), [])

    def test_missing_malformed_and_ambiguous_checksums(self):
        name = self.cli_asset()
        digest = hashlib.sha256((self.fixtures / name).read_bytes()).hexdigest()
        for checksums in (
            digest + "  another-asset.tar.gz\n",
            "abc123  " + name + "\n",
            "g" * 64 + "  " + name + "\n",
            digest + "  " + name + " extra\n",
            digest + "  " + name + "\n" + digest + " *" + name + "\n",
        ):
            with self.subTest(checksums=checksums):
                self.events_file.write_text("")
                self.checksums(name, checksums)
                result = self.run_installer("--version", VERSION, "--prefix", self.prefix)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("exactly one valid SHA256", result.stderr)
                self.assert_no_install()
                self.assertEqual(self.events("tar"), [])

    def test_standard_binary_mode_checksum_and_shasum_fallback(self):
        (self.tools / "sha256sum").unlink()
        name = self.cli_asset()
        digest = hashlib.sha256((self.fixtures / name).read_bytes()).hexdigest().upper()
        self.checksums(name, digest + " *" + name + "\n")
        self.assert_success(self.run_installer("--version", VERSION, "--prefix", self.prefix))
        self.assertEqual(self.events("shasum"), [["-a", "256"]])

    def test_missing_sha_tool_fails_before_download(self):
        for name in ("sha256sum", "shasum"):
            (self.tools / name).unlink()
        result = self.run_installer("--version", VERSION, "--prefix", self.prefix)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA256 verification requires", result.stderr)
        self.assertEqual(self.events("curl"), [])
        self.assert_no_install()

    def test_unsafe_archive_names_duplicates_and_links_are_rejected_before_extracting(self):
        cases = (
            {"extra": "../escaped"}, {"extra": str(self.root / "escaped")},
            {"extra": "unexpected-file"}, {"duplicate": True},
            {"link": "symlink"}, {"link": "hardlink"},
        )
        for fixture in cases:
            with self.subTest(fixture=fixture):
                self.events_file.write_text("")
                self.cli_asset(**fixture)
                result = self.run_installer("--version", VERSION, "--prefix", self.prefix)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("archive", result.stderr)
                self.assert_no_install()
                self.assertFalse(any(args[0] == "-xzf" for args in self.events("tar")))
                self.assertFalse((self.root / "escaped").exists())

    def test_existing_destination_symlink_is_not_followed(self):
        self.cli_asset()
        (self.prefix / "bin").mkdir(parents=True)
        other = self.root / "other executable"
        other.write_bytes(b"keep")
        (self.prefix / "bin" / PROJECT).symlink_to(other)
        result = self.run_installer("--version", VERSION, "--prefix", self.prefix)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Refusing to overwrite a link", result.stderr)
        self.assertEqual(other.read_bytes(), b"keep")
        self.assert_no_install()

    def test_unsupported_platforms_and_architectures(self):
        for os_name, arch in (("FreeBSD", "x86_64"), ("Linux", "i686"), ("Darwin", "ppc64")):
            with self.subTest(os=os_name, arch=arch):
                self.env.update(MOCK_OS=os_name, MOCK_ARCH=arch)
                result = self.run_installer("--version", VERSION, "--prefix", self.prefix)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Unsupported", result.stderr)
                self.assertEqual(self.events("curl"), [])

    def test_invalid_versions_cannot_become_urls_or_commands(self):
        for version in ("../1.2.3", "1.2.3/evil", "1.2", "v1.2.3;touch injected", "$(touch injected)", "1.2.3\n4.5.6", "1.2.3?x=y"):
            with self.subTest(version=version):
                result = self.run_installer("--version", version, "--prefix", self.prefix)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Invalid version", result.stderr)
                self.assertEqual(self.events("curl"), [])
                self.assertFalse((self.cwd / "injected").exists())

    def test_latest_redirect_must_be_the_selected_public_repo_and_version(self):
        for url in ("https://example.com/releases/tag/v1.2.3",
                    "https://github.com/idy/other/releases/tag/v1.2.3",
                    f"https://github.com/idy/{PROJECT}/releases/latest",
                    f"https://github.com/idy/{PROJECT}/releases/tag/v1.2.3/evil"):
            with self.subTest(url=url):
                self.events_file.write_text("")
                self.env["MOCK_LATEST"] = url
                result = self.run_installer("--no-sudo")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("redirect", result.stderr)
                self.assertEqual(len(self.events("curl")), 1)
                self.assert_no_install()

    def test_help_and_argument_errors_do_not_download(self):
        self.assert_success(self.run_installer("--help"))
        for args in (("--version",), ("--prefix", ""), ("--method", "rpm"), ("--unknown",)):
            with self.subTest(args=args):
                self.assertNotEqual(self.run_installer(*args).returncode, 0)
        self.assertEqual(self.events("curl"), [])



if __name__ == "__main__":
    unittest.main()
