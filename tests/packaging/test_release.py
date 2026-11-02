import base64
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch
from urllib.error import HTTPError

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("tailrelay_apt", ROOT / "packaging/apt-release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class TailRelayPackagingTests(unittest.TestCase):
    def test_standalone_root_version_and_both_architectures(self):
        self.assertEqual(release.ROOT, ROOT)
        self.assertEqual(release.PACKAGES, ("tailrelay",))
        version = release.read_version()
        self.assertEqual(version, (ROOT / "VERSION").read_text().strip())
        self.assertEqual(set(release.expected_files(version)), {
            f"tailrelay_{version}_amd64.deb", f"tailrelay_{version}_arm64.deb",
        })

    def test_package_never_ships_operator_configuration_or_identity(self):
        build = (ROOT / "packaging/build-deb.sh").read_text()
        self.assertNotIn('"$root/etc/tailrelay', build)
        self.assertNotIn('"$root/var/lib/tailrelay', build)
        self.assertIn("-tags with_gvisor", build)
        self.assertIn("-mod=readonly", build)
        self.assertIn("./cmd/tailrelay", build)
        self.assertIn('"$root/usr/share/doc/tailrelay/build.json"', build)
        self.assertNotIn("../.github", (ROOT / "Makefile").read_text())

    def test_unit_and_package_binary_paths_agree(self):
        unit = (ROOT / "deploy/tailrelay@.service").read_text()
        self.assertIn("ExecStart=/usr/bin/tailrelay run", unit)
        self.assertIn("User=tailrelay-%i", unit)
        self.assertIn("StateDirectory=tailrelay/%i", unit)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.version = release.read_version()
        self.paths = {name: self.directory / name for name in release.expected_files(self.version)}
        for path in self.paths.values():
            path.write_bytes(path.name.encode())
        self.hashes = {name: release.sha256(path) for name, path in self.paths.items()}
        self.registry = Mock()
        local = patch.object(release, "local_packages", return_value=self.paths)
        self.local = local.start()
        self.addCleanup(local.stop)
        verify = patch.object(release, "verify_apt")
        self.verify = verify.start()
        self.addCleanup(verify.stop)

    def test_absent_version_uploads_both_architectures_then_verifies(self):
        self.registry.inventory.side_effect = [{}, self.hashes]
        release.publish(self.registry, self.directory, self.version)
        self.assertEqual(self.registry.upload.call_count, 2)
        self.verify.assert_called_once_with(self.registry, self.version, self.hashes)

    def test_existing_version_skips_even_when_new_commit_would_differ(self):
        existing = dict.fromkeys(self.hashes, "old-published-content")
        self.registry.inventory.return_value = existing
        release.publish(self.registry, self.directory, self.version)
        self.registry.upload.assert_not_called()
        self.verify.assert_called_once_with(self.registry, self.version, existing)

    def test_partial_retry_only_uploads_missing_architecture(self):
        existing = dict(list(self.hashes.items())[:1])
        self.registry.inventory.side_effect = [existing, self.hashes]
        release.publish(self.registry, self.directory, self.version)
        self.assertEqual({call.args[0].name for call in self.registry.upload.call_args_list},
                         self.hashes.keys() - existing.keys())

    def test_partial_conflict_fails_before_any_upload(self):
        self.registry.inventory.return_value = {next(iter(self.hashes)): "different bytes"}
        with self.assertRaisesRegex(ValueError, "Published bytes differ"):
            release.publish(self.registry, self.directory, self.version)
        self.registry.upload.assert_not_called()

    def test_registry_failure_does_not_mean_version_absent(self):
        self.registry.inventory.side_effect = subprocess.CalledProcessError(1, "gcloud")
        with self.assertRaises(subprocess.CalledProcessError):
            release.publish(self.registry, self.directory, self.version)
        self.registry.upload.assert_not_called()

    def test_failed_upload_can_be_retried_without_overwriting_first_package(self):
        self.registry.inventory.return_value = {}
        self.registry.upload.side_effect = [None, subprocess.CalledProcessError(1, "upload")]
        with self.assertRaises(subprocess.CalledProcessError):
            release.publish(self.registry, self.directory, self.version)
        first = self.registry.upload.call_args_list[0].args[0].name
        self.registry.reset_mock(side_effect=True)
        self.registry.inventory.side_effect = [{first: self.hashes[first]}, self.hashes]
        release.publish(self.registry, self.directory, self.version)
        self.assertNotIn(first, [call.args[0].name for call in self.registry.upload.call_args_list])

    def test_version_accepts_plain_release_and_rejects_unsafe_values(self):
        path = self.directory / "VERSION"
        for version in ("1:1.0.2", "1.0.2-0", "1.0.2\nother", "$(id)", "1.0.2/../x"):
            path.write_text(version)
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.read_version(path)
        for version in ("1.0.2", "1.2.3~rc1-2", "1.2.3+fix.1-1"):
            path.write_text(version + "\n")
            self.assertEqual(release.read_version(path), version)

    def test_default_version_output_uses_root_without_accessing_registry(self):
        output = self.directory / "github-output"
        output.write_text("prior=value\n")
        with patch.object(release.sys, "argv", ["apt-release.py", "version"]), \
                patch.object(release.sys, "stdout", io.StringIO()) as stdout, \
                patch.dict(release.os.environ, {"GITHUB_OUTPUT": str(output)}), \
                patch.object(release, "Registry") as registry:
            release.main()
        registry.assert_not_called()
        self.assertEqual(stdout.getvalue(), self.version + "\n")
        self.assertEqual(output.read_text(), f"prior=value\nversion={self.version}\n")

    def test_invalid_version_cannot_inject_github_action_outputs(self):
        version_file = self.directory / "VERSION"
        version_file.write_text("0.1.0\nmissing=false\n")
        output = self.directory / "github-output"
        arguments = ["apt-release.py", "version", "--version-file", str(version_file)]
        with patch.object(release.sys, "argv", arguments), \
                patch.dict(release.os.environ, {"GITHUB_OUTPUT": str(output)}), \
                patch.object(release, "Registry") as registry:
            with self.assertRaises(ValueError):
                release.main()
        registry.assert_not_called()
        self.assertFalse(output.exists())

    def test_publish_defaults_to_standalone_debian_output(self):
        with patch.object(release, "Registry", return_value=self.registry), \
                patch.object(release.sys, "argv", ["apt-release.py", "publish"]), \
                patch.object(release, "publish") as publish:
            release.main()
        publish.assert_called_once_with(self.registry, ROOT / "bin/deb", self.version)

    def test_inventory_reads_gcp_content_suffix_and_urlsafe_hashes(self):
        registry = release.Registry("project", "region", "repo")
        responses = []
        for package in release.PACKAGES:
            files = []
            for arch in release.ARCHITECTURES:
                key = f"{package}_{self.version}_{arch}.deb"
                files.append({
                    "name": f"{registry.parent}/files/pool%2F{key[:-4]}_{'a' * 32}.deb",
                    "owner": f"{registry.parent}/packages/{package}/versions/{self.version}",
                    "hashes": [{"type": "SHA256", "value": base64.urlsafe_b64encode(
                        bytes.fromhex(self.hashes[key])).decode()}],
                })
            responses.append(json.dumps(files))
        with patch.object(subprocess, "check_output", side_effect=responses):
            self.assertEqual(registry.inventory(self.version), self.hashes)

    def test_empty_registry_returns_missing_but_cli_errors_propagate(self):
        registry = release.Registry("project", "region", "repo")
        with patch.object(subprocess, "check_output", return_value="[]"):
            self.assertEqual(registry.inventory(self.version), {})
        with patch.object(subprocess, "check_output", side_effect=subprocess.CalledProcessError(1, "gcloud")):
            with self.assertRaises(subprocess.CalledProcessError):
                registry.inventory(self.version)

    def test_inventory_accepts_modern_hex_and_legacy_base64_hashes(self):
        registry = release.Registry("project", "region", "repo")
        key = f"tailrelay_{self.version}_amd64.deb"
        digest = self.hashes[key]
        item = {
            "name": f"{registry.parent}/files/pool%2F{key}",
            "owner": f"{registry.parent}/packages/tailrelay/versions/{self.version}",
        }
        for value in (digest, digest.upper(), base64.b64encode(bytes.fromhex(digest)).decode(),
                      base64.urlsafe_b64encode(bytes.fromhex(digest)).decode()):
            item["hashes"] = [{"type": "SHA256", "value": value}]
            with self.subTest(value=value), patch.object(subprocess, "check_output", return_value=json.dumps([item])):
                self.assertEqual(registry.inventory(self.version), {key: digest})
        for value in ("g" * 64, digest[:-1], "00", base64.b64encode(bytes(31)).decode()):
            item["hashes"] = [{"type": "SHA256", "value": value}]
            with self.subTest(value=value), patch.object(subprocess, "check_output", return_value=json.dumps([item])):
                with self.assertRaises(ValueError):
                    registry.inventory(self.version)

    def test_existing_release_check_verifies_downloads_without_publishing(self):
        with patch.object(release, "Registry", return_value=self.registry), \
                patch.object(release.sys, "argv", ["apt-release.py", "check"]):
            self.registry.inventory.return_value = self.hashes
            release.main()
        self.registry.upload.assert_not_called()
        self.verify.assert_called_once_with(self.registry, self.version, self.hashes)

    def test_duplicate_or_malformed_registry_content_is_not_absence(self):
        registry = release.Registry("project", "region", "repo")
        item = {
            "name": f"{registry.parent}/files/pool%2Ftailrelay_{self.version}_arm64_{'a' * 32}.deb",
            "owner": f"{registry.parent}/packages/tailrelay/versions/{self.version}",
            "hashes": [{"type": "SHA256", "value": base64.b64encode(bytes(32)).decode()}],
        }
        with patch.object(subprocess, "check_output", return_value=json.dumps([item, item])):
            with self.assertRaisesRegex(ValueError, "duplicate"):
                registry.inventory(self.version)
        item["hashes"] = []
        with patch.object(subprocess, "check_output", return_value=json.dumps([item])):
            with self.assertRaises(ValueError):
                registry.inventory(self.version)
        item["owner"] = item["owner"].replace("/tailrelay/", "/another-package/")
        with patch.object(subprocess, "check_output", return_value=json.dumps([item])):
            with self.assertRaisesRegex(ValueError, "owner"):
                registry.inventory(self.version)


class ArtifactValidationTests(unittest.TestCase):
    def test_downloaded_artifact_directories_are_discovered(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            version = "1.0.2"
            for name in release.expected_files(version):
                arch = name.removesuffix(".deb").split("_")[-1]
                folder = root / f"tailrelay-deb-{arch}"
                folder.mkdir()
                (folder / name).touch()
            with patch.object(subprocess, "check_output", side_effect=lambda cmd, **_: {
                "Package": "tailrelay", "Version": version,
                "Architecture": Path(cmd[2]).stem.split("_")[-1],
            }[cmd[3]]):
                self.assertEqual(set(release.local_packages(root, version)),
                                 set(release.expected_files(version)))

    def test_missing_artifacts_fail_before_reading_deb_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "exactly one tailrelay package"):
                release.local_packages(Path(directory), "1.0.2")

    def test_wrong_deb_architecture_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            for name in release.expected_files("1.0.2"):
                (Path(directory) / name).touch()
            def metadata(command, **kwargs):
                package, version, _ = Path(command[2]).stem.split("_")
                return {"Package": package, "Version": version, "Architecture": "all"}[command[3]]
            with patch.object(subprocess, "check_output", side_effect=metadata):
                with self.assertRaisesRegex(ValueError, "Wrong Architecture"):
                    release.local_packages(Path(directory), "1.0.2")

    def test_duplicate_or_extra_packages_fail_before_reading_deb_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in release.expected_files("1.0.2"):
                (root / name).touch()
            nested = root / "duplicate"
            nested.mkdir()
            (nested / "tailrelay_1.0.2_amd64.deb").touch()
            with self.assertRaisesRegex(ValueError, "Duplicate"):
                release.local_packages(root, "1.0.2")
            (nested / "tailrelay_1.0.2_amd64.deb").unlink()
            (root / "another-package_1.0.2_amd64.deb").touch()
            with self.assertRaisesRegex(ValueError, "exactly one tailrelay"):
                release.local_packages(root, "1.0.2")

    def test_symlink_package_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / "fixture"
            target.write_bytes(b"package")
            (root / "tailrelay_1.0.2_amd64.deb").symlink_to(target)
            with self.assertRaisesRegex(ValueError, "regular file"):
                release.local_packages(root, "1.0.2")


class AptIndexTests(unittest.TestCase):
    def setUp(self):
        self.version = "1.0.2"
        self.registry = release.Registry("project", "region", "repo")
        self.payload = b"test package content"
        self.digest = release.hashlib.sha256(self.payload).hexdigest()
        self.hashes = dict.fromkeys(release.expected_files(self.version), self.digest)
        self.entries = {}
        for arch in release.ARCHITECTURES:
            self.entries[arch] = "\n\n".join(
                f"Package: {package}\nVersion: {self.version}\nArchitecture: {arch}\n"
                f"SHA256: {self.digest}\nFilename: pool/{package}_{self.version}_{arch}.deb"
                for package in release.PACKAGES
            ).encode()

    def response(self, url, **kwargs):
        if url.endswith("/Packages"):
            arch = url.split("binary-", 1)[1].split("/", 1)[0]
            return io.BytesIO(self.entries[arch])
        return io.BytesIO(self.payload)

    def test_published_packages_are_downloaded_and_hashed(self):
        with patch.object(release, "urlopen", side_effect=self.response) as request:
            release.verify_apt(self.registry, self.version, self.hashes)
        self.assertEqual(request.call_count, 4)

    def test_existing_release_check_only_reads_indexes(self):
        with patch.object(release, "urlopen", side_effect=self.response) as request:
            release.verify_apt(self.registry, self.version, self.hashes, download=False)
        self.assertEqual(request.call_count, 2)

    def test_stale_index_retries_but_auth_failure_does_not(self):
        calls = 0
        def delayed(url, **kwargs):
            nonlocal calls
            calls += 1
            if calls <= 2:
                raise HTTPError(url, 404, "Index not generated", {}, None)
            return self.response(url)
        with patch.object(release, "urlopen", side_effect=delayed), patch.object(release.time, "sleep") as sleep:
            release.verify_apt(self.registry, self.version, self.hashes)
            sleep.assert_called_once_with(15)
        with patch.object(release, "urlopen", side_effect=HTTPError("url", 403, "Forbidden", {}, None)):
            with self.assertRaises(HTTPError):
                release.verify_apt(self.registry, self.version, self.hashes)

    def test_download_bytes_must_match_registry_and_index(self):
        def corrupt(url, **kwargs):
            if url.endswith("/Packages"):
                return self.response(url)
            return io.BytesIO(b"corrupt")
        with patch.object(release, "urlopen", side_effect=corrupt):
            with self.assertRaisesRegex(ValueError, "download content conflict"):
                release.verify_apt(self.registry, self.version, self.hashes)

    def test_index_hash_conflict_fails_before_package_download(self):
        self.entries["amd64"] = self.entries["amd64"].replace(self.digest.encode(), b"0" * 64)
        with patch.object(release, "urlopen", side_effect=self.response) as request:
            with self.assertRaisesRegex(ValueError, "index content conflict"):
                release.verify_apt(self.registry, self.version, self.hashes)
        self.assertEqual(request.call_count, 1)

    def test_cross_origin_or_non_https_package_download_is_rejected(self):
        original = self.entries["amd64"]
        for url in ("https://another.invalid/package.deb", "http://region-apt.pkg.dev/package.deb"):
            self.entries["amd64"] = original.replace(
                b"Filename: pool/tailrelay_1.0.2_amd64.deb", f"Filename: {url}".encode())
            with self.subTest(url=url), patch.object(release, "urlopen", side_effect=self.response) as request:
                with self.assertRaisesRegex(ValueError, "download origin"):
                    release.verify_apt(self.registry, self.version, self.hashes)
                self.assertEqual(request.call_count, 1)


if __name__ == "__main__":
    unittest.main()
