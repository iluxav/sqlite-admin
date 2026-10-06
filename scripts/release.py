#!/usr/bin/env python3
"""Assemble and publish a complete, checksummed release via GitHub CLI."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


TARGETS = (
    ("darwin", "amd64"), ("darwin", "arm64"),
    ("linux", "amd64"), ("linux", "arm64"), ("linux", "armv7"),
    ("linux", "386"), ("linux", "riscv64"), ("linux", "ppc64le"),
    ("linux", "s390x"), ("linux", "loong64"),
)
ROOT = Path(__file__).resolve().parent.parent


def validate(version, repo):
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("version must be a stable vMAJOR.MINOR.PATCH tag")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("repository must be owner/repo")


def archives(version, directory):
    expected = [directory / f"sqliteadmin_{version}_{os_name}_{arch}.tar.gz"
                for os_name, arch in TARGETS]
    for path in expected:
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError(f"missing release archive: {path}")
    if set(directory.glob("*.tar.gz")) != set(expected):
        raise ValueError("unexpected archives in the release directory")
    return expected


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def prepare(version, directory, repo):
    validate(version, repo)
    files = archives(version, directory)
    # Make downloaded installers work for the repository running this workflow,
    # including a renamed repository or fork, without changing the source tree.
    installer = (ROOT / "install.sh").read_text().replace(
        "repo=${SQLITEADMIN_REPO:-iluxav/sqlite-admin}",
        "repo=${SQLITEADMIN_REPO:-" + repo + "}",
    )
    (directory / "install.sh").write_text(installer)
    files.append(directory / "install.sh")
    checksums = "".join(f"{digest(path)}  {path.name}\n" for path in sorted(files))
    (directory / "checksums.txt").write_text(checksums)


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True)


def find_release(version, repo):
    # The tag endpoint can return 404 for drafts. Authenticated listing includes
    # drafts and exposes the release ID needed to publish them.
    pages = json.loads(gh("api", f"repos/{repo}/releases", "--paginate", "--slurp"))
    return next((release for page in pages for release in page
                 if release["tag_name"] == version), None)


def publish(version, directory, repo):
    validate(version, repo)
    files = archives(version, directory) + [directory / "install.sh", directory / "checksums.txt"]
    for path in files:
        if not path.is_file():
            raise ValueError(f"missing release file: {path}")
    # Fail on API/network errors. Listing includes drafts with the workflow token.
    existing = find_release(version, repo)
    if existing and not existing["draft"]:
        raise ValueError(f"{version} is already published; published assets are not overwritten. Create a new version tag.")
    if not existing:
        notes = (
            f"sqliteadmin {version}\n\n"
            "Standalone SQLite admin UI for macOS and Linux.\n\n"
            "Install or update to the latest stable release:\n\n```sh\n"
            f"curl -fsSL https://github.com/{repo}/releases/latest/download/install.sh | sh\n"
            "sqliteadmin --version\n```\n\n"
            "Archives include the binary, configuration example, README, and optional systemd unit. "
            "The installer verifies SHA-256 checksums before installation.\n"
        )
        with tempfile.TemporaryDirectory() as temp:
            notes_path = Path(temp) / "notes.md"
            notes_path.write_text(notes)
            gh("release", "create", version, "--repo", repo, "--verify-tag", "--draft",
               "--title", version, "--notes-file", str(notes_path))
        existing = find_release(version, repo)
        if not existing or not existing["draft"]:
            raise ValueError("created draft release is not available; retry the workflow")
    if os.environ.get("RELEASE_COMMIT"):
        # Detect a tag moved while the matrix was running; release exactly what
        # was tested. GitHub's commit endpoint peels annotated tags as well.
        commit = json.loads(gh("api", f"repos/{repo}/commits/{version}"))["sha"]
        if commit != os.environ["RELEASE_COMMIT"]:
            raise ValueError("release tag moved during the build; refusing to publish")
    gh("release", "upload", version, "--repo", repo, "--clobber", *map(str, files))
    # Keep partial uploads hidden. Legacy latest selection uses version ordering,
    # so publishing an older maintenance tag cannot unconditionally replace latest.
    gh("api", "--method", "PATCH", f"repos/{repo}/releases/{existing['id']}",
       "-F", "draft=false", "-F", "prerelease=false", "-f", "make_latest=legacy")
    print(f"Published https://github.com/{repo}/releases/tag/{version}")


def main():
    if len(sys.argv) != 4 or sys.argv[1] not in ("prepare", "publish"):
        raise ValueError("usage: release.py prepare|publish VERSION DIRECTORY")
    operation, version, directory = sys.argv[1:]
    repo = os.environ.get("GITHUB_REPOSITORY", "iluxav/sqlite-admin")
    {"prepare": prepare, "publish": publish}[operation](version, Path(directory), repo)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"release: {error}")
