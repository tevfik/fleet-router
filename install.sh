#!/bin/sh
# fleet installer — one line:
#   curl -fsSL https://raw.githubusercontent.com/tevfik/fleet-router/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/tevfik/fleet-router/main/install.sh | sudo sh -s -- --system
#
# Options (after `sh -s --`):
#   --system        install to /usr/local/bin and a systemd system service (needs root; runs as the sudo user)
#   --service       user install + a systemd *user* service (starts the daemon)
#   --version vX.Y  a specific release (default: latest)
# Env: FLEET_BASE (release base URL), FLEET_DIR (target directory),
#      FLEET_TOKEN (Gitea access token — required while the repository is private; it is also saved as
#      ~/.config/fleet/update_token so `fleet update` and the daemon's auto-update can authenticate)
set -eu

REPO="${FLEET_REPO:-https://github.com/tevfik/fleet-router}"
case "$REPO" in https://github.com/*) FLEET_TOKEN= ;; esac   # the token is for a private Gitea, never sent to GitHub
system=0 service=0 version=latest
while [ $# -gt 0 ]; do
  case "$1" in
    --system) system=1 ;;
    --service) service=1 ;;
    --version) shift; version="$1" ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(uname -s)" = Linux ] || { echo "fleet releases are Linux only" >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fetch() {
  if command -v curl >/dev/null; then
    curl -fsSL ${FLEET_TOKEN:+-H "Authorization: token $FLEET_TOKEN"} "$1" -o "$2"
  else
    wget -qO "$2" ${FLEET_TOKEN:+--header="Authorization: token $FLEET_TOKEN"} "$1"
  fi
}

if [ "$version" = latest ] && [ -z "${FLEET_BASE:-}" ]; then   # {"tag_name":"v1.2.3",…} (Gitea or GitHub API)
  case "$REPO" in
    https://github.com/*) api="https://api.github.com/repos/${REPO#https://github.com/}/releases/latest" ;;
    *) api="${REPO%/*/*}/api/v1/repos/${REPO#*://*/}/releases/latest" ;;
  esac
  fetch "$api" "$tmp/latest.json" || { echo "cannot find the latest release ($api)" >&2; exit 1; }
  version="$(tr -d '\n' < "$tmp/latest.json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')"
  [ -n "$version" ] || { echo "no release published yet" >&2; exit 1; }
fi
base="${FLEET_BASE:-$REPO/releases/download/$version}"
name="fleet-linux-$arch"

echo "▸ downloading $name ($version)"
fetch "$base/$name" "$tmp/$name"
if fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
  (cd "$tmp" && grep " $name\$" SHA256SUMS | sha256sum -c -) || { echo "checksum mismatch" >&2; exit 1; }
else
  echo "  (no SHA256SUMS published — skipping verification)"
fi
chmod +x "$tmp/$name"

if [ "$system" = 1 ]; then
  [ "$(id -u)" = 0 ] || { echo "--system needs root: curl … | sudo sh -s -- --system" >&2; exit 1; }
  "$tmp/$name" setup all --system
else
  dir="${FLEET_DIR:-$HOME/.local/bin}"
  mkdir -p "$dir"
  install -m 0755 "$tmp/$name" "$dir/fleet"
  echo "✓ installed: $dir/fleet"
  case ":$PATH:" in *":$dir:"*) ;; *) echo "  note: $dir is not in PATH" ;; esac
  if [ "$service" = 1 ]; then "$dir/fleet" setup service; fi
fi
if [ -n "${FLEET_TOKEN:-}" ]; then   # for `fleet update` / auto-update on a private repository
  if [ "$system" = 1 ] && [ -n "${SUDO_USER:-}" ]; then cfgdir="$(getent passwd "$SUDO_USER" | cut -d: -f6)/.config/fleet"; else cfgdir="${XDG_CONFIG_HOME:-$HOME/.config}/fleet"; fi
  mkdir -p "$cfgdir" && (umask 077; printf '%s\n' "$FLEET_TOKEN" > "$cfgdir/update_token")
  [ "$system" = 1 ] && [ -n "${SUDO_USER:-}" ] && chown -R "$SUDO_USER" "$cfgdir" 2>/dev/null || true
  echo "✓ token saved: $cfgdir/update_token"
fi
echo "next: fleet            (menu; first run discovers your models)  ·  fleet doctor"
