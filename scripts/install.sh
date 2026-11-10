#!/usr/bin/env sh
# Permafrost (pfrost) — One-Line Universal Installer for Linux & macOS
# Usage: curl -fsSL https://raw.githubusercontent.com/amglogicalis/permafrost/main/scripts/install.sh | sh

set -e

REPO="amglogicalis/permafrost"
BINARY="pfrost"
INSTALL_DIR="${PFROST_INSTALL_DIR:-$HOME/.local/bin}"

printf "\033[36m\033[1m❄ Installing Permafrost (pfrost)...\033[0m\n"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux*)  OS="linux" ;;
  darwin*) OS="darwin" ;;
  *)
    printf "\033[31m✖ Unsupported operating system: %s\033[0m\n" "$OS"
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    printf "\033[31m✖ Unsupported CPU architecture: %s\033[0m\n" "$ARCH"
    exit 1
    ;;
esac

mkdir -p "$INSTALL_DIR"

# If running from local repository checkout with --local, build directly
if [ "$1" = "--local" ] && [ -f "./cmd/pfrost/main.go" ]; then
  printf "  • Compiling %s locally for %s/%s...\n" "$BINARY" "$OS" "$ARCH"
  go build -ldflags="-s -w" -o "$INSTALL_DIR/$BINARY" ./cmd/pfrost
  chmod +x "$INSTALL_DIR/$BINARY"
  printf "\033[32m✔ Installed %s to %s/%s\033[0m\n" "$BINARY" "$INSTALL_DIR" "$BINARY"
  exit 0
fi

# Fetch latest release tag from GitHub API
LATEST_TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)"

if [ -z "$LATEST_TAG" ]; then
  if command -v go >/dev/null 2>&1; then
    printf "  • Installing via 'go install github.com/%s/cmd/pfrost@latest'...\n" "$REPO"
    GOBIN="$INSTALL_DIR" go install "github.com/$REPO/cmd/pfrost@latest"
    printf "\033[32m✔ Installed %s to %s/%s\033[0m\n" "$BINARY" "$INSTALL_DIR" "$BINARY"
    exit 0
  fi
  printf "\033[31m✖ Could not fetch latest release from GitHub (%s).\033[0m\n" "$REPO"
  exit 1
fi

VERSION="${LATEST_TAG#v}"
ARCHIVE="${BINARY}_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$LATEST_TAG/$ARCHIVE"
CHECKSUMS_URL="https://github.com/$REPO/releases/download/$LATEST_TAG/checksums.txt"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

printf "  • Downloading %s (%s/%s)...\n" "$LATEST_TAG" "$OS" "$ARCH"
curl -fsSL "$DOWNLOAD_URL" -o "$TMP_DIR/$ARCHIVE"
curl -fsSL "$CHECKSUMS_URL" -o "$TMP_DIR/checksums.txt"

# Verify SHA-256 checksum
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMP_DIR" && grep "$ARCHIVE" checksums.txt | sha256sum -c - >/dev/null)
  printf "  • SHA-256 checksum verified ✔\n"
elif command -v shasum >/dev/null 2>&1; then
  (cd "$TMP_DIR" && grep "$ARCHIVE" checksums.txt | shasum -a 256 -c - >/dev/null)
  printf "  • SHA-256 checksum verified ✔\n"
fi

tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"
mv "$TMP_DIR/$BINARY" "$INSTALL_DIR/$BINARY"
chmod +x "$INSTALL_DIR/$BINARY"

printf "\n\033[32m✔ Permafrost (pfrost %s) installed successfully!\033[0m\n" "$LATEST_TAG"
printf "  • Location: %s/%s\n" "$INSTALL_DIR" "$BINARY"
printf "  • Run '\033[36mpfrost\033[0m' to launch the interactive TUI.\n"
