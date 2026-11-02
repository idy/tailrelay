import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("tailrelay_licenses", ROOT / "packaging/licenses.py")
licenses = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(licenses)


class LicenseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.goroot = self.root / "sdk/libexec"
        self.goroot.mkdir(parents=True)
        (self.root / "THIRD_PARTY_NOTICES.md").write_text("Project notice")
        self.module = self.root / "module"
        self.module.mkdir()
        (self.module / "LICENSE").write_text("Dependency license")
        (self.module / "NOTICE").write_text("Dependency copyright")
        module = {"Path": "example.com/dependency", "Version": "v1.2.3", "Dir": str(self.module)}
        package = json.dumps({"Module": module})
        # go list emits multiple JSON documents, including duplicate modules and a main module.
        self.stream = "\n " + package + "\n" + package + "\n" + json.dumps({"Module": {"Main": True}}) + "\n"

    def collect(self):
        output = self.root / "out/NOTICES.md"
        with patch.object(licenses, "ROOT", self.root), patch.object(
                licenses, "command", side_effect=[self.stream, str(self.goroot)]):
            licenses.collect("linux", "arm64", output)
        return output.read_text()

    def test_sdk_license_and_all_module_notices_are_collected_once(self):
        (self.goroot / "LICENSE").write_text("SDK Go license")
        notices = self.collect()
        self.assertIn("SDK Go license", notices)
        self.assertIn("Dependency license", notices)
        self.assertIn("Dependency copyright", notices)
        self.assertEqual(notices.count("## example.com/dependency v1.2.3"), 1)

    def test_homebrew_license_is_discovered_beside_libexec(self):
        (self.goroot.parent / "LICENSE").write_text("Homebrew Go license")
        self.assertIn("Homebrew Go license", self.collect())

    def test_vendored_fallback_works_when_sdk_license_is_absent(self):
        (self.root / "LICENSES").mkdir()
        (self.root / "LICENSES/go.txt").write_text("Vendored Go license")
        self.assertIn("Vendored Go license", self.collect())

    def test_missing_go_license_fails_instead_of_omitting_it(self):
        with self.assertRaisesRegex(ValueError, "No Go standard library license"):
            self.collect()

    def test_missing_dependency_license_stops_release(self):
        (self.goroot / "LICENSE").write_text("SDK license")
        for path in self.module.iterdir():
            path.unlink()
        with self.assertRaisesRegex(ValueError, "example.com/dependency"):
            self.collect()
