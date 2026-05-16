#!/usr/bin/env bash
#
# scripts/install.sh — server-side install for rmfakecloud.
#
# Builds from source, installs the binary + systemd unit + env file,
# creates a service user, starts the service, and emits a device.env
# the operator can use with scripts/device-setup.sh.
#
# Out of scope (operator handles): reverse proxy, TLS termination,
# public DNS, generating a CA cert for the tablet to trust.
#
# Run as root from the repo root. See --help.

set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/common.sh
. "$REPO_ROOT/scripts/lib/common.sh"

# Defaults -------------------------------------------------------------------
STORAGE_URL=""
DATA_DIR="/var/rmfakecloud"
PORT="3000"
PREFIX="/opt/rmfakecloud"
SERVICE_USER="rmfakecloud"
ENV_FILE="/etc/rmfakecloud.env"
UNIT_PATH="/etc/systemd/system/rmfakecloud.service"
UNIT_TEMPLATE="$REPO_ROOT/other/rmfakecloud.service"
ENV_TEMPLATE="$REPO_ROOT/other/rmfakecloud.env"
DEVICE_ENV_OUT=""   # default computed after parsing flags (CWD/device.env)
SKIP_BUILD=0
RECONFIGURE=0
ROTATE_JWT=0
VERIFY_ONLY=0
NON_INTERACTIVE=0
DRY_RUN=0

usage() {
    cat <<'EOF'
scripts/install.sh — build and install rmfakecloud as a systemd service.

Usage: sudo scripts/install.sh [flags]

Flags:
  --storage-url URL       Public URL the tablet will resolve (no default).
  --data-dir PATH         Data directory (default: /var/rmfakecloud).
  --port N                HTTP port (default: 3000).
  --prefix PATH           Install prefix for the binary (default: /opt/rmfakecloud).
  --service-user NAME     Service account name (default: rmfakecloud).
  --env-file PATH         Where to write the env file (default: /etc/rmfakecloud.env).
  --unit-path PATH        Where to write the systemd unit
                          (default: /etc/systemd/system/rmfakecloud.service).
  --device-env-out PATH   Where to emit device.env (default: ./device.env).
  --skip-build            Reuse an existing dist/rmfakecloud-<arch>.
  --reconfigure           Re-render unit/env from flags. Preserves JWT_SECRET_KEY.
  --rotate-jwt            With --reconfigure, also regenerate JWT (logs out all sessions).
  --verify-only           Skip everything except the verify step.
  --non-interactive       Fail instead of prompting.
  --dry-run               Print intended changes, touch nothing.
  -h, --help              This message.

After install, run scripts/device-setup.sh on the workstation cabled to the tablet.
EOF
}

parse_args() {
    while [ $# -gt 0 ]; do
        case $1 in
            --storage-url)     STORAGE_URL=$2; shift 2;;
            --storage-url=*)   STORAGE_URL=${1#*=}; shift;;
            --data-dir)        DATA_DIR=$2; shift 2;;
            --data-dir=*)      DATA_DIR=${1#*=}; shift;;
            --port)            PORT=$2; shift 2;;
            --port=*)          PORT=${1#*=}; shift;;
            --prefix)          PREFIX=$2; shift 2;;
            --prefix=*)        PREFIX=${1#*=}; shift;;
            --service-user)    SERVICE_USER=$2; shift 2;;
            --service-user=*)  SERVICE_USER=${1#*=}; shift;;
            --env-file)        ENV_FILE=$2; shift 2;;
            --env-file=*)      ENV_FILE=${1#*=}; shift;;
            --unit-path)       UNIT_PATH=$2; shift 2;;
            --unit-path=*)     UNIT_PATH=${1#*=}; shift;;
            --device-env-out)  DEVICE_ENV_OUT=$2; shift 2;;
            --device-env-out=*) DEVICE_ENV_OUT=${1#*=}; shift;;
            --skip-build)      SKIP_BUILD=1; shift;;
            --reconfigure)     RECONFIGURE=1; shift;;
            --rotate-jwt)      ROTATE_JWT=1; shift;;
            --verify-only)     VERIFY_ONLY=1; shift;;
            --non-interactive) NON_INTERACTIVE=1; shift;;
            --dry-run)         DRY_RUN=1; shift;;
            -h|--help)         usage; exit 0;;
            *) die "unknown flag: $1 (try --help)";;
        esac
    done
    [ "$ROTATE_JWT" = 1 ] && [ "$RECONFIGURE" = 0 ] && die "--rotate-jwt requires --reconfigure"
    export NON_INTERACTIVE DRY_RUN
    # Compute device.env default after flags so --device-env-out wins.
    if [ -z "$DEVICE_ENV_OUT" ]; then
        if [ -n "${SUDO_USER:-}" ]; then
            local home; home=$(getent passwd "$SUDO_USER" | cut -d: -f6)
            DEVICE_ENV_OUT="${home:-$PWD}/device.env"
        else
            DEVICE_ENV_OUT="$PWD/device.env"
        fi
    fi
}

