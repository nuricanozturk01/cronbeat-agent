#!/bin/sh
# Installs the cronbeat host CLI on Linux (amd64 or arm64).
#
#   curl -fsSL https://docs.cronbeat.dev/install.sh | sh
#
# Settings (environment variables):
#   CRONBEAT_VERSION         version to install, default is DEFAULT_VERSION below
#   CRONBEAT_DOWNLOAD_BASE   release download URL, default is the GitHub releases of cronbeat-agent
#   CRONBEAT_INSTALL_DIR     install directory, default /usr/local/bin when it is writable, else ~/.local/bin
#
# The tarball is checked against SHA256SUMS before anything is installed. The only
# downloaded program that is ever run is "cronbeat version", to confirm the install.
set -eu

# The release process bumps this value together with internal/hostversion.
DEFAULT_VERSION="0.1.0"
DEFAULT_DOWNLOAD_BASE="https://github.com/nuricanozturk01/cronbeat-agent/releases/download"

VERSION="${CRONBEAT_VERSION:-$DEFAULT_VERSION}"
VERSION="${VERSION#v}"
DOWNLOAD_BASE="${CRONBEAT_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
DOWNLOAD_BASE="${DOWNLOAD_BASE%/}"
SYSTEM_BIN_DIR="${CRONBEAT_SYSTEM_BIN_DIR:-/usr/local/bin}"

TMP_DIR=""

say() {
  printf '%s\n' "$*"
}

fail() {
  printf 'cronbeat install: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}

have() {
  command -v "$1" >/dev/null 2>&1
}

detect_arch() {
  os="${CRONBEAT_UNAME_S:-$(uname -s)}"
  machine="${CRONBEAT_UNAME_M:-$(uname -m)}"
  if [ "$os" != "Linux" ]; then
    fail "the host CLI is available for Linux only (this system is $os)"
  fi
  case "$machine" in
    x86_64 | amd64) ARCH="amd64" ;;
    aarch64 | arm64) ARCH="arm64" ;;
    *) fail "unsupported CPU architecture $machine (supported: x86_64, aarch64)" ;;
  esac
}

check_version() {
  case "$VERSION" in
    "" | *[!0-9A-Za-z.+-]* | .* | *..*) fail "invalid version \"$VERSION\"" ;;
  esac
  case "$VERSION" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) fail "invalid version \"$VERSION\", expected something like 0.1.0" ;;
  esac
}

fetch() {
  url="$1"
  out="$2"
  if have curl; then
    case "$url" in
      https://*) curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 300 --retry 2 -o "$out" "$url" ;;
      *) curl -fsSL --connect-timeout 15 --max-time 300 --retry 2 -o "$out" "$url" ;;
    esac
  elif have wget; then
    wget -q -O "$out" "$url"
  else
    fail "curl or wget is required to download the release"
  fi || fail "download failed: $url"
}

check_download_base() {
  case "$DOWNLOAD_BASE" in
    https://*) ;;
    http://127.0.0.1 | http://127.0.0.1:* | http://localhost | http://localhost:* | "http://[::1]" | "http://[::1]:"*) ;;
    *) fail "the download base must use https (http is only accepted for localhost)" ;;
  esac
}

sha256_of() {
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "sha256sum or shasum is required to verify the download, nothing was installed"
  fi
}

expected_checksum() {
  awk -v name="$1" '
    { file = $2; sub(/^\*/, "", file) }
    file == name { print $1; count++ }
    END { if (count != 1) exit 1 }
  ' "$2"
}

verify_checksum() {
  asset="$1"
  archive="$2"
  sums="$3"
  expected="$(expected_checksum "$asset" "$sums")" || fail "SHA256SUMS has no single entry for $asset, nothing was installed"
  case "$expected" in
    *[!0-9a-fA-F]*) fail "SHA256SUMS has an invalid entry for $asset" ;;
  esac
  [ "${#expected}" -eq 64 ] || fail "SHA256SUMS has an invalid entry for $asset"
  actual="$(sha256_of "$archive")" || exit 1
  expected="$(printf '%s' "$expected" | tr 'A-F' 'a-f')"
  if [ "$actual" != "$expected" ]; then
    fail "checksum mismatch for $asset (expected $expected, got $actual), nothing was installed"
  fi
}

