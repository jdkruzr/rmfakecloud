#!/usr/bin/env bash
#
# scripts/device-setup.sh — operator-workstation companion to install.sh.
#
# Reads a device.env (typically the one emitted by install.sh), detects
# whether the tablet is rm1/rm2 or Paper Pro, downloads the matching
# installer from ddvk/rmfakecloud-proxy, scp's it (plus an optional CA
# cert) to the tablet, runs it, and verifies the tablet can reach the
# server. Hosts edits, CA cert install, and xochitl restart are the
# upstream installer's job — not ours.
#
# Run this from a workstation that has SSH access to the tablet (USB
# ethernet works out of the box: 10.11.99.1).

set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/common.sh
. "$REPO_ROOT/scripts/lib/common.sh"

# Defaults --------------------------------------------------------------------
ENV_FILE="./device.env"
DEVICE_IP=""
DEVICE_USER=""
SSH_KEY=""
MODEL=""
CA_CERT=""
INSTALLER_VERSION=""
INSTALLER_SHA256=""
KEEP_PAYLOAD=""
DRY_RUN=0

usage() {
    cat <<'EOF'
scripts/device-setup.sh — point a reMarkable tablet at a self-hosted rmfakecloud.

Usage: scripts/device-setup.sh [flags]

Flags:
  --env-file PATH          device.env emitted by install.sh (default: ./device.env).
  --device-ip IP           Tablet IP (default: 10.11.99.1).
  --device-user USER       SSH user on the tablet (default: root).
  --ssh-key PATH           SSH private key (default: ssh-agent / ~/.ssh/id_rsa).
  --model {auto,rm1,rm2,rmpro}   Override device model detection.
  --ca-cert PATH           CA cert the tablet must trust (operator-provided).
  --installer-version TAG  Pinned upstream release (default: v0.0.6).
  --keep-payload           Don't delete the staged installer from the tablet.
  --dry-run                Show what would happen; don't touch the tablet.
  -h, --help               This message.

Most operators just run:
  scripts/device-setup.sh --env-file device.env --ca-cert /path/to/ca.pem
EOF
}

parse_args() {
    while [ $# -gt 0 ]; do
        case $1 in
            --env-file)            ENV_FILE=$2; shift 2;;
            --env-file=*)          ENV_FILE=${1#*=}; shift;;
            --device-ip)           DEVICE_IP=$2; shift 2;;
            --device-ip=*)         DEVICE_IP=${1#*=}; shift;;
            --device-user)         DEVICE_USER=$2; shift 2;;
            --device-user=*)       DEVICE_USER=${1#*=}; shift;;
            --ssh-key)             SSH_KEY=$2; shift 2;;
            --ssh-key=*)           SSH_KEY=${1#*=}; shift;;
            --model)               MODEL=$2; shift 2;;
            --model=*)             MODEL=${1#*=}; shift;;
            --ca-cert)             CA_CERT=$2; shift 2;;
            --ca-cert=*)           CA_CERT=${1#*=}; shift;;
            --installer-version)   INSTALLER_VERSION=$2; shift 2;;
            --installer-version=*) INSTALLER_VERSION=${1#*=}; shift;;
            --keep-payload)        KEEP_PAYLOAD=1; shift;;
            --dry-run)             DRY_RUN=1; shift;;
            -h|--help)             usage; exit 0;;
            *) die "unknown flag: $1 (try --help)";;
        esac
    done
    export DRY_RUN
}

# ---- 1. load env ------------------------------------------------------------
load_env() {
    # Stash CLI values before sourcing the env file — `set -a; . file; set +a`
    # exports every assignment in the file, which would otherwise clobber any
    # variable a flag just set. We reapply CLI values after sourcing so the
    # documented precedence (CLI > env file > default) actually holds.
    local cli_device_ip=$DEVICE_IP cli_device_user=$DEVICE_USER cli_ssh_key=$SSH_KEY
    local cli_model=$MODEL cli_ca_cert=$CA_CERT cli_installer_version=$INSTALLER_VERSION
    local cli_keep_payload=$KEEP_PAYLOAD

    if [ -e "$ENV_FILE" ]; then
        log "loading $ENV_FILE"
        # shellcheck disable=SC1090
        set -a; . "$ENV_FILE"; set +a
    elif [ "$ENV_FILE" != "./device.env" ]; then
        die "env file not found: $ENV_FILE"
    fi
    DEVICE_IP=${cli_device_ip:-${DEVICE_IP:-10.11.99.1}}
    DEVICE_USER=${cli_device_user:-${DEVICE_USER:-root}}
    SSH_KEY=${cli_ssh_key:-${SSH_KEY:-}}
    MODEL=${cli_model:-${MODEL:-auto}}
    CA_CERT=${cli_ca_cert:-${CA_CERT:-}}
    INSTALLER_VERSION=${cli_installer_version:-${INSTALLER_VERSION:-v0.0.6}}
    KEEP_PAYLOAD=${cli_keep_payload:-${KEEP_PAYLOAD:-0}}
    [ -n "${STORAGE_URL:-}" ] || die "STORAGE_URL not set (in $ENV_FILE or env). Pass --env-file or set it."
    log "STORAGE_URL=$STORAGE_URL  device=$DEVICE_USER@$DEVICE_IP  model=$MODEL"
}