# ---- 1. preflight: systemd --------------------------------------------------
preflight_systemd() {
    [ -d /run/systemd/system ] || die "this system is not running systemd (no /run/systemd/system). The script only supports systemd hosts."
}

# ---- 2. preflight: root -----------------------------------------------------
preflight_root() {
    require_root "$@"
}

# ---- 3. preflight: build deps ----------------------------------------------
preflight_build_deps() {
    [ "$SKIP_BUILD" = 1 ] && { log "skipping build-dep check (--skip-build)"; return 0; }
    local missing=()
    if has_cmd go; then
        local gov; gov=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
        ver_ge "$gov" "1.23" || missing+=("go>=1.23 (have ${gov:-none})")
    else
        missing+=("go>=1.23")
    fi
    if has_cmd node; then
        local nv; nv=$(node --version | sed 's/^v//')
        ver_ge "$nv" "21" || missing+=("node>=21 (have $nv)")
    else
        missing+=("node>=21")
    fi
    if has_cmd pnpm; then
        local pv; pv=$(pnpm --version)
        ver_ge "$pv" "9" || missing+=("pnpm>=9 (have $pv)")
    else
        missing+=("pnpm>=9")
    fi
    if [ ${#missing[@]} -gt 0 ]; then
        warn "missing or outdated build dependencies: ${missing[*]}"
        cat >&2 <<'EOF'

Install hints (pick the one matching your distro):

  Debian/Ubuntu:
    # Go: see https://go.dev/dl/ (apt's Go is often too old)
    curl -fsSL https://deb.nodesource.com/setup_21.x | sudo -E bash -
    sudo apt-get install -y nodejs
    sudo npm install -g pnpm@9

  Fedora/RHEL:
    sudo dnf install -y golang nodejs
    sudo npm install -g pnpm@9

  Arch:
    sudo pacman -S --needed go nodejs pnpm

  macOS (Homebrew):
    brew install go node pnpm

EOF
        die "install the missing tools and re-run, or pass --skip-build with a prebuilt binary in dist/"
    fi
}

# ---- 4. detect arch ---------------------------------------------------------
detect_arch() {
    local m; m=$(uname -m)
    case $m in
        x86_64|amd64)  ARCH_TARGET="x64";;
        aarch64|arm64) ARCH_TARGET="arm64";;
        armv7l)        ARCH_TARGET="armv7";;
        armv6l)        ARCH_TARGET="armv6";;
        *) die "unsupported architecture: $m";;
    esac
    BINARY_SRC="$REPO_ROOT/dist/rmfakecloud-$ARCH_TARGET"
    log "architecture: $m -> Makefile target $ARCH_TARGET"
}

# ---- 5. ensure service user -------------------------------------------------
ensure_service_user() {
    if getent passwd "$SERVICE_USER" >/dev/null; then
        log "service user $SERVICE_USER exists"
        return 0
    fi
    info "creating service user $SERVICE_USER"
    run useradd --system --home-dir "$DATA_DIR" --no-create-home \
        --shell /usr/sbin/nologin "$SERVICE_USER"
}

# ---- 6. build from source ---------------------------------------------------
build_from_source() {
    if [ "$SKIP_BUILD" = 1 ]; then
        [ -x "$BINARY_SRC" ] || die "--skip-build but $BINARY_SRC is missing"
        log "skipping build, using existing $BINARY_SRC"
        return 0
    fi
    info "building rmfakecloud-$ARCH_TARGET (this builds the React UI then the Go binary)"
    # Drop privs if invoked via sudo so the build artifacts are owned by the
    # original user; the Makefile expects to run as a regular user.
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
        run sudo -u "$SUDO_USER" --preserve-env=PATH make -C "$REPO_ROOT" "dist/rmfakecloud-$ARCH_TARGET"
    else
        warn "running build as root (no SUDO_USER detected); artifacts will be root-owned"
        run make -C "$REPO_ROOT" "dist/rmfakecloud-$ARCH_TARGET"
    fi
    [ "$DRY_RUN" = 1 ] || [ -x "$BINARY_SRC" ] || die "build finished but $BINARY_SRC missing"
}

# ---- 7. install artifacts ---------------------------------------------------
install_artifacts() {
    info "installing binary to $PREFIX/bin/rmfakecloud"
    run install -d -m 0755 "$PREFIX/bin"
    run install -m 0755 "$BINARY_SRC" "$PREFIX/bin/rmfakecloud"
    info "ensuring data dir $DATA_DIR (owner $SERVICE_USER:$SERVICE_USER, mode 0750)"
    run install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DATA_DIR"
}

