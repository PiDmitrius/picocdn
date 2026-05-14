#!/bin/bash
set -euo pipefail

# picocdn installer
# Usage: curl -fsSL https://raw.githubusercontent.com/PiDmitrius/picocdn/main/install.sh | bash

REPO="PiDmitrius/picocdn"
INSTALL_DIR="$HOME/.local/bin"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[x]${NC} $*"; exit 1; }
tilde() { echo "$1" | sed "s|^$HOME|~|"; }

ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    *)       fail "unsupported architecture: $ARCH" ;;
esac

info "checking latest version..."
VERSION=$(curl -sfI "https://github.com/${REPO}/releases/latest" | grep -i ^location: | sed 's|.*/v||' | tr -d '\r')
if [ -z "$VERSION" ]; then
    fail "could not determine latest version"
fi
TAG="v${VERSION}"
info "latest: ${TAG}"

URL="https://github.com/${REPO}/releases/download/${TAG}/picocdn-${TAG}-linux-${ARCH}"
info "downloading picocdn-${TAG}-linux-${ARCH}..."

mkdir -p "$INSTALL_DIR"
if ! curl -sfL "$URL" -o "${INSTALL_DIR}/picocdn"; then
    fail "download failed: ${URL}"
fi
chmod +x "${INSTALL_DIR}/picocdn"
info "installed: $(tilde "${INSTALL_DIR}/picocdn")"

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    warn "$(tilde "$INSTALL_DIR") is not in your PATH"

    SHELL_NAME=$(basename "$SHELL")
    case "$SHELL_NAME" in
        bash) RC="$HOME/.bashrc" ;;
        zsh)  RC="$HOME/.zshrc" ;;
        *)    RC="" ;;
    esac

    EXPORT_LINE="export PATH=\"${INSTALL_DIR}:\$PATH\""

    if [ -n "$RC" ] && [ -f "$RC" ]; then
        if ! grep -qF "$INSTALL_DIR" "$RC"; then
            echo "" >> "$RC"
            echo "# Added by picocdn installer" >> "$RC"
            echo "$EXPORT_LINE" >> "$RC"
            info "added to $(tilde "$RC"): ${EXPORT_LINE}"
            warn "run: source $(tilde "$RC")"
        fi
    else
        warn "add to your shell profile: ${EXPORT_LINE}"
    fi

    export PATH="${INSTALL_DIR}:$PATH"
fi

SHELL_NAME=$(basename "$SHELL")
case "$SHELL_NAME" in
    bash) RC="$HOME/.bashrc" ;;
    zsh)  RC="$HOME/.zshrc" ;;
    *)    RC="" ;;
esac

if [ -n "$RC" ] && [ -f "$RC" ]; then
    if ! grep -qF "XDG_RUNTIME_DIR" "$RC"; then
        cat >> "$RC" << 'XDGEOF'

# systemd user session support (added by picocdn installer)
if [ -S "/run/user/$(id -u)/bus" ]; then
  export XDG_RUNTIME_DIR="/run/user/$(id -u)"
  export DBUS_SESSION_BUS_ADDRESS="unix:path=${XDG_RUNTIME_DIR}/bus"
fi
XDGEOF
        info "added systemd user session support to $(tilde "$RC")"
    fi
fi

if command -v loginctl >/dev/null 2>&1; then
    LINGER=$(loginctl show-user "$(whoami)" --property=Linger 2>/dev/null | cut -d= -f2 || true)
    if [ "$LINGER" != "yes" ]; then
        if loginctl enable-linger "$(whoami)" 2>/dev/null; then
            info "enabled systemd linger"
        else
            warn "run: sudo loginctl enable-linger $(whoami)"
        fi
    fi
fi

if ! command -v picocdn >/dev/null 2>&1; then
    fail "picocdn not found in PATH after install"
fi

info "$("${INSTALL_DIR}/picocdn" version) installed successfully"

echo ""
echo "Next steps:"
echo "  source ~/.bashrc"
echo "  picocdn install"
echo "  picocdn namespace create --auth-file ~/.local/share/picocdn/auth.json default"
echo "  picocdn start"