ssh_opts() {
    # Echo SSH options as a positional list (caller wraps with eval / array).
    local opts=(-o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new)
    [ -n "$SSH_KEY" ] && opts+=(-i "$SSH_KEY")
    printf '%q ' "${opts[@]}"
}

ssh_run() {
    # Usage: ssh_run "remote command"
    local opts; read -r -a opts <<<"$(ssh_opts)"
    if [ "$DRY_RUN" = 1 ]; then
        log "[dry-run] ssh ${opts[*]} $DEVICE_USER@$DEVICE_IP $*"
        return 0
    fi
    ssh "${opts[@]}" "$DEVICE_USER@$DEVICE_IP" "$@"
}

scp_to() {
    # Usage: scp_to LOCAL_PATH REMOTE_PATH
    local opts; read -r -a opts <<<"$(ssh_opts)"
    if [ "$DRY_RUN" = 1 ]; then
        log "[dry-run] scp ${opts[*]} $1 $DEVICE_USER@$DEVICE_IP:$2"
        return 0
    fi
    scp "${opts[@]}" "$1" "$DEVICE_USER@$DEVICE_IP:$2"
}

# ---- 2. ssh check -----------------------------------------------------------
check_ssh() {
    info "checking SSH to $DEVICE_USER@$DEVICE_IP"
    if ssh_run "true"; then
        log "SSH ok"
        return 0
    fi
    cat >&2 <<EOF

Could not SSH to $DEVICE_USER@$DEVICE_IP.

Common fixes:
  - Plug the tablet in via USB and wait for the USB-ethernet interface
    to come up (default IP 10.11.99.1).
  - On the tablet: Settings > Help > About > Copyrights and licenses
    will show an SSH password; use it once, then install your key with
    ssh-copy-id.
  - If using --ssh-key, double-check the path: $SSH_KEY.

EOF
    die "SSH check failed"
}

# ---- 3. detect model --------------------------------------------------------
detect_model() {
    if [ "$MODEL" != "auto" ]; then
        case $MODEL in
            rm1|rm2|rmpro) log "model: $MODEL (from flag/env)"; return 0;;
            *) die "invalid --model: $MODEL (must be rm1, rm2, rmpro, or auto)";;
        esac
    fi
    info "detecting tablet model"
    local machine
    machine=$(ssh_run "cat /sys/devices/soc0/machine 2>/dev/null || cat /proc/device-tree/model 2>/dev/null || true")
    machine=${machine%%$'\n'*}
    log "machine: ${machine:-(empty)}"
    case $machine in
        *"reMarkable Prototype 1"*|*"reMarkable 1"*) MODEL=rm1;;
        *"reMarkable 2"*) MODEL=rm2;;
        *"reMarkable Ferrari"*|*"reMarkable Chiappa"*|*"Paper Pro"*) MODEL=rmpro;;
        *)
            warn "could not auto-detect model from: $machine"
            MODEL=$(prompt "Enter model (rm1|rm2|rmpro)" "rm2")
            ;;
    esac
    log "model: $MODEL"
}

# ---- 4. pick installer ------------------------------------------------------
pick_installer() {
    case $MODEL in
        rm1|rm2) INSTALLER="installer-rm12.sh";;
        rmpro)   INSTALLER="installer-rmpro.sh";;
        *) die "no installer mapping for model: $MODEL";;
    esac
    INSTALLER_URL="https://github.com/ddvk/rmfakecloud-proxy/releases/download/$INSTALLER_VERSION/$INSTALLER"
    log "installer: $INSTALLER from $INSTALLER_VERSION"
}

