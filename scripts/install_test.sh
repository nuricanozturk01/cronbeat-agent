#!/bin/sh
# Tests scripts/install.sh against a fake release served by a local HTTP server.
#
#   sh scripts/install_test.sh
#
# CRONBEAT_TEST_BINARY may point to a prebuilt cronbeat binary (built for this machine);
# otherwise it is built from this repository. The uname results are injected, so the
# amd64 and arm64 downloads are tested on any machine.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
INSTALL="$HERE/install.sh"
VERSION="0.1.0"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/cronbeat-install-test.XXXXXX")"
SERVER_PID=""
FAILED=0
PASSED=0

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
  fi
  chmod -R u+rwx "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() {
  PASSED=$((PASSED + 1))
  printf 'ok   %s\n' "$1"
}

failed() {
  FAILED=$((FAILED + 1))
  printf 'FAIL %s\n' "$1"
  if [ -n "${2:-}" ]; then
    printf '%s\n' "$2" | sed 's/^/     /'
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

command -v python3 >/dev/null 2>&1 || {
  echo "python3 is needed for the local release server, skipping install tests"
  exit 0
}

build_binary() {
  if [ -n "${CRONBEAT_TEST_BINARY:-}" ]; then
    cp "$CRONBEAT_TEST_BINARY" "$WORK/cronbeat"
  else
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X cronbeat.dev/cronbeat-agent/internal/hostversion.Version=$VERSION" \
      -o "$WORK/cronbeat" ./cmd/cronbeat)
  fi
  chmod 755 "$WORK/cronbeat"
}

# make_release DIR [BINARY]: a release tree with both architectures and SHA256SUMS.
make_release() {
  dir="$1"
  binary="${2:-$WORK/cronbeat}"
  rel="$dir/host-v$VERSION"
  mkdir -p "$rel" "$WORK/stage"
  rm -rf "$WORK/stage"/*
  cp "$binary" "$WORK/stage/cronbeat"
  : >"$rel/SHA256SUMS"
  for arch in amd64 arm64; do
    name="cronbeat_${VERSION}_linux_${arch}.tar.gz"
    tar -czf "$rel/$name" -C "$WORK/stage" cronbeat
    printf '%s  %s\n' "$(sha256_of "$rel/$name")" "$name" >>"$rel/SHA256SUMS"
  done
}

build_binary

make_release "$WORK/www/good"
make_release "$WORK/www/tampered"
printf 'tampered' >>"$WORK/www/tampered/host-v$VERSION/cronbeat_${VERSION}_linux_amd64.tar.gz"
make_release "$WORK/www/nosum"
grep -v 'linux_amd64' "$WORK/www/nosum/host-v$VERSION/SHA256SUMS" >"$WORK/sums.tmp"
mv "$WORK/sums.tmp" "$WORK/www/nosum/host-v$VERSION/SHA256SUMS"
make_release "$WORK/www/star"
sed 's/  cronbeat_/ *cronbeat_/' "$WORK/www/star/host-v$VERSION/SHA256SUMS" >"$WORK/sums.tmp"
mv "$WORK/sums.tmp" "$WORK/www/star/host-v$VERSION/SHA256SUMS"
make_release "$WORK/www/dupsum"
cat "$WORK/www/dupsum/host-v$VERSION/SHA256SUMS" "$WORK/www/dupsum/host-v$VERSION/SHA256SUMS" >"$WORK/sums.tmp"
mv "$WORK/sums.tmp" "$WORK/www/dupsum/host-v$VERSION/SHA256SUMS"

printf '#!/bin/sh\necho "cronbeat 9.9.9 (linux/amd64)"\n' >"$WORK/wrong-version"
chmod 755 "$WORK/wrong-version"
make_release "$WORK/www/wrongversion" "$WORK/wrong-version"

printf '#!/bin/sh\nexit 1\n' >"$WORK/broken"
chmod 755 "$WORK/broken"
make_release "$WORK/www/broken" "$WORK/broken"

mkdir -p "$WORK/stage-extra"
cp "$WORK/cronbeat" "$WORK/stage-extra/cronbeat"
printf 'license text' >"$WORK/stage-extra/LICENSE"
printf 'must not be installed' >"$WORK/stage-extra/evil"
make_release "$WORK/www/extra"
rel="$WORK/www/extra/host-v$VERSION"
tar -czf "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz" -C "$WORK/stage-extra" cronbeat LICENSE evil
printf '%s  %s\n' "$(sha256_of "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz")" "cronbeat_${VERSION}_linux_amd64.tar.gz" >"$rel/SHA256SUMS"

make_release "$WORK/www/nomember"
rel="$WORK/www/nomember/host-v$VERSION"
tar -czf "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz" -C "$WORK/stage-extra" LICENSE
printf '%s  %s\n' "$(sha256_of "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz")" "cronbeat_${VERSION}_linux_amd64.tar.gz" >"$rel/SHA256SUMS"

make_release "$WORK/www/symlink"
rel="$WORK/www/symlink/host-v$VERSION"
mkdir -p "$WORK/stage-link"
rm -rf "$WORK/stage-link"/*
ln -s /bin/sh "$WORK/stage-link/cronbeat"
tar -czf "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz" -C "$WORK/stage-link" cronbeat
printf '%s  %s\n' "$(sha256_of "$rel/cronbeat_${VERSION}_linux_amd64.tar.gz")" "cronbeat_${VERSION}_linux_amd64.tar.gz" >"$rel/SHA256SUMS"

PORT="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/www" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
i=0
until python3 -c "import socket; socket.create_connection(('127.0.0.1', $PORT), 0.5).close()" 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 300 ] || {
    echo "the local server did not start"
    cat "$WORK/server.log"
    exit 1
  }
  sleep 0.1
done
BASE="http://127.0.0.1:$PORT"

# run_install NAME RELEASE [VAR=value ...]: runs install.sh in a clean home; output in $WORK/out, status in $STATUS.
run_install() {
  name="$1"
  release="$2"
  shift 2
  CASE="$WORK/case-$name"
  rm -rf "$CASE"
  mkdir -p "$CASE/home" "$CASE/tmp" "$CASE/system-bin"
  STATUS=0
  (
    cd "$CASE"
    env -i PATH="${TEST_PATH:-$PATH}" HOME="$CASE/home" TMPDIR="$CASE/tmp" \
      CRONBEAT_DOWNLOAD_BASE="$BASE/$release" CRONBEAT_VERSION="$VERSION" \
      CRONBEAT_SYSTEM_BIN_DIR="$CASE/system-bin" CRONBEAT_UNAME_S="Linux" CRONBEAT_UNAME_M="x86_64" \
      "$@" /bin/sh "$INSTALL"
  ) >"$CASE/out" 2>&1 || STATUS=$?
}

installed() {
  [ -f "$1/cronbeat" ] && [ -x "$1/cronbeat" ]
}

no_leftovers() {
  [ -z "$(ls -A "$CASE/tmp")" ]
}

expect_failure() {
  name="$1"
  pattern="$2"
  dir="${3:-$CASE/system-bin}"
  if [ "$STATUS" -eq 0 ]; then
    failed "$name" "expected a failure but the install succeeded"
  elif ! grep -q "$pattern" "$CASE/out"; then
    failed "$name" "message does not mention \"$pattern\": $(cat "$CASE/out")"
  elif installed "$dir" || installed "$CASE/home/.local/bin"; then
    failed "$name" "a binary was installed despite the failure"
  elif ! no_leftovers; then
    failed "$name" "temporary files were left behind: $(ls "$CASE/tmp")"
  else
    pass "$name"
  fi
}

# success amd64
run_install amd64 good
if [ "$STATUS" -ne 0 ]; then
  failed "install amd64" "$(cat "$CASE/out")"
elif ! installed "$CASE/system-bin"; then
  failed "install amd64" "binary missing: $(cat "$CASE/out")"
elif ! "$CASE/system-bin/cronbeat" version | grep -q "^cronbeat $VERSION "; then
  failed "install amd64" "installed binary does not report the version"
elif ! grep -q 'cronbeat configure' "$CASE/out" || ! grep -q 'cronbeat check' "$CASE/out"; then
  failed "install amd64" "next steps are missing: $(cat "$CASE/out")"
elif ! no_leftovers; then
  failed "install amd64" "temporary files were left behind"
elif [ -n "$(find "$CASE/system-bin" -name '.cronbeat*' 2>/dev/null)" ]; then
  failed "install amd64" "staging file left in the install directory"
else
  pass "install amd64 (x86_64)"
fi

# success arm64 via aarch64
run_install arm64 good CRONBEAT_UNAME_M="aarch64"
if [ "$STATUS" -eq 0 ] && installed "$CASE/system-bin" && grep -q 'linux/arm64' "$CASE/out"; then
  pass "install arm64 (aarch64)"
else
  failed "install arm64 (aarch64)" "$(cat "$CASE/out")"
fi

# explicit v prefix and install dir
run_install prefix good CRONBEAT_VERSION="v$VERSION" CRONBEAT_INSTALL_DIR="$WORK/case-prefix/custom"
if [ "$STATUS" -eq 0 ] && installed "$WORK/case-prefix/custom"; then
  pass "version with v prefix and CRONBEAT_INSTALL_DIR"
else
  failed "version with v prefix and CRONBEAT_INSTALL_DIR" "$(cat "$CASE/out")"
fi

# checksums with the binary marker
run_install star star
if [ "$STATUS" -eq 0 ] && installed "$CASE/system-bin"; then
  pass "SHA256SUMS with * binary marker"
else
  failed "SHA256SUMS with * binary marker" "$(cat "$CASE/out")"
fi

# upgrade over an existing install
run_install upgrade good
printf 'old binary' >"$CASE/system-bin/cronbeat"
STATUS=0
(
  cd "$CASE"
  env -i PATH="$PATH" HOME="$CASE/home" TMPDIR="$CASE/tmp" CRONBEAT_DOWNLOAD_BASE="$BASE/good" CRONBEAT_VERSION="$VERSION" \
    CRONBEAT_SYSTEM_BIN_DIR="$CASE/system-bin" CRONBEAT_UNAME_S="Linux" CRONBEAT_UNAME_M="x86_64" /bin/sh "$INSTALL"
) >"$CASE/out" 2>&1 || STATUS=$?
if [ "$STATUS" -eq 0 ] && "$CASE/system-bin/cronbeat" version | grep -q "^cronbeat $VERSION "; then
  pass "upgrade replaces the old binary"
else
  failed "upgrade replaces the old binary" "$(cat "$CASE/out")"
fi

# archive with extra members: only cronbeat is extracted and installed
run_install extra extra
if [ "$STATUS" -eq 0 ] && installed "$CASE/system-bin" && [ ! -e "$CASE/system-bin/evil" ] && [ ! -e "$CASE/system-bin/LICENSE" ]; then
  pass "only the cronbeat file is installed from the archive"
else
  failed "only the cronbeat file is installed from the archive" "$(cat "$CASE/out")"
fi

run_install tampered tampered
expect_failure "tampered tarball is rejected" "checksum mismatch"

run_install nosum nosum
expect_failure "missing SHA256SUMS entry is rejected" "SHA256SUMS"

run_install dupsum dupsum
expect_failure "duplicate SHA256SUMS entries are rejected" "SHA256SUMS"

run_install missing good CRONBEAT_VERSION="9.9.9"
expect_failure "missing asset (404) is reported" "download failed"

run_install arch good CRONBEAT_UNAME_M="armv7l"
expect_failure "unsupported architecture" "unsupported CPU architecture"

run_install os good CRONBEAT_UNAME_S="Darwin"
expect_failure "unsupported operating system" "Linux only"

run_install badversion good CRONBEAT_VERSION="0.1.0; rm -rf /"
expect_failure "malformed version is rejected" "invalid version"

run_install badbase good CRONBEAT_DOWNLOAD_BASE="http://example.com/releases"
expect_failure "plain http to a remote host is rejected" "https"

run_install wrongversion wrongversion
expect_failure "binary reporting another version is not installed" "expected version"

run_install broken broken
expect_failure "binary that cannot run is not installed" "does not run"

run_install nomember nomember
expect_failure "archive without cronbeat is rejected" "does not contain"

run_install symlink symlink
expect_failure "archive with a symlink as cronbeat is rejected" "regular file"

# no sha256 tool: a PATH with the basic tools but neither sha256sum nor shasum
NOSHA="$WORK/nosha-bin"
mkdir -p "$NOSHA"
for tool in curl wget tar awk mktemp uname id grep sed mv cp chmod rm mkdir tr cut head dirname basename cat ls tail sort; do
  if p="$(command -v "$tool" 2>/dev/null)" && [ -x "$p" ]; then
    ln -sf "$p" "$NOSHA/$tool"
  fi
done
TEST_PATH="$NOSHA" run_install nosha good
expect_failure "no sha256 tool fails closed" "sha256sum or shasum"

# non writable system directory falls back to ~/.local/bin
if [ "$(id -u)" = "0" ]; then
  printf 'SKIP system directory fallback (running as root)\n'
else
  rm -rf "$WORK/case-fallback"
  mkdir -p "$WORK/case-fallback/system-bin"
  chmod 555 "$WORK/case-fallback/system-bin"
  CASE="$WORK/case-fallback"
  mkdir -p "$CASE/home" "$CASE/tmp"
  STATUS=0
  (
    cd "$CASE"
    env -i PATH="$PATH" HOME="$CASE/home" TMPDIR="$CASE/tmp" CRONBEAT_DOWNLOAD_BASE="$BASE/good" CRONBEAT_VERSION="$VERSION" \
      CRONBEAT_SYSTEM_BIN_DIR="$CASE/system-bin" CRONBEAT_UNAME_S="Linux" CRONBEAT_UNAME_M="x86_64" /bin/sh "$INSTALL"
  ) >"$CASE/out" 2>&1 || STATUS=$?
  if [ "$STATUS" -eq 0 ] && installed "$CASE/home/.local/bin" && [ ! -e "$CASE/system-bin/cronbeat" ] &&
    grep -q 'export PATH=' "$CASE/out" && grep -q 'sudo' "$CASE/out"; then
    pass "non writable system directory falls back to ~/.local/bin with a PATH hint"
  else
    failed "non writable system directory falls back to ~/.local/bin" "$(cat "$CASE/out")"
  fi
fi

printf '\n%s passed, %s failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
