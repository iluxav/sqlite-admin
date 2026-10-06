#!/bin/sh
# Build one self-contained release archive. Usage: VERSION OS ARCH [OUTPUT_DIR]
set -eu

version=${1:?usage: build-release.sh VERSION OS ARCH [OUTPUT_DIR]}
target_os=${2:?missing OS}
target_arch=${3:?missing architecture}
output_dir=${4:-dist}
case "$version" in *[!v0-9.]*) printf 'invalid release version\n' >&2; exit 1 ;; esac
printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
  printf 'release version must be vMAJOR.MINOR.PATCH\n' >&2; exit 1;
}
case "$target_os/$target_arch" in
  darwin/amd64|darwin/arm64|linux/amd64|linux/arm64|linux/armv7|linux/386|linux/riscv64|linux/ppc64le|linux/s390x|linux/loong64) ;;
  *) printf 'unsupported release target: %s/%s\n' "$target_os" "$target_arch" >&2; exit 1 ;;
esac

export GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 GOAMD64=v1
if [ "$target_arch" = armv7 ]; then export GOARCH=arm GOARM=7; fi
mkdir -p "$output_dir"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' 0
trap 'exit 1' HUP INT TERM
go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags "-s -w -X github.com/iluxav/sqlite-admin/internal/cli.Version=$version" \
  -o "$stage/sqliteadmin" ./cmd/sqliteadmin
cp README.md "$stage/README.md"
cp .env.example "$stage/sqliteadmin.env.example"
cp contrib/sqliteadmin.service "$stage/sqliteadmin.service"
tar -C "$stage" -czf "$output_dir/sqliteadmin_${version}_${target_os}_${target_arch}.tar.gz" \
  sqliteadmin README.md sqliteadmin.env.example sqliteadmin.service
