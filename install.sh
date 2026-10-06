#!/bin/sh
# Install the latest stable sqliteadmin release, or SQLITEADMIN_VERSION=vX.Y.Z.
# Usage: curl -fsSL https://github.com/iluxav/sqlite-admin/releases/latest/download/install.sh | sh
# Options: SQLITEADMIN_INSTALL_DIR, SQLITEADMIN_VERSION, SQLITEADMIN_REPO.
set -eu

main() {
  repo=${SQLITEADMIN_REPO:-iluxav/sqlite-admin}
  err() { printf 'sqliteadmin install: %s\n' "$*" >&2; exit 1; }
  case "$repo" in *[!A-Za-z0-9_./-]*) err 'invalid repository; use owner/repo' ;; esac
  for tool in curl uname tr grep awk tar mktemp install; do
    command -v "$tool" >/dev/null 2>&1 || err "required command is missing: $tool"
  done
  printf '%s\n' "$repo" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || err 'invalid repository; use owner/repo'

  platform=$(uname -s | tr '[:upper:]' '[:lower:]')
  machine=$(uname -m)
  # On Apple Silicon, prefer a native binary even from a Rosetta shell.
  if [ "$platform" = darwin ] && [ "$machine" = x86_64 ] && command -v sysctl >/dev/null 2>&1; then
    if [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then machine=arm64; fi
  fi
  case "$machine" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    armv7|armv7l|armv8l) arch=armv7 ;;
    i386|i486|i586|i686) arch=386 ;;
    riscv64|ppc64le|s390x) arch=$machine ;;
    loongarch64|loong64) arch=loong64 ;;
    *) err "unsupported architecture: $machine" ;;
  esac
  case "$platform/$arch" in
    darwin/amd64|darwin/arm64|linux/amd64|linux/arm64|linux/armv7|linux/386|linux/riscv64|linux/ppc64le|linux/s390x|linux/loong64) ;;
    *) err "unsupported platform: $platform/$arch" ;;
  esac

  if command -v sha256sum >/dev/null 2>&1; then hash_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then hash_tool=shasum
  else err 'SHA-256 verification requires sha256sum or shasum'; fi

  # Resolve latest once, so the archive and checksum always use the same tag.
  version=${SQLITEADMIN_VERSION:-}
  if [ -z "$version" ]; then
    release_url=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 \
      --connect-timeout 15 --max-time 60 -o /dev/null -w '%{url_effective}' \
      "https://github.com/$repo/releases/latest") || err 'cannot find the latest release; check the repository and try again'
    case "$release_url" in "https://github.com/$repo/releases/tag/"*) version=${release_url##*/} ;;
      *) err 'the latest release did not resolve to a release tag' ;; esac
  fi
  case "$version" in *[!v0-9.]*) err 'version must be vMAJOR.MINOR.PATCH' ;; esac
  printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || err 'version must be vMAJOR.MINOR.PATCH'

  asset="sqliteadmin_${version}_${platform}_${arch}.tar.gz"
  base="https://github.com/$repo/releases/download/$version"
  temp_dir=$(mktemp -d)
  pending_install=
  trap 'rm -rf "$temp_dir"; if [ -n "$pending_install" ]; then rm -f "$pending_install"; fi' 0
  trap 'exit 1' HUP INT TERM
  printf 'Downloading sqliteadmin %s for %s/%s…\n' "$version" "$platform" "$arch"
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 15 --max-time 300 \
    "$base/$asset" -o "$temp_dir/$asset" || err "cannot download $asset"
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 15 --max-time 60 \
    "$base/checksums.txt" -o "$temp_dir/checksums.txt" || err 'cannot download checksums.txt'
  expected=$(awk -v name="$asset" '$2 == name { count++; hash=$1 } END { if (count == 1) print hash }' "$temp_dir/checksums.txt")
  printf '%s\n' "$expected" | grep -Eq '^[a-fA-F0-9]{64}$' || err "missing or invalid checksum for $asset"
  if [ "$hash_tool" = sha256sum ]; then
    actual=$(sha256sum "$temp_dir/$asset" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "$temp_dir/$asset" | awk '{print $1}')
  fi
  [ "$expected" = "$actual" ] || err "checksum mismatch for $asset; nothing was installed"

  # Extract only the expected file, then reject symlinks and non-regular files.
  tar -xzf "$temp_dir/$asset" -C "$temp_dir" sqliteadmin || err 'cannot extract the binary'
  if [ ! -f "$temp_dir/sqliteadmin" ] || [ -L "$temp_dir/sqliteadmin" ]; then
    err 'archive does not contain a regular sqliteadmin binary'
  fi
  if [ -n "${SQLITEADMIN_INSTALL_DIR:-}" ]; then
    install_dir=$SQLITEADMIN_INSTALL_DIR
  elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    install_dir=/usr/local/bin
  else
    [ -n "${HOME:-}" ] || err 'set SQLITEADMIN_INSTALL_DIR; HOME is not set'
    install_dir="$HOME/.local/bin"
  fi
  case "$install_dir" in /*) ;; *) install_dir="$PWD/$install_dir" ;; esac
  mkdir -p "$install_dir" || err "cannot create $install_dir; set SQLITEADMIN_INSTALL_DIR to a writable directory"
  [ ! -d "$install_dir/sqliteadmin" ] || err 'the install destination is a directory'
  pending_install=$(mktemp "$install_dir/.sqliteadmin.XXXXXX") || err "cannot write to $install_dir; set SQLITEADMIN_INSTALL_DIR"
  install -m 0755 "$temp_dir/sqliteadmin" "$pending_install"
  # Rename in place so upgrading a running daemon does not truncate its binary.
  mv -f "$pending_install" "$install_dir/sqliteadmin"
  pending_install=
  printf 'Installed sqliteadmin %s to %s/sqliteadmin\n' "$version" "$install_dir"
  case ":${PATH:-}:" in *":$install_dir:"*) ;; *) printf 'Add %s to your PATH.\n' "$install_dir" ;; esac
  printf 'Run: sqliteadmin --version\nStart: sqliteadmin serve --db /path/to/app.db --env-file /path/to/app.env\n'
  printf 'Restart any running sqliteadmin instance to use the new version.\n'
}

# Keep execution last so a truncated curl stream cannot perform a partial install.
main "$@"
