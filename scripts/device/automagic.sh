#!/bin/sh
# Deprecated. Kept as a shim for anyone who knows this path.
# The replacement runs on the operator's workstation (not on the tablet)
# and reads a device.env emitted by scripts/install.sh.
set -e

cat >&2 <<'EOF'
scripts/device/automagic.sh is deprecated.

Use scripts/device-setup.sh from your workstation instead:
  scripts/device-setup.sh --env-file ./device.env

That script auto-detects rm1/rm2 vs Paper Pro, pins the upstream installer
version, and verifies the tablet can reach /health on your STORAGE_URL.

Continuing with the legacy behavior...
EOF

INSTALLER="installer.sh"
REPOURL="https://github.com/ddvk/rmfakecloud-proxy/releases/download/v0.0.4/${INSTALLER}"
wget "$REPOURL" -O "${INSTALLER}"
chmod +x "./${INSTALLER}"
"./${INSTALLER}" install