# ---- 8. render env file -----------------------------------------------------
render_env_file() {
    info "rendering env file $ENV_FILE"
    local jwt existing_jwt
    existing_jwt=""
    if [ -e "$ENV_FILE" ]; then
        existing_jwt=$(sed -n 's/^JWT_SECRET_KEY=\(.*\)$/\1/p' "$ENV_FILE" | head -n1 || true)
    fi
    if [ "$ROTATE_JWT" = 1 ]; then
        warn "rotating JWT_SECRET_KEY — every existing session will be logged out"
        jwt=$(generate_jwt_secret)
    elif [ -n "$existing_jwt" ] && [ "$existing_jwt" != "tbd" ]; then
        log "preserving existing JWT_SECRET_KEY"
        jwt=$existing_jwt
    else
        jwt=$(generate_jwt_secret)
    fi

    # Build env file by stripping the source template's JWT/DATADIR/PORT/STORAGE_URL
    # placeholders and injecting our values at the top.
    local rendered
    rendered=$(
        printf '# Rendered by scripts/install.sh on %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf 'JWT_SECRET_KEY=%s\n' "$jwt"
        printf 'DATADIR=%s\n' "$DATA_DIR"
        printf 'PORT=%s\n' "$PORT"
        [ -n "$STORAGE_URL" ] && printf 'STORAGE_URL=%s\n' "$STORAGE_URL"
        printf '\n# ---- below this line: commented optional config from template ----\n\n'
        # Strip the required-block lines we just emitted to avoid duplicates.
        sed -e '/^JWT_SECRET_KEY=/d' \
            -e '/^DATADIR=/d' \
            -e '/^PORT=/d' \
            -e '/^STORAGE_URL=/d' \
            "$ENV_TEMPLATE"
    )
    printf '%s\n' "$rendered" | write_if_changed "$ENV_FILE" 0640 "root:$SERVICE_USER"
}

generate_jwt_secret() {
    if has_cmd openssl; then
        openssl rand -hex 48
    elif [ -r /dev/urandom ]; then
        head -c 48 /dev/urandom | xxd -p -c 96 2>/dev/null \
            || head -c 48 /dev/urandom | od -An -tx1 | tr -d ' \n'
    else
        die "cannot generate JWT secret — neither openssl nor /dev/urandom available"
    fi
}

# ---- 9. render systemd unit -------------------------------------------------
render_unit() {
    info "rendering systemd unit $UNIT_PATH"
    local exec_path="$PREFIX/bin/rmfakecloud"
    local rendered
    # Customize template: ExecStart path, ReadWritePaths, capabilities for low ports.
    rendered=$(
        sed \
            -e "s|^ExecStart=.*|ExecStart=$exec_path|" \
            -e "s|^ReadWritePaths=.*|ReadWritePaths=$DATA_DIR|" \
            -e "s|^User=.*|User=$SERVICE_USER|" \
            -e "s|^Group=.*|Group=$SERVICE_USER|" \
            -e "s|^EnvironmentFile=.*|EnvironmentFile=$ENV_FILE|" \
            "$UNIT_TEMPLATE"
        if [ "$PORT" -lt 1024 ]; then
            printf '\n# Injected because PORT=%s is < 1024\n' "$PORT"
            printf 'AmbientCapabilities=CAP_NET_BIND_SERVICE\n'
            printf 'CapabilityBoundingSet=CAP_NET_BIND_SERVICE\n'
        fi
    )
    local before_hash="" after_hash=""
    [ -e "$UNIT_PATH" ] && before_hash=$(sha256sum "$UNIT_PATH" | cut -d' ' -f1)
    printf '%s\n' "$rendered" | write_if_changed "$UNIT_PATH" 0644 "root:root"
    [ -e "$UNIT_PATH" ] && after_hash=$(sha256sum "$UNIT_PATH" | cut -d' ' -f1)
    if [ "$before_hash" != "$after_hash" ]; then
        run systemctl daemon-reload
    fi
}

# ---- 10. enable and start ---------------------------------------------------
enable_and_start() {
    if [ "$RECONFIGURE" = 1 ]; then
        info "restarting rmfakecloud (reconfigure mode)"
        run systemctl try-restart rmfakecloud || run systemctl start rmfakecloud
    else
        info "enabling and starting rmfakecloud"
        run systemctl enable --now rmfakecloud
    fi
}

