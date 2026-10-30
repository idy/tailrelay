import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "tailrelay_apt", ROOT.parent / ".github/actions/server-apt/apt-release.py"
)
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class TailRelayPackagingTests(unittest.TestCase):
    def test_version_and_both_architectures_use_the_shared_registry(self):
        version = release.read_version(ROOT / "VERSION")
        with patch.object(release, "PACKAGES", ("tailrelay",)):
            self.assertEqual(set(release.expected_files(version)), {
                f"tailrelay_{version}_amd64.deb", f"tailrelay_{version}_arm64.deb",
            })

    def test_package_never_ships_operator_configuration_or_identity(self):
        build = (ROOT.parent / ".github/actions/server-apt/build-deb.sh").read_text()
        branch = build.split('elif [ "$package" = tailrelay ]; then', 1)[1].split(
            'elif [ "$package" = nautiloid ]; then', 1
        )[0]
        self.assertNotIn('"$root/etc/tailrelay', branch)
        self.assertNotIn('"$root/var/lib/tailrelay', branch)
        self.assertIn("-tags with_gvisor", branch)

    def test_unit_and_package_binary_paths_agree(self):
        unit = (ROOT / "deploy/tailrelay@.service").read_text()
        self.assertIn("ExecStart=/usr/bin/tailrelay run", unit)
        self.assertIn("User=tailrelay-%i", unit)
        self.assertIn("StateDirectory=tailrelay/%i", unit)


if __name__ == "__main__":
    unittest.main()