# ---- 5. fetch installer -----------------------------------------------------
fetch_installer() {
    STAGE_DIR=$(mktemp -d -t rmfakecloud-device.XXXXXX)
    trap 'rm -rf "$STAGE_DIR"' EXIT
    info "downloading $INSTALLER_URL"
    if [ "$DRY_RUN" = 1 ]; then
        log "[dry-run] would download to $STAGE_DIR/$INSTALLER"
        printf '#!/bin/sh\n# placeholder\n' >"$STAGE_DIR/$INSTALLER"
    else
        curl -fsSLo "$STAGE_DIR/$INSTALLER" "$INSTALLER_URL" \
            || die "download failed: $INSTALLER_URL"
    fi
    [ "$DRY_RUN" = 1 ] || [ -s "$STAGE_DIR/$INSTALLER" ] || die "installer download is empty"
    if [ "$DRY_RUN" = 0 ] && ! head -c 2 "$STAGE_DIR/$INSTALLER" | grep -q '#!'; then
        die "installer does not look like a shell script (missing shebang)"
    fi
    if [ -n "$INSTALLER_SHA256" ]; then
        info "verifying sha256"
        local got
        got=$(sha256sum "$STAGE_DIR/$INSTALLER" | cut -d' ' -f1)
        [ "$got" = "$INSTALLER_SHA256" ] || die "sha256 mismatch: got $got, expected $INSTALLER_SHA256"
        log "sha256 verified"
    fi
}

# ---- 6. stage payload -------------------------------------------------------
stage_payload() {
    info "scp'ing installer to tablet"
    scp_to "$STAGE_DIR/$INSTALLER" "/home/root/$INSTALLER"
    if [ -n "$CA_CERT" ]; then
        [ -r "$CA_CERT" ] || die "CA cert not readable: $CA_CERT"
        info "scp'ing CA cert to tablet"
        local cert_name; cert_name=$(basename "$CA_CERT")
        scp_to "$CA_CERT" "/home/root/$cert_name"
        REMOTE_CA_CERT="/home/root/$cert_name"
    else
        warn "no --ca-cert supplied; the upstream installer will only succeed if your tablet already trusts the certificate chain for $STORAGE_URL"
        REMOTE_CA_CERT=""
    fi
}

# ---- 7. run installer -------------------------------------------------------
run_installer() {
    info "running $INSTALLER on tablet (this stops xochitl while it works)"
    local cmd="cd /home/root && chmod +x ./$INSTALLER && STORAGE_URL='$STORAGE_URL'"
    [ -n "$REMOTE_CA_CERT" ] && cmd="$cmd CA_CERT='$REMOTE_CA_CERT'"
    cmd="$cmd sh ./$INSTALLER install"
    ssh_run "$cmd"
}

# ---- 8. verify device -------------------------------------------------------
verify_device() {
    info "verifying tablet can reach $STORAGE_URL/health"
    if [ "$DRY_RUN" = 1 ]; then
        log "[dry-run] skipping verify"
        return 0
    fi
    if ssh_run "curl -fsS '$STORAGE_URL/health'"; then
        log "tablet reaches /health over the proxy"
    else
        warn "tablet could NOT reach $STORAGE_URL/health"
        warn "check: hosts file edits, CA trust, DNS for $STORAGE_URL"
        die "device verify failed"
    fi
}

# ---- 9. cleanup -------------------------------------------------------------
cleanup_payload() {
    if [ "$KEEP_PAYLOAD" = 1 ]; then
        log "keeping staged installer on tablet (--keep-payload)"
        return 0
    fi
    info "removing staged installer from tablet"
    ssh_run "rm -f /home/root/$INSTALLER" || warn "cleanup failed (ignore)"
}

# ---- main -------------------------------------------------------------------
main() {
    parse_args "$@"
    step "1/9" "load_env";       load_env
    step "2/9" "check_ssh";      check_ssh
    step "3/9" "detect_model";   detect_model
    step "4/9" "pick_installer"; pick_installer
    step "5/9" "fetch_installer"; fetch_installer
    step "6/9" "stage_payload";  stage_payload
    step "7/9" "run_installer";  run_installer
    step "8/9" "verify_device";  verify_device
    step "9/9" "cleanup_payload"; cleanup_payload

    cat >&2 <<EOF

Tablet is configured. Next steps on the tablet itself:

  1. Menu > Settings > Account > Connect.
  2. On the web UI of $STORAGE_URL, click "Connect" to get an 8-char code.
  3. Enter the code on the tablet.

If something is off, on the tablet:
  systemctl status xochitl
  journalctl -u xochitl -f
EOF
}

main "$@"