extract_binary() {
  archive="$1"
  dest="$2"
  members="$(tar -tzf "$archive")" || fail "cannot read the archive"
  printf '%s\n' "$members" | grep -qx 'cronbeat' || fail "the archive does not contain a cronbeat file"
  mkdir -p "$dest"
  tar -xzf "$archive" -C "$dest" cronbeat || fail "cannot extract the archive"
  if [ ! -f "$dest/cronbeat" ] || [ -L "$dest/cronbeat" ]; then
    fail "the archive entry cronbeat is not a regular file"
  fi
  chmod 755 "$dest/cronbeat"
}

choose_install_dir() {
  if [ -n "${CRONBEAT_INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$CRONBEAT_INSTALL_DIR"
    return
  fi
  if [ -d "$SYSTEM_BIN_DIR" ] && [ -w "$SYSTEM_BIN_DIR" ]; then
    INSTALL_DIR="$SYSTEM_BIN_DIR"
    return
  fi
  if [ "$(id -u)" = "0" ] && mkdir -p "$SYSTEM_BIN_DIR" 2>/dev/null; then
    INSTALL_DIR="$SYSTEM_BIN_DIR"
    return
  fi
  [ -n "${HOME:-}" ] || fail "no write access to $SYSTEM_BIN_DIR and HOME is not set, set CRONBEAT_INSTALL_DIR"
  INSTALL_DIR="$HOME/.local/bin"
  FELL_BACK=1
}

install_binary() {
  src="$1"
  mkdir -p "$INSTALL_DIR" || fail "cannot create $INSTALL_DIR"
  staged="$INSTALL_DIR/.cronbeat.new.$$"
  if ! cp "$src" "$staged" 2>/dev/null; then
    rm -f "$staged"
    fail "cannot write to $INSTALL_DIR (try again with sudo, or set CRONBEAT_INSTALL_DIR)"
  fi
  chmod 755 "$staged"
  if ! mv -f "$staged" "$INSTALL_DIR/cronbeat"; then
    rm -f "$staged"
    fail "cannot replace $INSTALL_DIR/cronbeat"
  fi
}

on_path() {
  case ":${PATH:-}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

main() {
  FELL_BACK=""
  check_version
  check_download_base
  detect_arch

  asset="cronbeat_${VERSION}_linux_${ARCH}.tar.gz"
  release_url="$DOWNLOAD_BASE/host-v${VERSION}"

  TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cronbeat-install.XXXXXX")" || fail "cannot create a temporary directory"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP

  say "Downloading cronbeat $VERSION (linux/$ARCH)"
  fetch "$release_url/$asset" "$TMP_DIR/$asset"
  fetch "$release_url/SHA256SUMS" "$TMP_DIR/SHA256SUMS"
  verify_checksum "$asset" "$TMP_DIR/$asset" "$TMP_DIR/SHA256SUMS"
  say "Checksum verified"

  extract_binary "$TMP_DIR/$asset" "$TMP_DIR/extract"
  reported="$("$TMP_DIR/extract/cronbeat" version 2>/dev/null)" || fail "the downloaded binary does not run on this system, nothing was installed"
  case "$reported" in
    "cronbeat $VERSION "* | "cronbeat $VERSION") ;;
    *) fail "the downloaded binary reports \"$reported\", expected version $VERSION, nothing was installed" ;;
  esac

  choose_install_dir
  install_binary "$TMP_DIR/extract/cronbeat"

  say "Installed $INSTALL_DIR/cronbeat ($reported)"
  if [ -n "$FELL_BACK" ]; then
    say "No write access to $SYSTEM_BIN_DIR, so it went to $INSTALL_DIR. For a system wide install run the installer with sudo."
  fi
  if ! on_path "$INSTALL_DIR"; then
    say "Add $INSTALL_DIR to your PATH, for example: export PATH=\"$INSTALL_DIR:\$PATH\""
  fi
  say ""
  say "Next steps:"
  say "  cronbeat configure    # store the API key of your host agent (read from the CronBeat app)"
  say "  cronbeat check        # verify the key"
  say "  cronbeat run --name my-job --schedule \"0 2 * * *\" -- /path/to/job.sh"
}

main "$@"
