#!/usr/bin/env bash
# Install usher from a GitHub release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/nexustar/usher/main/install.sh | bash
#
# What it does:
#   1. Downloads the latest release binary to ~/.local/bin/usher (override with USHER_INSTALL_DIR)
#   2. Installs and (re)starts a user-level service (launchd on macOS, systemd on Linux)
#
# Re-run to upgrade.
#
# To uninstall:
#   usher setup --remove
#   # macOS:  launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/io.github.nexustar.usher.plist
#   # Linux:  systemctl --user disable --now usher
#   rm ~/.local/bin/usher

set -euo pipefail

REPO="nexustar/usher"
INSTALL_DIR="${USHER_INSTALL_DIR:-$HOME/.local/bin}"

# --- helpers ----------------------------------------------------------------

die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[34m==>\033[0m %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"
}

# --- detect platform --------------------------------------------------------

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"

  case "$os" in
    darwin) ;;
    linux)  ;;
    *)      die "unsupported OS: $os" ;;
  esac

  case "$arch" in
    x86_64)       arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *)            die "unsupported architecture: $arch" ;;
  esac

  PLATFORM="${os}-${arch}"
}

# --- download ---------------------------------------------------------------

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1"
  else
    shasum -a 256 "$1"
  fi | cut -d' ' -f1
}

download_binary() {
  need curl

  local base="https://github.com/${REPO}/releases/latest/download"
  local name="usher-${PLATFORM}"
  local bin="${INSTALL_DIR}/usher"
  mkdir -p "$INSTALL_DIR"

  # rename over the running binary; writing it in place fails (text file busy)
  TMP_BIN="$(mktemp "${INSTALL_DIR}/.usher-install.XXXXXX")"
  trap 'rm -f "$TMP_BIN"' EXIT

  info "Downloading ${name} → ${bin}"
  curl -fSL --progress-bar "${base}/${name}" -o "$TMP_BIN"

  local want got
  want="$(curl -fsSL "${base}/checksums.txt" | awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }')"
  [ -n "$want" ] || die "no checksum for ${name} in the release's checksums.txt"
  got="$(sha256_of "$TMP_BIN")"
  [ "$got" = "$want" ] || die "checksum mismatch for ${name} (got ${got}, want ${want})"

  chmod 755 "$TMP_BIN"
  if [ -f "$bin" ]; then
    cp -p "$bin" "${bin}.prev"
    info "Previous binary kept as ${bin}.prev"
  fi
  mv -f "$TMP_BIN" "$bin"
}

# --- ensure PATH ------------------------------------------------------------

ensure_path() {
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) return ;;
  esac
  export PATH="${INSTALL_DIR}:${PATH}"

  local line="export PATH=\"${INSTALL_DIR}:\$PATH\""
  local rc=""
  case "${SHELL:-}" in
    */zsh)  rc="$HOME/.zshrc"   ;;
    */bash) rc="$HOME/.bashrc"  ;;
  esac
  if [ -z "$rc" ] || [ ! -f "$rc" ]; then
    for f in "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.zshrc" "$HOME/.zprofile" "$HOME/.profile"; do
      [ -f "$f" ] && rc="$f" && break
    done
  fi
  if [ -n "$rc" ]; then
    grep -qF "$INSTALL_DIR" "$rc" 2>/dev/null || printf '\n%s\n' "$line" >> "$rc"
    info "Added ${INSTALL_DIR} to PATH in ${rc}"
  else
    info "Add to your shell profile: ${line}"
  fi
}

# --- service environment ----------------------------------------------------

# service_path is this terminal's PATH, install dir first, deduplicated.
service_path() {
  local out="$INSTALL_DIR" dir dirs
  IFS=: read -r -a dirs <<< "$PATH"
  for dir in ${dirs[@]+"${dirs[@]}"}; do
    [ -n "$dir" ] || continue
    case ":${out}:" in *":${dir}:"*) continue ;; esac
    out="${out}:${dir}"
  done
  printf '%s' "$out"
}

# warn_unshared_env names variables the service won't get; names only, as
# values may be secrets.
warn_unshared_env() {
  local names
  names="$(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' |
    grep -E '^([A-Za-z]+_(PROXY|proxy)|NODE_EXTRA_CA_CERTS|ANTHROPIC_[A-Z0-9_]+|OPENAI_[A-Z0-9_]+|CLAUDE_CODE_USE_[A-Z0-9_]+|AWS_[A-Z0-9_]+)$' |
    sort -u | tr '\n' ' ' || true)"
  [ -n "$names" ] || return 0
  info "Set in this shell but not passed to the service: ${names}"
  info "  If the agents need them, add them to $1 (secrets in a file only you can read)"
}

# --- service install --------------------------------------------------------

install_service_darwin() {
  local plist_dir="$HOME/Library/LaunchAgents"
  local plist="${plist_dir}/io.github.nexustar.usher.plist"
  local bin="${INSTALL_DIR}/usher"

  mkdir -p "$plist_dir"

  local svc_path
  svc_path="$(service_path | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')"

  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>io.github.nexustar.usher</string>
  <key>ProgramArguments</key>
  <array>
    <string>${bin}</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/usher.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/usher.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${svc_path}</string>
  </dict>
</dict>
</plist>
EOF

  launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  info "LaunchAgent installed and started"
  warn_unshared_env "$plist"
  info "  Logs: /tmp/usher.log"
  info "  Stop: launchctl bootout gui/$(id -u) ${plist}"
}

install_service_linux() {
  local unit_dir="$HOME/.config/systemd/user"
  local unit="${unit_dir}/usher.service"
  local bin="${INSTALL_DIR}/usher"

  need systemctl

  local svc_path
  svc_path="$(service_path | sed -e 's/%/%%/g')"

  mkdir -p "$unit_dir"

  cat > "$unit" <<EOF
[Unit]
Description=usher — Claude/Codex session router
After=network.target

[Service]
Type=simple
ExecStart=${bin} serve
Restart=on-failure
RestartSec=3
Environment="PATH=${svc_path}"

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable usher
  systemctl --user restart usher  # `enable --now` is a no-op if already running
  if [ "$(loginctl show-user "$USER" --property=Linger 2>/dev/null)" != "Linger=yes" ]; then
    info "To keep usher running after logout: sudo loginctl enable-linger $USER"
  fi
  info "systemd user service installed and started"
  warn_unshared_env "$unit"
  info "  Logs: journalctl --user -u usher -f"
  info "  Stop: systemctl --user disable --now usher"
}

install_service() {
  case "$(uname -s)" in
    Darwin) install_service_darwin ;;
    Linux)  install_service_linux  ;;
  esac
}

# --- main -------------------------------------------------------------------

main() {
  detect_platform
  download_binary
  ensure_path
  install_service

  echo
  info "Done! usher is running on http://127.0.0.1:7777"
  info "Set a password for remote access: usher set-password"
}

main "$@"
