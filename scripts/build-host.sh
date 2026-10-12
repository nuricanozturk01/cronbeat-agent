#!/bin/sh
# Builds the release files of the cronbeat host CLI into dist/:
#   cronbeat_<version>_linux_amd64.tar.gz, cronbeat_<version>_linux_arm64.tar.gz and SHA256SUMS
#
#   sh scripts/build-host.sh [version]     (default: the version in internal/hostversion)
#
# Needs Go and tar. The binaries are static (CGO_ENABLED=0) and carry the version through -X.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-${VERSION:-}}"
if [ -z "$VERSION" ]; then
  VERSION="$(sed -n 's/^var Version = "\(.*\)"$/\1/p' internal/hostversion/version.go)"
fi
VERSION="${VERSION#v}"
case "$VERSION" in
  [0-9]*.[0-9]*.[0-9]* ) ;;
  *) echo "build-host: invalid version \"$VERSION\"" >&2; exit 1 ;;
esac
case "$VERSION" in
  *[!0-9A-Za-z.+-]* ) echo "build-host: invalid version \"$VERSION\"" >&2; exit 1 ;;
esac

DIST="$ROOT/dist"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/cronbeat-build.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
rm -rf "$DIST"
mkdir -p "$DIST"

# A fixed timestamp and owner make the tarballs reproducible with GNU tar.
EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
TAR_FLAGS=""
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  TAR_FLAGS="--sort=name --owner=0 --group=0 --numeric-owner --mtime=@$EPOCH"
fi

for arch in amd64 arm64; do
  dir="$STAGE/$arch"
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X cronbeat.dev/cronbeat-agent/internal/hostversion.Version=$VERSION" \
    -o "$dir/cronbeat" ./cmd/cronbeat
  chmod 755 "$dir/cronbeat"
  cp LICENSE "$dir/LICENSE"
  archive="cronbeat_${VERSION}_linux_${arch}.tar.gz"
  # shellcheck disable=SC2086 # TAR_FLAGS is a list of separate options
  tar $TAR_FLAGS -cf - -C "$dir" cronbeat LICENSE | gzip -n -9 >"$DIST/$archive"
done

cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum cronbeat_*.tar.gz >SHA256SUMS
else
  shasum -a 256 cronbeat_*.tar.gz >SHA256SUMS
fi

ls -l "$DIST"
cat SHA256SUMS
