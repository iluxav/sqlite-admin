import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
VERSION = "v1.2.3"


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.assets = self.root / "release"
        self.assets.mkdir()
        self.install_dir = self.root / "install with spaces"
        self.env = {key: value for key, value in os.environ.items()
                    if not key.startswith(("SQLITEADMIN_", "TEST_"))}
        self.env.update(PATH=str(self.tools), HOME=str(self.root), TMPDIR=str(self.root),
                        SQLITEADMIN_INSTALL_DIR=str(self.install_dir),
                        SQLITEADMIN_VERSION=VERSION, TEST_RELEASE_DIR=str(self.assets),
                        TEST_CURL_LOG=str(self.root / "requests"), TEST_OS="Linux", TEST_ARCH="x86_64")
        for name in ("tr", "grep", "awk", "tar", "gzip", "mktemp", "install", "mkdir", "mv", "rm", "sha256sum", "shasum"):
            actual = shutil.which(name)
            if actual:
                (self.tools / name).symlink_to(actual)
        self.tool("uname", '#!/bin/sh\ncase "$1" in -s) echo "$TEST_OS" ;; -m) echo "$TEST_ARCH" ;; esac\n')
        self.tool("sysctl", '#!/bin/sh\necho "${TEST_ROSETTA:-0}"\n')
        self.tool("curl", f"#!{sys.executable}\n" + '''import os, pathlib, sys
args = sys.argv[1:]
url = next(arg for arg in args if arg.startswith("https://"))
with open(os.environ["TEST_CURL_LOG"], "a") as log:
    log.write(url + "\\n")
if os.environ.get("TEST_DOWNLOAD_FAIL"):
    sys.exit(22)
if url.endswith("/releases/latest"):
    print(url.removesuffix("/latest") + "/tag/v1.2.3", end="")
else:
    # Reject unpinned downloads; latest must be resolved before fetching assets.
    if "/releases/download/v1.2.3/" not in url:
        sys.exit(22)
    source = pathlib.Path(os.environ["TEST_RELEASE_DIR"]) / url.rsplit("/", 1)[1]
    if not source.is_file():
        sys.exit(22)
    pathlib.Path(args[args.index("-o")+1]).write_bytes(source.read_bytes())
''')

    def tool(self, name, contents):
        path = self.tools / name
        path.write_text(contents)
        path.chmod(0o755)

    def archive(self, platform="linux", arch="amd64", bad_checksum=False, symlink=False):
        name = f"sqliteadmin_{VERSION}_{platform}_{arch}.tar.gz"
        path = self.assets / name
        binary = b"#!/bin/sh\nprintf 'sqliteadmin v1.2.3\\n'\n"
        with tarfile.open(path, "w:gz") as archive:
            member = tarfile.TarInfo("sqliteadmin")
            member.mode = 0o755
            if symlink:
                member.type = tarfile.SYMTYPE
                member.linkname = "/bin/sh"
                archive.addfile(member)
            else:
                member.size = len(binary)
                archive.addfile(member, io.BytesIO(binary))
        checksum = "0" * 64 if bad_checksum else hashlib.sha256(path.read_bytes()).hexdigest()
        (self.assets / "checksums.txt").write_text(f"{checksum}  {name}\n")
        return binary

    def run_installer(self):
        # Feed stdin just like curl | sh: no terminal and no temporary script path.
        return subprocess.run(["/bin/sh"], input=(ROOT / "install.sh").read_text(),
                              env=self.env, text=True, capture_output=True, timeout=10)

    def test_all_architecture_mappings(self):
        targets = [("Darwin", "x86_64", "darwin", "amd64"),
                   ("Darwin", "arm64", "darwin", "arm64"),
                   ("Linux", "x86_64", "linux", "amd64"),
                   ("Linux", "aarch64", "linux", "arm64"),
                   ("Linux", "armv7l", "linux", "armv7"),
                   ("Linux", "armv8l", "linux", "armv7"),
                   ("Linux", "i686", "linux", "386"),
                   ("Linux", "riscv64", "linux", "riscv64"),
                   ("Linux", "ppc64le", "linux", "ppc64le"),
                   ("Linux", "s390x", "linux", "s390x"),
                   ("Linux", "loongarch64", "linux", "loong64")]
        for system, machine, platform, arch in targets:
            with self.subTest(system=system, machine=machine):
                self.env.update(TEST_OS=system, TEST_ARCH=machine)
                expected = self.archive(platform, arch)
                result = self.run_installer()
                self.assertEqual(result.returncode, 0, result.stderr)
                installed = self.install_dir / "sqliteadmin"
                self.assertEqual(installed.read_bytes(), expected)
                self.assertEqual(installed.stat().st_mode & 0o777, 0o755)

    def test_latest_resolves_once_and_rosetta_uses_arm64(self):
        self.env.pop("SQLITEADMIN_VERSION")
        self.env.update(TEST_OS="Darwin", TEST_ARCH="x86_64", TEST_ROSETTA="1")
        self.archive("darwin", "arm64")
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = (self.root / "requests").read_text().splitlines()
        self.assertEqual(len(requests), 3)
        self.assertTrue(requests[0].endswith("/releases/latest"))
        self.assertIn("/download/v1.2.3/", requests[1])
        self.assertTrue(requests[1].endswith("darwin_arm64.tar.gz"))

    def test_checksum_failure_preserves_existing_install(self):
        self.install_dir.mkdir()
        old = self.install_dir / "sqliteadmin"
        old.write_bytes(b"previous binary")
        self.archive(bad_checksum=True)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        self.assertEqual(old.read_bytes(), b"previous binary")

    def test_bad_download_and_unsupported_arch_never_install(self):
        self.archive()
        self.env["TEST_DOWNLOAD_FAIL"] = "1"
        self.assertNotEqual(self.run_installer().returncode, 0)
        self.env.pop("TEST_DOWNLOAD_FAIL")
        self.env["TEST_ARCH"] = "armv6l"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported architecture", result.stderr)
        self.assertFalse(self.install_dir.exists())

    def test_symlink_payload_is_rejected(self):
        self.archive(symlink=True)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("regular sqliteadmin binary", result.stderr)
        self.assertFalse(self.install_dir.exists())

    def test_shasum_fallback_and_atomic_upgrade(self):
        if not (self.tools / "shasum").exists():
            self.skipTest("shasum is not installed")
        (self.tools / "sha256sum").unlink(missing_ok=True)
        self.install_dir.mkdir()
        old = self.install_dir / "sqliteadmin"
        old.write_bytes(b"previous binary")
        with old.open("rb") as running_binary:
            expected = self.archive()
            result = self.run_installer()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(running_binary.read(), b"previous binary")
            self.assertEqual(old.read_bytes(), expected)

    def test_invalid_versions_are_rejected(self):
        for version in ("main", "v1.2.3-rc.1", "v01.2.3", "v1.2.3\n../../bad"):
            self.env["SQLITEADMIN_VERSION"] = version
            result = self.run_installer()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("version must be", result.stderr)
        self.assertFalse(self.install_dir.exists())


if __name__ == "__main__":
    unittest.main()