# ---- 11. emit device env ----------------------------------------------------
emit_device_env() {
    info "writing device env to $DEVICE_ENV_OUT"
    local owner=""
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
        owner="$SUDO_USER:$(id -gn "$SUDO_USER" 2>/dev/null || echo "$SUDO_USER")"
    fi
    {
        printf '# Auto-emitted by scripts/install.sh on %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf '# Used by scripts/device-setup.sh to configure a reMarkable tablet.\n\n'
        printf '# REQUIRED — must match the STORAGE_URL passed to install.sh\n'
        printf 'STORAGE_URL=%s\n\n' "${STORAGE_URL:-https://CHANGE.ME}"
        cat <<'EOF'
# Tablet connection (USB-ethernet default)
DEVICE_IP=10.11.99.1
DEVICE_USER=root

# Upstream installer pin (from ddvk/rmfakecloud-proxy)
INSTALLER_VERSION=v0.0.6
INSTALLER_SHA256=

# Operator-provided CA the tablet must trust (absolute path on workstation)
CA_CERT=

# Workstation
SSH_KEY=~/.ssh/id_rsa
MODEL=auto                       # auto|rm1|rm2|rmpro
KEEP_PAYLOAD=0
EOF
    } | write_if_changed "$DEVICE_ENV_OUT" 0644 "$owner"
}

# ---- 12. verify -------------------------------------------------------------
verify() {
    info "verifying"
    [ "$DRY_RUN" = 1 ] && { log "[dry-run] skipping verify"; return 0; }

    local state
    state=$(systemctl is-active rmfakecloud 2>&1 || true)
    if [ "$state" != "active" ]; then
        warn "systemctl is-active rmfakecloud → $state"
        warn "recent log:"
        journalctl -u rmfakecloud -n 30 --no-pager >&2 || true
        die "service is not active"
    fi
    log "service is active"

    # Give the server a beat to bind.
    local tries=0
    while [ $tries -lt 20 ]; do
        if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
            break
        fi
        tries=$((tries + 1))
        sleep 0.5
    done
    local health
    health=$(curl -fsS "http://127.0.0.1:$PORT/health" 2>&1 || true)
    if [ -z "$health" ]; then
        die "GET /health failed on port $PORT"
    fi
    log "/health: $health"

    if ! curl -fsI "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
        warn "HEAD / did not return 2xx — UI assets may not be embedded"
    else
        log "UI root responds"
    fi

    local err_lines
    err_lines=$(journalctl -u rmfakecloud --since "5 min ago" -p err --no-pager -q | wc -l)
    if [ "$err_lines" -gt 0 ]; then
        warn "$err_lines error lines in journal in the last 5 min — review with:"
        warn "  journalctl -u rmfakecloud --since '5 min ago' -p err"
    fi
}

# ---- 13. print next steps ---------------------------------------------------
print_next_steps() {
    cat >&2 <<EOF

rmfakecloud is running on 127.0.0.1:$PORT.

Outside of this script, you still need to:

  1. Reverse proxy. Terminate TLS for ${STORAGE_URL:-<STORAGE_URL>} and proxy to
     http://127.0.0.1:$PORT. Pass through Host and X-Forwarded-*.

  2. Public DNS / firewall. ${STORAGE_URL:-<STORAGE_URL>} must resolve from the
     tablet's network, and 443 must be reachable.

  3. First-user bootstrap. Open ${STORAGE_URL:-<STORAGE_URL>} in a browser and
     register. The very first POST to /login creates the admin account.

  4. Device pairing. After (1)-(3), from a workstation cabled to the tablet:
        scripts/device-setup.sh --env-file $DEVICE_ENV_OUT

Logs:        journalctl -u rmfakecloud -f
Config:      $ENV_FILE
Data:        $DATA_DIR
Reconfigure: sudo $0 --reconfigure --storage-url ...
EOF
}

# ---- main -------------------------------------------------------------------
main() {
    parse_args "$@"

    if [ "$VERIFY_ONLY" = 1 ]; then
        step "verify" "verify"
        verify
        exit 0
    fi

    step "1/13" "preflight_systemd"
    preflight_systemd
    step "2/13" "preflight_root"
    preflight_root "$@"
    step "3/13" "preflight_build_deps"
    preflight_build_deps

    # Validate STORAGE_URL only when actually installing.
    if [ -z "$STORAGE_URL" ]; then
        STORAGE_URL=$(prompt "Public URL the tablet will resolve (e.g. https://rm.example.com)") \
            || die "STORAGE_URL is required"
    fi

    step "4/13" "detect_arch"
    detect_arch
    step "5/13" "ensure_service_user"
    ensure_service_user
    step "6/13" "build_from_source"
    build_from_source
    step "7/13" "install_artifacts"
    install_artifacts
    step "8/13" "render_env_file"
    render_env_file
    step "9/13" "render_unit"
    render_unit
    step "10/13" "enable_and_start"
    enable_and_start
    step "11/13" "emit_device_env"
    emit_device_env
    step "12/13" "verify"
    verify
    step "13/13" "print_next_steps"
    print_next_steps
}

main "$@"
