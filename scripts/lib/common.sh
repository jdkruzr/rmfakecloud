# Shared helpers for scripts/install.sh and scripts/device-setup.sh.
# Source me, don't exec me.

# Colors only when stdout is a tty.
if [ -t 1 ]; then
    _C_RESET='\033[0m'; _C_RED='\033[31m'; _C_YEL='\033[33m'
    _C_GRN='\033[32m'; _C_DIM='\033[2m'
else
    _C_RESET=''; _C_RED=''; _C_YEL=''; _C_GRN=''; _C_DIM=''
fi

log()  { printf '%b[%s]%b %s\n' "$_C_DIM" "$(date +%H:%M:%S)" "$_C_RESET" "$*" >&2; }
info() { printf '%b==>%b %s\n' "$_C_GRN" "$_C_RESET" "$*" >&2; }
warn() { printf '%bwarn:%b %s\n' "$_C_YEL" "$_C_RESET" "$*" >&2; }
die()  { printf '%berror:%b %s\n' "$_C_RED" "$_C_RESET" "$*" >&2; exit 1; }

step() {
    # Visual section banner: step "3/13" "preflight_build_deps"
    printf '\n%b[%s] %s%b\n' "$_C_GRN" "$1" "$2" "$_C_RESET" >&2
}

has_cmd() { command -v "$1" >/dev/null 2>&1; }

require_root() {
    [ "$(id -u)" = 0 ] || die "must run as root (try: sudo $0 $*)"
}

# Compare two dotted versions: ver_ge "1.23.3" "1.23" -> 0 if first >= second.
ver_ge() {
    # shellcheck disable=SC2046
    set -- $(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1) "$1"
    [ "$1" = "$2" ]
}

# Atomic write_if_changed PATH MODE OWNER < CONTENT
# - PATH: destination
# - MODE: octal mode (e.g. 0640)
# - OWNER: owner:group (or empty to leave alone)
# - Reads new content from stdin.
# - Prints "created PATH", "updated PATH", or "unchanged PATH".
# - Returns 0 on success; 2 if --dry-run would have changed something (set DRY_RUN=1).
write_if_changed() {
    local target=$1 mode=$2 owner=$3 tmp action
    tmp=$(mktemp "${target}.XXXXXX") || die "mktemp failed for $target"
    cat >"$tmp"
    if [ -e "$target" ] && cmp -s "$tmp" "$target"; then
        action="unchanged"
        rm -f "$tmp"
    elif [ -e "$target" ]; then
        action="updated"
    else
        action="created"
    fi
    if [ "$action" = "unchanged" ]; then
        log "unchanged $target"
        return 0
    fi
    if [ "${DRY_RUN:-0}" = 1 ]; then
        log "[dry-run] would $action $target"
        rm -f "$tmp"
        return 0
    fi
    chmod "$mode" "$tmp" || die "chmod $mode $tmp failed"
    [ -n "$owner" ] && { chown "$owner" "$tmp" || die "chown $owner $tmp failed"; }
    mv "$tmp" "$target" || die "mv $tmp $target failed"
    info "$action $target"
}

# Run a command unless DRY_RUN=1.
run() {
    if [ "${DRY_RUN:-0}" = 1 ]; then
        log "[dry-run] $*"
        return 0
    fi
    "$@"
}

# Prompt with default. Honors NON_INTERACTIVE=1 (returns default or dies if no default).
prompt() {
    local question=$1 default=${2:-} reply
    if [ "${NON_INTERACTIVE:-0}" = 1 ]; then
        [ -n "$default" ] || die "non-interactive: $question has no default"
        printf '%s\n' "$default"
        return 0
    fi
    if [ -n "$default" ]; then
        printf '%s [%s]: ' "$question" "$default" >&2
    else
        printf '%s: ' "$question" >&2
    fi
    IFS= read -r reply
    [ -n "$reply" ] || reply=$default
    [ -n "$reply" ] || die "$question is required"
    printf '%s\n' "$reply"
}
