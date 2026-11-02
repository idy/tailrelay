import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("tailrelay_release", ROOT / "packaging/release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.identity = {"schema_version": 1, "project": "tailrelay", "version": "0.1.2",
                         "tag": "v0.1.2", "commit": "a" * 40, "repository": "idy/tailrelay", "dirty": False}
        metadata = patch.object(release, "metadata", return_value=self.identity)
        metadata.start()
        self.addCleanup(metadata.stop)
        for goos, arch in release.PLATFORMS:
            names = {f"tailrelay_0.1.2_{goos}_{arch}.tar.gz"}
            if goos == "linux":
                names.add(f"tailrelay_0.1.2_linux_{arch}.deb")
            assets = {}
            for name in names:
                path = self.directory / name
                path.write_bytes(name.encode())
                assets[name] = {"size": path.stat().st_size, "sha256": release.digest(path)}
            fragment = dict(self.identity, os=goos, arch=arch, assets=assets)
            (self.directory / f"release-metadata-{goos}-{arch}.json").write_text(json.dumps(fragment))
        self.remote = None
        self.bytes = {}
        self.events = []
        self.fail_upload = None

    def finalize(self):
        return release.finalize(self.directory, "v0.1.2")

    def fixture_release(self, draft=True):
        self.finalize()
        self.remote = {"id": 123, "tag_name": "v0.1.2", "draft": draft,
                       "body": f"<!-- tailrelay-release commit={self.identity['commit']} -->",
                       "upload_url": "https://uploads.github.com/repos/idy/tailrelay/releases/123/assets{?name,label}",
                       "html_url": "https://github.com/idy/tailrelay/releases/tag/v0.1.2", "assets": []}
        return self.remote

    def add_asset(self, name, content=None):
        asset_id = len(self.bytes) + 1
        self.bytes[asset_id] = content if content is not None else (self.directory / name).read_bytes()
        self.remote["assets"].append({"id": asset_id, "name": name})

    def api(self, path, method="GET", data=None, input_file=None, accept=None, output=None):
        self.events.append((method, path))
        if "/commits/" in path:
            return {"sha": self.identity["commit"]}
        if path.startswith("repos/idy/tailrelay/releases?"):
            return [copy.deepcopy(self.remote)] if self.remote else []
        if path == "repos/idy/tailrelay/releases" and method == "POST":
            self.fixture_release()
            self.remote.update(data)
            return copy.deepcopy(self.remote)
        if path.startswith("https://uploads.github.com/"):
            if self.fail_upload == input_file.name:
                raise subprocess.CalledProcessError(1, "upload")
            self.add_asset(input_file.name)
            return self.remote["assets"][-1]
        if "/releases/assets/" in path:
            self.assertEqual(accept, "application/octet-stream")
            output.write(self.bytes[int(path.rsplit("/", 1)[1])])
            return None
        if path == "repos/idy/tailrelay/releases/123":
            if method == "PATCH":
                self.remote.update(data)
            return copy.deepcopy(self.remote)
        raise AssertionError((path, method))

    def publish(self):
        with patch.object(release, "api", side_effect=self.api):
            release.publish(self.directory, "v0.1.2")

    def test_complete_inventory_and_manifest_are_hashed(self):
        manifest = self.finalize()
        self.assertEqual(len(manifest["assets"]), 6)
        sums = (self.directory / "SHA256SUMS").read_text().splitlines()
        self.assertEqual(len(sums), 7)
        for line in sums:
            sha, name = line.split("  ")
            self.assertEqual(sha, release.digest(self.directory / name))

    def test_missing_platform_is_rejected(self):
        next(self.directory.glob("release-metadata-*.json")).unlink()
        with self.assertRaisesRegex(ValueError, "complete Linux/macOS"):
            self.finalize()

    def test_mixed_commit_is_rejected(self):
        fragment = next(self.directory.glob("release-metadata-*.json"))
        data = json.loads(fragment.read_text())
        data["commit"] = "b" * 40
        fragment.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "release source"):
            self.finalize()

    def test_corrupted_or_symlink_asset_is_rejected(self):
        asset = next(self.directory.glob("*.tar.gz"))
        original = asset.read_bytes()
        asset.write_bytes(b"corrupt")
        with self.assertRaisesRegex(ValueError, "hash or size"):
            self.finalize()
        other = self.directory / "other"
        other.write_bytes(original)
        asset.unlink()
        asset.symlink_to(other)
        with self.assertRaisesRegex(ValueError, "hash or size"):
            self.finalize()

    def test_initial_release_is_draft_until_every_remote_byte_is_verified(self):
        self.publish()
        self.assertFalse(self.remote["draft"])
        self.assertEqual(len(self.remote["assets"]), 8)
        self.assertEqual(self.events[-1], ("PATCH", "repos/idy/tailrelay/releases/123"))
        self.assertEqual(sum("/releases/assets/" in path for _, path in self.events), 8)

    def test_partial_draft_retry_does_not_reupload_existing_bytes(self):
        self.fixture_release()
        name = next(iter(self.finalize()["assets"]))
        self.add_asset(name)
        self.publish()
        uploads = [path for method, path in self.events if path.startswith("https://uploads.")]
        self.assertEqual(len(uploads), 7)
        self.assertFalse(any(path.endswith("name=" + name) for path in uploads))
        self.assertFalse(self.remote["draft"])

    def test_conflicting_draft_fails_before_any_upload(self):
        self.fixture_release()
        self.add_asset(next(iter(self.finalize()["assets"])), b"old conflicting bytes")
        with self.assertRaisesRegex(ValueError, "bytes differ"):
            self.publish()
        self.assertFalse(any(method in ("POST", "PATCH") for method, _ in self.events))
        self.assertTrue(self.remote["draft"])

    def test_identical_published_release_is_read_only(self):
        self.fixture_release(draft=False)
        for path in [*self.finalize()["assets"], "SHA256SUMS", "release-manifest.json"]:
            self.add_asset(path)
        self.publish()
        self.assertFalse(any(method != "GET" for method, _ in self.events))

    def test_incomplete_published_release_cannot_be_filled(self):
        self.fixture_release(draft=False)
        with self.assertRaisesRegex(ValueError, "cannot be filled"):
            self.publish()
        self.assertFalse(any(method != "GET" for method, _ in self.events))

    def test_wrong_release_owner_fails_without_writes(self):
        self.fixture_release()["body"] = "some other release"
        with self.assertRaisesRegex(ValueError, "not owned"):
            self.publish()
        self.assertFalse(any(method != "GET" for method, _ in self.events))

    def test_upload_failure_keeps_draft_and_retry_finishes_it(self):
        self.fail_upload = "tailrelay_0.1.2_linux_arm64.tar.gz"
        with self.assertRaises(subprocess.CalledProcessError):
            self.publish()
        self.assertTrue(self.remote["draft"])
        uploaded_ids = dict(self.bytes)
        self.fail_upload = None
        self.publish()
        self.assertFalse(self.remote["draft"])
        self.assertEqual(len(self.remote["assets"]), 8)
        for asset_id, payload in uploaded_ids.items():
            self.assertEqual(self.bytes[asset_id], payload)

    def test_unexpected_or_duplicate_remote_asset_fails(self):
        for duplicate in (False, True):
            with self.subTest(duplicate=duplicate):
                self.fixture_release()
                self.bytes = {}
                self.add_asset("SHA256SUMS" if duplicate else "unexpected", b"fixture")
                if duplicate:
                    self.add_asset("SHA256SUMS")
                with self.assertRaisesRegex(ValueError, "unexpected or duplicate"):
                    self.publish()

    def test_remote_tag_must_match_before_release_lookup(self):
        with patch.object(release, "api", return_value={"sha": "b" * 40}) as api:
            with self.assertRaisesRegex(ValueError, "Remote tag"):
                release.publish(self.directory, "v0.1.2")
        self.assertEqual(api.call_count, 1)


class ArchiveTests(unittest.TestCase):
    def test_archive_is_deterministic_and_cannot_be_overwritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            binary = directory / "binary"
            binary.write_bytes(b"fixture")
            first, second = directory / "first.tar.gz", directory / "second.tar.gz"
            release.archive(first, {"tailrelay": binary, "LICENSE": binary}, 123)
            release.archive(second, {"tailrelay": binary, "LICENSE": binary}, 123)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with tarfile.open(first) as archive:
                self.assertEqual(archive.getmember("tailrelay").mode, 0o755)
                self.assertEqual(archive.getmember("LICENSE").mode, 0o644)
                self.assertTrue(all(member.isfile() for member in archive))
            with self.assertRaises(FileExistsError):
                release.archive(first, {"tailrelay": binary}, 123)
