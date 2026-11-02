import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "tailrelay_snapshot", ROOT / "packaging/snapshot.py"
)
snapshot = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(snapshot)

COMMIT = "1234567890abcdef1234567890abcdef12345678"
IDENTITY = {
    "schema_version": 1, "package": "tailrelay", "channel": "snapshot",
    "version": "0.1.2+snapshot." + COMMIT[:12], "commit": COMMIT,
    "tag": snapshot.TAG_PREFIX + COMMIT, "dirty": False,
}


class FakeGitHub:
    """Keep uploaded bytes separately so tests exercise real download verification."""
    def __init__(self):
        self.tag = None
        self.release = None
        self.files = {}
        self.uploaded = []
        self.writes = []
        self.fail_upload_after = None
        self.metadata = {"private": True, "full_name": "idy/tailrelay"}

    def api(self, path, method="GET", data=None, missing_ok=False):
        if path == "":
            return copy.deepcopy(self.metadata)
        if method != "GET":
            self.writes.append((path, method))
        if path.startswith("git/ref/tags/"):
            return copy.deepcopy(self.tag)
        if path == "git/refs":
            self.tag = {"object": {"type": "commit", "sha": data["sha"]}}
            return copy.deepcopy(self.tag)
        if method == "GET":
            if path.startswith("releases?"):
                return [] if self.release is None else [self.api("releases/1")]
            if path.startswith("releases/tags/") and self.release is not None and self.release["draft"]:
                if missing_ok:
                    return None
                raise ValueError("Published-tag endpoint cannot retrieve a draft")
            if self.release is None:
                if not missing_ok:
                    raise ValueError("Missing release")
                return None
            result = copy.deepcopy(self.release)
            result["assets"] = [{"name": name, "id": index + 1} for index, name in enumerate(sorted(self.files))]
            return result
        if path == "releases":
            self.release = dict(data, id=1, assets=[], html_url="https://example.test/snapshot")
        else:
            self.release.update(data)
        return self.api("releases/1")

    find_release = snapshot.GitHub.find_release

    def download(self, release, directory):
        for name, content in self.files.items():
            (directory / name).write_bytes(content)

    def upload(self, tag, paths):
        for path in paths:
            if self.fail_upload_after == len(self.uploaded):
                raise OSError("Simulated interrupted upload")
            if path.name in self.files:
                raise AssertionError("Attempted to overwrite an existing asset")
            self.uploaded.append(path.name)
            self.files[path.name] = path.read_bytes()


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        for name, arch in snapshot.BINARIES.items():
            header = bytearray(64)
            header[:7] = b"\x7fELF\x02\x01\x01"
            header[18:20] = snapshot.ARCHES[arch].to_bytes(2, "little")
            (self.directory / name).write_bytes(header + name.encode())
        self.identity = patch.object(snapshot, "source", return_value=dict(IDENTITY))
        self.identity.start()
        self.addCleanup(self.identity.stop)
        summary = patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": ""})
        summary.start()
        self.addCleanup(summary.stop)

    def bundle(self):
        return snapshot.assemble(self.directory, IDENTITY)

    def publish(self, github):
        return snapshot.publish(self.directory, "idy/tailrelay", github)

    def test_exact_inventory_and_deterministic_manifest(self):
        self.bundle()
        before = {name: (self.directory / name).read_bytes() for name in snapshot.ASSETS}
        snapshot.verify(self.directory, IDENTITY)
        manifest = json.loads(before[snapshot.MANIFEST])
        self.assertEqual(manifest["commit"], COMMIT)
        self.assertEqual(set(manifest["assets"]), set(snapshot.BINARIES))
        for name in (snapshot.MANIFEST, snapshot.CHECKSUMS):
            (self.directory / name).unlink()
        self.bundle()
        self.assertEqual(before, {name: (self.directory / name).read_bytes()
                                  for name in snapshot.ASSETS})

    def test_private_or_extra_files_cannot_enter_bundle(self):
        (self.directory / "proxy.credentials.json").write_text("private")
        with self.assertRaisesRegex(ValueError, "exactly the two"):
            self.bundle()

    def test_wrong_architecture_is_rejected(self):
        path = self.directory / "tailrelay-linux-arm64"
        path.write_bytes((self.directory / "tailrelay-linux-amd64").read_bytes())
        with self.assertRaisesRegex(ValueError, "ELF architecture"):
            self.bundle()

    def test_downloaded_tampered_binary_or_manifest_is_rejected(self):
        self.bundle()
        path = self.directory / "tailrelay-linux-amd64"
        path.write_bytes(path.read_bytes() + b"tampered")
        with self.assertRaisesRegex(ValueError, "differs from its manifest"):
            snapshot.verify(self.directory, IDENTITY)
        with self.assertRaisesRegex(ValueError, "checked-out source"):
            snapshot.verify(self.directory, dict(IDENTITY, commit="0" * 40))

    def test_missing_checksum_entry_is_rejected(self):
        self.bundle()
        (self.directory / snapshot.CHECKSUMS).write_text("")
        with self.assertRaisesRegex(ValueError, "checksums differ"):
            snapshot.verify(self.directory, IDENTITY)

    def test_publish_and_rerun_verifies_without_replacing_any_asset(self):
        self.bundle()
        github = FakeGitHub()
        release = self.publish(github)
        self.assertFalse(release["draft"])
        self.assertTrue(release["prerelease"])
        self.assertEqual(set(github.uploaded), snapshot.ASSETS)
        writes = list(github.writes)
        self.publish(github)
        self.assertEqual(github.writes, writes)
        self.assertEqual(len(github.uploaded), 4)

    def test_draft_lookup_does_not_require_the_published_tag_endpoint(self):
        self.bundle()
        github = FakeGitHub()
        github.fail_upload_after = 0
        with self.assertRaises(OSError):
            self.publish(github)
        self.assertIsNone(github.api("releases/tags/" + IDENTITY["tag"], missing_ok=True))
        self.assertEqual(github.find_release(IDENTITY["tag"])["id"], 1)
        github.fail_upload_after = None
        self.assertFalse(self.publish(github)["draft"])

    def test_draft_download_uses_asset_ids_without_looking_up_a_tag(self):
        with patch.dict(os.environ, {"GH_TOKEN": "test-only-token"}):
            github = snapshot.GitHub("idy/tailrelay")
        directory = self.directory / "download"
        directory.mkdir()
        def download(args, **kwargs):
            self.assertIn("repos/idy/tailrelay/releases/assets/42", args)
            kwargs["stdout"].write(b"downloaded bytes")
        with patch.object(snapshot.subprocess, "run", side_effect=download):
            github.download({"assets": [{"id": 42, "name": "tailrelay-linux-amd64"}]}, directory)
        self.assertEqual((directory / "tailrelay-linux-amd64").read_bytes(), b"downloaded bytes")

    def test_interrupted_upload_resumes_only_missing_files(self):
        self.bundle()
        github = FakeGitHub()
        github.fail_upload_after = 2
        with self.assertRaisesRegex(OSError, "interrupted"):
            self.publish(github)
        self.assertTrue(github.release["draft"])
        self.assertEqual(len(github.files), 2)
        github.fail_upload_after = None
        self.publish(github)
        self.assertFalse(github.release["draft"])
        self.assertEqual(len(github.uploaded), len(set(github.uploaded)))
        self.assertEqual(set(github.files), snapshot.ASSETS)

    def test_existing_draft_conflict_prevents_further_uploads(self):
        self.bundle()
        github = FakeGitHub()
        github.fail_upload_after = 1
        with self.assertRaises(OSError):
            self.publish(github)
        github.files[github.uploaded[0]] += b"conflict"
        github.fail_upload_after = None
        with self.assertRaisesRegex(ValueError, "bytes differ"):
            self.publish(github)
        self.assertTrue(github.release["draft"])
        self.assertEqual(len(github.uploaded), 1)

    def test_existing_published_partial_release_is_never_filled_or_changed(self):
        self.bundle()
        github = FakeGitHub()
        self.publish(github)
        del github.files["tailrelay-linux-arm64"]
        writes = list(github.writes)
        with self.assertRaisesRegex(ValueError, "missing assets"):
            self.publish(github)
        self.assertEqual(github.writes, writes)
        self.assertFalse(github.release["draft"])

    def test_moved_tag_is_never_adopted_or_overwritten(self):
        self.bundle()
        github = FakeGitHub()
        github.tag = {"object": {"type": "commit", "sha": "0" * 40}}
        with self.assertRaisesRegex(ValueError, "refusing to move"):
            self.publish(github)
        self.assertEqual(github.writes, [])
        self.assertIsNone(github.release)

    def test_foreign_release_is_rejected(self):
        self.bundle()
        github = FakeGitHub()
        self.publish(github)
        github.release["body"] = "Manually published something else"
        writes = list(github.writes)
        with self.assertRaisesRegex(ValueError, "not owned"):
            self.publish(github)
        self.assertEqual(github.writes, writes)

    def test_public_repository_can_publish(self):
        self.bundle()
        github = FakeGitHub()
        github.metadata = {"private": False, "full_name": "idy/tailrelay"}
        self.publish(github)
        self.assertFalse(github.release["draft"])
        self.assertEqual(set(github.files), snapshot.ASSETS)

    def test_wrong_repository_is_rejected_before_any_write(self):
        self.bundle()
        github = FakeGitHub()
        github.metadata = {"private": False, "full_name": "idy/another-repo"}
        with self.assertRaisesRegex(ValueError, "selected GitHub repository"):
            self.publish(github)
        self.assertEqual(github.writes, [])
        self.assertEqual(github.uploaded, [])

    def test_dirty_source_cannot_be_published(self):
        self.bundle()
        self.identity.stop()
        with patch.dict(os.environ, {"GITHUB_SHA": COMMIT}), \
                patch.object(snapshot, "git", side_effect=[COMMIT, " M config.go"]):
            with self.assertRaisesRegex(ValueError, "Commit TailRelay"):
                self.publish(FakeGitHub())

    def test_repository_metadata_and_nested_endpoints_use_canonical_urls(self):
        with patch.dict(os.environ, {"GH_TOKEN": "test-only-token"}):
            github = snapshot.GitHub("idy/tailrelay")
        root = "https://api.github.com/repos/idy/tailrelay"
        for path, expected in (("", root), ("releases?per_page=100", root + "/releases?per_page=100")):
            with self.subTest(path=path):
                with patch.object(snapshot, "urlopen", return_value=io.BytesIO(b"{}")) as opened:
                    self.assertEqual(github.api(path), {})
                self.assertEqual(opened.call_args.args[0].full_url, expected)

    def test_authentication_failure_is_not_treated_as_missing_release(self):
        with patch.dict(os.environ, {"GH_TOKEN": "test-only-token"}):
            github = snapshot.GitHub("idy/tailrelay")
        with patch.object(snapshot, "urlopen", side_effect=HTTPError(
                "https://api.github.com/test", 403, "Forbidden", {}, None)):
            with self.assertRaisesRegex(ValueError, "HTTP 403"):
                github.api("releases/tags/test", missing_ok=True)


class SnapshotSourceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "VERSION").write_text("0.1.2\n")
        root = patch.object(snapshot, "ROOT", self.root)
        root.start()
        self.addCleanup(root.stop)
        environment = patch.dict(os.environ, {"GITHUB_SHA": COMMIT})
        environment.start()
        self.addCleanup(environment.stop)

    def test_git_commands_run_from_standalone_root(self):
        with patch.object(snapshot.subprocess, "run") as run:
            snapshot.git("rev-parse", "HEAD")
        self.assertEqual(run.call_args.kwargs["cwd"], ROOT)

    def test_source_commit_and_version_are_root_relative(self):
        with patch.object(snapshot, "git", side_effect=[COMMIT, ""]) as git:
            self.assertEqual(snapshot.source(), IDENTITY)
        self.assertEqual(git.call_args_list[1].args,
                         ("status", "--porcelain", "--untracked-files=normal"))

    def test_root_runtime_packaging_and_workflow_changes_are_dirty(self):
        for filename in ("config.go", "Makefile", "packaging/build-deb.sh",
                         ".github/workflows/tailrelay-apt.yml"):
            with self.subTest(filename=filename), \
                    patch.object(snapshot, "git", side_effect=[COMMIT, " M " + filename]):
                with self.assertRaisesRegex(ValueError, "Commit TailRelay"):
                    snapshot.source()

    def test_local_dirty_build_is_explicitly_marked(self):
        with patch.object(snapshot, "git", side_effect=[COMMIT, "?? local.go"]):
            self.assertEqual(snapshot.source(allow_dirty=True), dict(IDENTITY, dirty=True))

    def test_workflow_commit_must_match_checkout(self):
        with patch.object(snapshot, "git", return_value="a" * 40):
            with self.assertRaisesRegex(ValueError, "workflow source commit"):
                snapshot.source()

    def test_source_requires_full_commit_and_plain_release_version(self):
        with patch.object(snapshot, "git", return_value=COMMIT[:12]):
            with self.assertRaisesRegex(ValueError, "full Git commit"):
                snapshot.source()
        (self.root / "VERSION").write_text("0.1.2+local\n")
        with patch.object(snapshot, "git", side_effect=[COMMIT, ""]):
            with self.assertRaisesRegex(ValueError, "major.minor.patch"):
                snapshot.source()


if __name__ == "__main__":
    unittest.main()
