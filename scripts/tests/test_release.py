import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("release", Path(__file__).resolve().parents[1] / "release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dist = Path(self.temp.name)
        self.version = "v1.2.3"
        self.repo = "iluxav/sqlite-admin"
        for system, arch in release.TARGETS:
            (self.dist / f"sqliteadmin_{self.version}_{system}_{arch}.tar.gz").write_bytes(b"archive fixture")
        release.prepare(self.version, self.dist, self.repo)
        self.calls = []
        self.addCleanup(patch.stopall)
        patch.dict(os.environ, {"RELEASE_COMMIT": "tested-commit"}).start()

    def fake_gh(self, *args):
        self.calls.append(args)
        if "--paginate" in args:
            return "[[]]"
        if args[0] == "api" and "/releases/tags/" in args[1]:
            return json.dumps({"id": 123, "tag_name": self.version, "draft": True})
        if args[0] == "api" and "/commits/" in args[1]:
            return json.dumps({"sha": "tested-commit"})
        return ""

    def test_manifest_and_fork_installer(self):
        release.prepare(self.version, self.dist, "someone/fork")
        lines = (self.dist / "checksums.txt").read_text().splitlines()
        self.assertEqual(len(lines), len(release.TARGETS) + 1)
        for line in lines:
            checksum, name = line.split()
            self.assertEqual(checksum, release.digest(self.dist / name))
        self.assertIn("repo=${SQLITEADMIN_REPO:-someone/fork}", (self.dist / "install.sh").read_text())

    def test_missing_target_blocks_prepare(self):
        next(self.dist.glob("*.tar.gz")).unlink()
        with self.assertRaisesRegex(ValueError, "missing release archive"):
            release.prepare(self.version, self.dist, self.repo)

    def test_upload_finishes_before_publish(self):
        with patch.object(release, "gh", self.fake_gh):
            release.publish(self.version, self.dist, self.repo)
        create = next(call for call in self.calls if call[:2] == ("release", "create"))
        self.assertIn("--draft", create)
        upload = next(i for i, call in enumerate(self.calls) if call[:2] == ("release", "upload"))
        self.assertLess(upload, len(self.calls) - 1)
        self.assertIn("draft=false", self.calls[-1])
        self.assertIn("make_latest=legacy", self.calls[-1])

    def test_upload_failure_leaves_draft(self):
        def fail_upload(*args):
            if args[:2] == ("release", "upload"):
                raise subprocess.CalledProcessError(1, ["gh", *args])
            return self.fake_gh(*args)
        with patch.object(release, "gh", fail_upload), self.assertRaises(subprocess.CalledProcessError):
            release.publish(self.version, self.dist, self.repo)
        self.assertFalse(any("draft=false" in call for call in self.calls))

    def test_published_release_is_not_overwritten(self):
        existing = json.dumps([[{"id": 123, "tag_name": self.version, "draft": False}]])
        with patch.object(release, "gh", return_value=existing) as gh:
            with self.assertRaisesRegex(ValueError, "already published"):
                release.publish(self.version, self.dist, self.repo)
            self.assertEqual(gh.call_count, 1)

    def test_draft_can_be_resumed(self):
        def draft(*args):
            if "--paginate" in args:
                return json.dumps([[{"id": 123, "tag_name": self.version, "draft": True}]])
            return self.fake_gh(*args)
        with patch.object(release, "gh", draft):
            release.publish(self.version, self.dist, self.repo)
        self.assertFalse(any(call[:2] == ("release", "create") for call in self.calls))
        self.assertIn("draft=false", self.calls[-1])

    def test_moved_tag_blocks_upload(self):
        def moved(*args):
            if args[0] == "api" and "/commits/" in args[1]:
                return json.dumps({"sha": "different-commit"})
            return self.fake_gh(*args)
        with patch.object(release, "gh", moved), self.assertRaisesRegex(ValueError, "tag moved"):
            release.publish(self.version, self.dist, self.repo)
        self.assertFalse(any(call[:2] == ("release", "upload") for call in self.calls))


if __name__ == "__main__":
    unittest.main()
