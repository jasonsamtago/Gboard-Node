#!/usr/bin/env bash
set -Eeuo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

APP_NAME="gboard-node"
INSTALL_ROOT="/etc/gboard-node"
BACKUP_DIR="${INSTALL_ROOT}/backups"
INSTALL_META="${INSTALL_ROOT}/install-meta.json"
CONFIG_FILE="${INSTALL_ROOT}/config.yml"
CREDENTIALS_FILE="${INSTALL_ROOT}/credentials.env"
BINARY_PATH="/usr/local/bin/gboard-node"
SERVICE_NAME="gboard-node.service"
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}"
CLI_PATH="/usr/local/bin/gbctl"
CLI_PATH_USR_BIN="/usr/bin/gbctl"
INSTALLER_COPY_PATH="${INSTALL_ROOT}/install.sh"

# One-release compat: existing xboard-node hosts are migrated in place.
# Do not treat a generic "xboard" string as a brand path.
LEGACY_INSTALL_ROOT="/etc/xboard-node"
LEGACY_CONFIG_FILE="${LEGACY_INSTALL_ROOT}/config.yml"
LEGACY_INSTALL_META="${LEGACY_INSTALL_ROOT}/install-meta.json"
LEGACY_BINARY_PATH="/usr/local/bin/xboard-node"
LEGACY_SERVICE_NAME="xboard-node.service"
LEGACY_SERVICE_PATH="/etc/systemd/system/${LEGACY_SERVICE_NAME}"
LEGACY_CLI_PATH="/usr/local/bin/xbctl"
LEGACY_CLI_PATH_USR_BIN="/usr/bin/xbctl"
CLI_BINARY_SOURCE=""
DEFAULT_HEALTH_PORT=65530
DEFAULT_KERNEL="singbox"
DEFAULT_MODE="node"
DEFAULT_ACTION="install"
DEFAULT_RELEASE_VERSION="dev"
DEFAULT_LOG_LEVEL="info"
DEFAULT_KERNEL_LOG_LEVEL="warn"
DEFAULT_DOWNLOAD_BASE="https://github.com/jasonsamtago/Gboard-Node/releases"

ACTION="${DEFAULT_ACTION}"
MODE=""
PANEL_URL=""
TOKEN=""
NODE_ID=""
NODE_TYPE=""
MACHINE_ID=""
KERNEL_TYPE="${DEFAULT_KERNEL}"
RELEASE_VERSION="${DEFAULT_RELEASE_VERSION}"
HEALTH_PORT="${DEFAULT_HEALTH_PORT}"
HEALTH_ENABLED=1
RUNTIME_GOMEMLIMIT=""
RUNTIME_GOGC=""
BINARY_SOURCE=""
CLI_BINARY_SOURCE=""
FORCE_RECONFIGURE=0
PURGE=0
YES=0
ARCH=""
OS=""
DOWNLOAD_URL=""
CURRENT_STATE="fresh"
TMP_DIR=""
BACKUP_PATH=""
SERVICE_EXISTED=0
CLEANUP_DONE=0

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step()  { echo -e "${CYAN}[STEP]${NC} ${BOLD}$1${NC}"; }

cleanup_tmp() {
    if [ "$CLEANUP_DONE" -eq 1 ]; then
        return
    fi
    CLEANUP_DONE=1
    if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
}

load_health_port_from_config() {
    local cfg_path="$1"
    if [ ! -f "$cfg_path" ]; then
        return
    fi
    local parsed
    if [ -x "$CLI_PATH" ]; then
        parsed=$("$CLI_PATH" config health-port --config "$cfg_path" 2>/dev/null)
    elif [ -x "$LEGACY_CLI_PATH" ]; then
        parsed=$("$LEGACY_CLI_PATH" config health-port --config "$cfg_path" 2>/dev/null)
    else
        parsed=$(grep -m1 'health_port:' "$cfg_path" 2>/dev/null | sed 's/.*health_port:[[:space:]]*//' | tr -cd '0-9')
    fi
    if [ -n "$parsed" ] && [ "$parsed" -ge 0 ] 2>/dev/null; then
        HEALTH_PORT="$parsed"
        if [ "$HEALTH_PORT" -eq 0 ]; then
            HEALTH_ENABLED=0
        else
            HEALTH_ENABLED=1
        fi
    fi
}

rollback_install() {
    log_warn "Rolling back installation"
    if [ -n "$BACKUP_PATH" ] && [ -d "$BACKUP_PATH" ]; then
        if [ -f "$BACKUP_PATH/gboard-node" ]; then
            install -m 755 "$BACKUP_PATH/gboard-node" "$BINARY_PATH"
        else
            rm -f "$BINARY_PATH"
        fi
        if [ -f "$BACKUP_PATH/config.yml" ]; then
            install -m 600 "$BACKUP_PATH/config.yml" "$CONFIG_FILE"
        else
            rm -f "$CONFIG_FILE"
        fi
        if [ -f "$BACKUP_PATH/credentials.env" ]; then
            install -m 600 "$BACKUP_PATH/credentials.env" "$CREDENTIALS_FILE"
        else
            rm -f "$CREDENTIALS_FILE"
        fi
        if [ -f "$BACKUP_PATH/install-meta.json" ]; then
            install -m 644 "$BACKUP_PATH/install-meta.json" "$INSTALL_META"
        else
            rm -f "$INSTALL_META"
        fi
        if [ -f "$BACKUP_PATH/gbctl" ]; then
            install -m 755 "$BACKUP_PATH/gbctl" "$CLI_PATH"
        elif [ -f "$BACKUP_PATH/xbctl" ]; then
            install -m 755 "$BACKUP_PATH/xbctl" "$CLI_PATH"
        else
            rm -f "$CLI_PATH"
        fi
        install_cli_compat_symlinks
        if [ -f "$BACKUP_PATH/${SERVICE_NAME}" ]; then
            install -m 644 "$BACKUP_PATH/${SERVICE_NAME}" "$SERVICE_PATH"
        else
            rm -f "$SERVICE_PATH"
        fi
    fi
    load_health_port_from_config "$CONFIG_FILE"
    systemctl daemon-reload || true
    if [ "$SERVICE_EXISTED" -eq 1 ] || [ -f "$SERVICE_PATH" ]; then
        systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 || true
        if ! wait_for_health; then
            log_error "Rollback completed but restored service did not become healthy"
            show_recent_logs
            return 1
        fi
    else
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
    log_warn "Rollback complete"
}

on_error() {
    local exit_code=$?
    local line_no=${1:-unknown}
    if [ "$exit_code" -ne 0 ]; then
        log_error "Install failed at line ${line_no} (exit=${exit_code})"
        if [ -n "$BACKUP_PATH" ]; then
            rollback_install || true
        fi
    fi
    cleanup_tmp
    exit "$exit_code"
}
trap 'on_error $LINENO' ERR
trap cleanup_tmp EXIT

usage() {
    cat <<'HELP'

  Gboard-Node Installer

  ACTIONS:
    install      Install or reconcile the configured deployment (default)
    upgrade      Upgrade binary and restart service
    uninstall    Remove installed service and binary (config kept unless --purge)
    status       Show current installation status
    help         Show this help

  MODES (auto-detected from --node-id or --machine-id if omitted):
    --mode node      Panel single-node mode (default)
    --mode machine   Panel machine mode

  REQUIRED FOR NODE MODE:
    --panel, -a      Panel URL
    --token, -t      Panel server token
    --node-id, -n    Node ID

  REQUIRED FOR MACHINE MODE:
    --panel, -a       Panel URL
    --token, -t       Machine token
    --machine-id      Machine ID

  OPTIONAL:
    --node-type, -T     Explicit node type for node mode
    --kernel, -k        singbox or xray (default: singbox)
    --version           Release version or latest (default: latest)
    --binary            Use a local gboard-node binary path instead of downloading
    --gbctl-binary      Use a local gbctl binary path instead of downloading
    --health-port       Local health port (default: 65530, use 0 to disable)
    --gomemlimit        Runtime GOMEMLIMIT value, e.g. 256MiB
    --gogc              Runtime GOGC value, e.g. 50
    --force-reconfigure Overwrite an existing install even if mode/target changed
    --purge             With uninstall, delete /etc/gboard-node too
    --yes, -y           Non-interactive confirmation for destructive operations

  COMPAT:
    Existing /etc/xboard-node installs are migrated to /etc/gboard-node
    automatically on install, upgrade, status, and service commands.
    xbctl remains a symlink to gbctl for one release.

  EXAMPLES:
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --node-id 1
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --machine-id 1
    sudo bash install.sh upgrade
    sudo bash install.sh uninstall --purge --yes

HELP
}

parse_args() {
    local positional=()
    while [ $# -gt 0 ]; do
        case "$1" in
            install|upgrade|uninstall|status|help)
                ACTION="$1"
                shift
                ;;
            --mode)
                MODE="$2"
                shift 2
                ;;
            --panel|-a|--api)
                PANEL_URL="$2"
                shift 2
                ;;
            --token|-t)
                TOKEN="$2"
                shift 2
                ;;
            --node-id|-n)
                NODE_ID="$2"
                shift 2
                ;;
            --node-type|-T)
                NODE_TYPE="$2"
                shift 2
                ;;
            --machine-id)
                MACHINE_ID="$2"
                shift 2
                ;;
            --kernel|-k)
                KERNEL_TYPE="$2"
                shift 2
                ;;
            --version)
                RELEASE_VERSION="$2"
                shift 2
                ;;
            --binary)
                BINARY_SOURCE="$2"
                shift 2
                ;;
            --gbctl-binary)
                CLI_BINARY_SOURCE="$2"
                shift 2
                ;;
            --health-port)
                HEALTH_PORT="$2"
                shift 2
                ;;
            --gomemlimit)
                RUNTIME_GOMEMLIMIT="$2"
                shift 2
                ;;
            --gogc)
                RUNTIME_GOGC="$2"
                shift 2
                ;;
            --force-reconfigure)
                FORCE_RECONFIGURE=1
                shift
                ;;
            --purge)
                PURGE=1
                shift
                ;;
            --yes|-y)
                YES=1
                shift
                ;;
            --help|-h)
                ACTION="help"
                shift
                ;;
            *)
                positional+=("$1")
                shift
                ;;
        esac
    done

    if [ ${#positional[@]} -gt 0 ] && [ "$ACTION" = "install" ]; then
        ACTION="${positional[0]}"
    fi

    case "$KERNEL_TYPE" in
        singbox|SingBox|SINGBOX) KERNEL_TYPE="singbox" ;;
        xray|Xray|XRAY) KERNEL_TYPE="xray" ;;
        *) ;;
    esac

    # Auto-detect mode from arguments when --mode is not specified.
    if [ -z "$MODE" ]; then
        if [ -n "$MACHINE_ID" ]; then
            MODE="machine"
        else
            MODE="node"
        fi
    fi

    case "$MODE" in
        node|machine) ;;
        *)
            log_error "Unsupported mode: $MODE"
            usage
            exit 1
            ;;
    esac
}

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "Please run as root or with sudo"
        exit 1
    fi
}

detect_arch() {
    local raw
    raw=$(uname -m)
    case "$raw" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            log_error "Unsupported architecture: $raw"
            exit 1
            ;;
    esac
}

detect_os() {
    local kernel
    kernel="$(uname -s)"
    case "$kernel" in
        FreeBSD|freebsd)
            OS="freebsd"
            ;;
        Linux|linux)
            if [ -f /etc/os-release ]; then
                . /etc/os-release
                OS="$ID"
            else
                OS="linux"
            fi
            ;;
        *)
            log_error "Unsupported OS: ${kernel} (will not treat as linux)"
            exit 1
            ;;
    esac
}

ensure_systemd() {
    if [ "${OS:-}" = "freebsd" ]; then
        return 0
    fi
    if ! command -v systemctl >/dev/null 2>&1; then
        log_error "systemd is required for this installer"
        exit 1
    fi
    if [ ! -d /run/systemd/system ]; then
        log_error "This host does not appear to be running systemd"
        exit 1
    fi
}

run_with_retry() {
    local attempts="$1"
    local delay="$2"
    shift 2
    local i=1
    while [ "$i" -le "$attempts" ]; do
        if "$@"; then
            return 0
        fi
        if [ "$i" -lt "$attempts" ]; then
            log_warn "Command failed, retrying in ${delay}s: $*"
            sleep "$delay"
        fi
        i=$((i + 1))
    done
    return 1
}

install_dependencies() {
    case "$OS" in
        ubuntu|debian)
            DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get update -qq
            DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get install -y -qq curl wget ca-certificates >/dev/null 2>&1
            ;;
        centos|rhel|rocky|almalinux|fedora)
            if command -v dnf >/dev/null 2>&1; then
                run_with_retry 5 3 dnf install -y -q curl wget ca-certificates >/dev/null 2>&1
            else
                run_with_retry 5 3 yum install -y -q curl wget ca-certificates >/dev/null 2>&1
            fi
            ;;
        *)
            log_warn "OS ${OS} is not in the official support set; continuing best-effort"
            ;;
    esac
}

ensure_dirs() {
    mkdir -p "$INSTALL_ROOT" "$BACKUP_DIR"
    chmod 700 "$INSTALL_ROOT"
}

is_root() {
    [ "$(id -u)" -eq 0 ]
}

rewrite_legacy_paths_in_file() {
    local f="$1"
    [ -f "$f" ] || return 0
    # Case-sensitive install-root rewrite only. Do not touch generic "xboard".
    if grep -q '/etc/xboard-node' "$f" 2>/dev/null; then
        sed -i 's|/etc/xboard-node|/etc/gboard-node|g' "$f"
    fi
}

rewrite_legacy_paths_in_tree() {
    local root="$1"
    [ -d "$root" ] || return 0
    rewrite_legacy_paths_in_file "$root/config.yml"
    rewrite_legacy_paths_in_file "$root/install-meta.json"
    rewrite_legacy_paths_in_file "$root/credentials.env"
    if [ -d "$root/instances" ]; then
        local f
        while IFS= read -r -d '' f; do
            rewrite_legacy_paths_in_file "$f"
        done < <(find "$root/instances" -type f \( -name '*.yml' -o -name '*.yaml' -o -name '*.json' \) -print0 2>/dev/null)
    fi
}

# Move /etc/xboard-node → /etc/gboard-node when the new tree is absent.
# If both exist, prefer the new tree and leave the old directory alone.
migrate_legacy_layout() {
    if [ -d "$LEGACY_INSTALL_ROOT" ] && [ ! -e "$INSTALL_ROOT" ]; then
        log_step "Migrating ${LEGACY_INSTALL_ROOT} -> ${INSTALL_ROOT}"
        if systemctl is-active "$LEGACY_SERVICE_NAME" >/dev/null 2>&1; then
            systemctl stop "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
        fi
        if mv "$LEGACY_INSTALL_ROOT" "$INSTALL_ROOT"; then
            :
        else
            log_warn "Rename failed, copying ${LEGACY_INSTALL_ROOT} to ${INSTALL_ROOT}"
            cp -a "$LEGACY_INSTALL_ROOT" "$INSTALL_ROOT"
            rm -rf "$LEGACY_INSTALL_ROOT"
        fi
        chmod 700 "$INSTALL_ROOT" 2>/dev/null || true
        rewrite_legacy_paths_in_tree "$INSTALL_ROOT"
        log_info "Moved install data to ${INSTALL_ROOT}"
    elif [ -d "$LEGACY_INSTALL_ROOT" ] && [ -e "$INSTALL_ROOT" ]; then
        log_warn "Both ${INSTALL_ROOT} and ${LEGACY_INSTALL_ROOT} exist; using ${INSTALL_ROOT} and leaving the old tree in place"
        rewrite_legacy_paths_in_tree "$INSTALL_ROOT"
    elif [ -d "$INSTALL_ROOT" ]; then
        rewrite_legacy_paths_in_tree "$INSTALL_ROOT"
    fi
}

write_rcd_script() {
    local dest="$1"
    cat >"$dest" <<EOF_RCD
#!/bin/sh
#
# PROVIDE: gboard_node
# REQUIRE: NETWORKING
# KEYWORD: shutdown

. /etc/rc.subr

name="gboard_node"
rcvar="gboard_node_enable"
command="${BINARY_PATH}"
command_args="-c ${CONFIG_FILE}"

load_rc_config \$name
run_rc_command "\$1"
EOF_RCD
}

write_service_unit() {
    local dest="$1"
    cat >"$dest" <<EOF_UNIT
[Unit]
Description=Gboard Node Backend
Documentation=https://github.com/jasonsamtago/Gboard-Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_ROOT}
EnvironmentFile=-${CREDENTIALS_FILE}
ExecStart=${BINARY_PATH} -c ${CONFIG_FILE}
Restart=always
RestartSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF_UNIT
}

install_cli_compat_symlinks() {
    if [ ! -x "$CLI_PATH" ]; then
        return 0
    fi
    ln -sfn "$CLI_PATH" "$CLI_PATH_USR_BIN" 2>/dev/null || true
    # One-release compatibility so existing xbctl scripts keep working.
    ln -sfn "$CLI_PATH" "$LEGACY_CLI_PATH" 2>/dev/null || true
    ln -sfn "$CLI_PATH" "$LEGACY_CLI_PATH_USR_BIN" 2>/dev/null || true
}

# Promote old binaries/unit to gboard-node names, then drop the old artifacts.
# xbctl is kept as a symlink to gbctl.
retire_legacy_runtime() {
    if [ ! -x "$BINARY_PATH" ] && [ -x "$LEGACY_BINARY_PATH" ]; then
        log_info "Installing ${BINARY_PATH} from ${LEGACY_BINARY_PATH}"
        install -m 755 "$LEGACY_BINARY_PATH" "$BINARY_PATH"
    fi
    if [ ! -x "$CLI_PATH" ]; then
        if [ -x "$LEGACY_CLI_PATH" ] && [ ! -L "$LEGACY_CLI_PATH" ]; then
            install -m 755 "$LEGACY_CLI_PATH" "$CLI_PATH"
        elif [ -x "$LEGACY_CLI_PATH_USR_BIN" ] && [ ! -L "$LEGACY_CLI_PATH_USR_BIN" ]; then
            install -m 755 "$LEGACY_CLI_PATH_USR_BIN" "$CLI_PATH"
        fi
    fi

    local legacy_active=0
    if systemctl is-active "$LEGACY_SERVICE_NAME" >/dev/null 2>&1; then
        legacy_active=1
    fi
    if [ -f "$LEGACY_SERVICE_PATH" ] || [ "$legacy_active" -eq 1 ]; then
        systemctl stop "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl disable "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
    fi

    if [ ! -f "$SERVICE_PATH" ] && [ -x "$BINARY_PATH" ] && [ -f "$CONFIG_FILE" ]; then
        local unit_tmp
        unit_tmp=$(mktemp)
        write_service_unit "$unit_tmp"
        install -m 644 "$unit_tmp" "$SERVICE_PATH"
        rm -f "$unit_tmp"
    fi

    if [ -f "$SERVICE_PATH" ]; then
        systemctl daemon-reload || true
        systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
        if [ "$legacy_active" -eq 1 ]; then
            systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
            systemctl start "$SERVICE_NAME" >/dev/null 2>&1 || true
        fi
    fi

    if [ -f "$LEGACY_SERVICE_PATH" ]; then
        rm -f "$LEGACY_SERVICE_PATH"
        systemctl daemon-reload || true
    fi

    if [ -e "$LEGACY_BINARY_PATH" ] && [ "$LEGACY_BINARY_PATH" != "$BINARY_PATH" ]; then
        rm -f "$LEGACY_BINARY_PATH"
    fi

    install_cli_compat_symlinks
}

# with_runtime=runtime also switches the systemd unit and binaries.
# with_runtime=layout only moves the config tree (used by uninstall).
maybe_migrate_legacy_install() {
    local with_runtime="${1:-runtime}"
    if ! is_root; then
        return 0
    fi
    migrate_legacy_layout
    if [ "$with_runtime" = "runtime" ]; then
        retire_legacy_runtime
    fi
}

validate_positive_int() {
    local label="$1"
    local value="$2"
    if ! [[ "$value" =~ ^[0-9]+$ ]] || [ "$value" -le 0 ]; then
        log_error "${label} must be a positive integer, got: ${value}"
        exit 1
    fi
}

validate_install_request() {
    if [ -z "$PANEL_URL" ]; then
        log_error "Panel URL is required"
        exit 1
    fi
    if [ -z "$TOKEN" ]; then
        log_error "Token is required"
        exit 1
    fi
    if ! [[ "$HEALTH_PORT" =~ ^[0-9]+$ ]]; then
        log_error "health-port must be a non-negative integer"
        exit 1
    fi
    if [ "$HEALTH_PORT" -eq 0 ]; then
        HEALTH_ENABLED=0
    fi
    case "$KERNEL_TYPE" in
        singbox|xray) ;;
        *)
            log_error "Kernel must be singbox or xray"
            exit 1
            ;;
    esac
    case "$MODE" in
        node)
            validate_positive_int "Node ID" "$NODE_ID"
            ;;
        machine)
            validate_positive_int "Machine ID" "$MACHINE_ID"
            ;;
    esac
}

detect_current_state() {
    local has_binary=0 has_config=0 has_service=0
    if [ -x "$BINARY_PATH" ] || [ -x "$LEGACY_BINARY_PATH" ]; then
        has_binary=1
    fi
    if [ -f "$CONFIG_FILE" ] || [ -f "$LEGACY_CONFIG_FILE" ]; then
        has_config=1
    fi
    if [ -f "$SERVICE_PATH" ] || [ -f "$LEGACY_SERVICE_PATH" ]; then
        has_service=1
    fi

    if [ "$has_binary" -eq 0 ] && [ "$has_config" -eq 0 ] && [ "$has_service" -eq 0 ]; then
        CURRENT_STATE="fresh"
    elif [ "$has_binary" -eq 1 ] && [ "$has_config" -eq 1 ] && [ "$has_service" -eq 1 ]; then
        CURRENT_STATE="installed"
    else
        CURRENT_STATE="partial"
    fi
}

require_reconfigure_confirmation() {
    return
}

select_binary_source() {
    if [ -n "$BINARY_SOURCE" ]; then
        if [ ! -f "$BINARY_SOURCE" ]; then
            log_error "Binary source not found: $BINARY_SOURCE"
            exit 1
        fi
        echo "$BINARY_SOURCE"
        return
    fi
    if [ -f "./gboard-node" ]; then
        echo "./gboard-node"
        return
    fi
    if [ "${OS:-}" = "freebsd" ]; then
        if [ -f "./gboard-node-freebsd-${ARCH}" ]; then
            echo "./gboard-node-freebsd-${ARCH}"
            return
        fi
        echo ""
        return
    fi
    if [ -f "./gboard-node-linux-${ARCH}" ]; then
        echo "./gboard-node-linux-${ARCH}"
        return
    fi
    # One-release fallback for leftover local filenames from the xboard rename.
    if [ -f "./xboard-node" ]; then
        echo "./xboard-node"
        return
    fi
    if [ -f "./xboard-node-linux-${ARCH}" ]; then
        echo "./xboard-node-linux-${ARCH}"
        return
    fi
    echo ""
}

resolve_download_url() {
    local artifact="$1"
    if [ "$RELEASE_VERSION" = "latest" ]; then
        DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/latest/download/${artifact}"
    else
        DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/${RELEASE_VERSION}/${artifact}"
    fi
}

stage_binary() {
    local staged="$TMP_DIR/gboard-node"
    local local_src
    local_src=$(select_binary_source)
    if [ -n "$local_src" ]; then
        log_step "Using local binary: ${local_src}"
        cp "$local_src" "$staged"
    else
        if [ "${OS:-}" = "freebsd" ]; then
            resolve_download_url "gboard-node-freebsd-${ARCH}"
        else
            resolve_download_url "gboard-node-linux-${ARCH}"
        fi
        log_step "Downloading binary: ${DOWNLOAD_URL}"
        if ! curl -fsSL "$DOWNLOAD_URL" -o "$staged"; then
            log_error "Failed to download binary from ${DOWNLOAD_URL}"
            exit 1
        fi
    fi
    chmod +x "$staged"
    if ! "$staged" -v >/dev/null 2>&1; then
        log_error "Downloaded binary failed version check"
        exit 1
    fi
}

stage_gbctl() {
    local staged="$TMP_DIR/gbctl"
    local local_src=""
    if [ -n "$CLI_BINARY_SOURCE" ]; then
        if [ ! -f "$CLI_BINARY_SOURCE" ]; then
            log_error "gbctl binary source not found: $CLI_BINARY_SOURCE"
            exit 1
        fi
        local_src="$CLI_BINARY_SOURCE"
    elif [ -f "./gbctl" ]; then
        local_src="./gbctl"
    elif [ "${OS:-}" = "freebsd" ] && [ -f "./gbctl-freebsd-${ARCH}" ]; then
        local_src="./gbctl-freebsd-${ARCH}"
    elif [ "${OS:-}" != "freebsd" ] && [ -f "./gbctl-linux-${ARCH}" ]; then
        local_src="./gbctl-linux-${ARCH}"
    elif [ "${OS:-}" != "freebsd" ] && [ -f "./xbctl" ]; then
        local_src="./xbctl"
    elif [ "${OS:-}" != "freebsd" ] && [ -f "./xbctl-linux-${ARCH}" ]; then
        local_src="./xbctl-linux-${ARCH}"
    fi
    if [ -n "$local_src" ]; then
        log_step "Using local gbctl binary: ${local_src}"
        cp "$local_src" "$staged"
    else
        if [ "${OS:-}" = "freebsd" ]; then
            resolve_download_url "gbctl-freebsd-${ARCH}"
        else
            resolve_download_url "gbctl-linux-${ARCH}"
        fi
        log_step "Downloading gbctl: ${DOWNLOAD_URL}"
        if ! curl -fsSL "$DOWNLOAD_URL" -o "$staged"; then
            log_error "Failed to download gbctl from ${DOWNLOAD_URL}"
            exit 1
        fi
    fi
    chmod +x "$staged"
    if ! "$staged" version > /dev/null 2>&1; then
        log_error "Downloaded gbctl failed version check"
        exit 1
    fi
}

render_config() {
    local init_args=(
        config init
        --mode "$MODE"
        --panel-url "$PANEL_URL"
        --kernel "${KERNEL_TYPE:-singbox}"
        --health-port "${HEALTH_PORT:-0}"
        --token "$TOKEN"
        --version "$RELEASE_VERSION"
        --output "$TMP_DIR/config.yml"
        --credentials-out "$TMP_DIR/credentials.env"
        --meta "$TMP_DIR/install-meta.json"
        --install-root "$INSTALL_ROOT"
    )
    if [ -f "$CONFIG_FILE" ]; then
        init_args+=(--config "$CONFIG_FILE")
    fi
    if [ -f "$CREDENTIALS_FILE" ]; then
        init_args+=(--credentials-in "$CREDENTIALS_FILE")
    fi
    if [ "$MODE" = "machine" ]; then
        init_args+=(--machine-id "$MACHINE_ID")
    else
        init_args+=(--node-id "$NODE_ID")
        if [ -n "$NODE_TYPE" ]; then
            init_args+=(--node-type "$NODE_TYPE")
        fi
    fi
    if [ -n "$RUNTIME_GOMEMLIMIT" ]; then
        init_args+=(--gomemlimit "$RUNTIME_GOMEMLIMIT")
    fi
    if [ -n "$RUNTIME_GOGC" ] && [ "$RUNTIME_GOGC" -gt 0 ] 2>/dev/null; then
        init_args+=(--gogc "$RUNTIME_GOGC")
    fi

    local output
    output=$("$TMP_DIR/gbctl" "${init_args[@]}") || {
        log_error "gbctl config init failed"
        exit 1
    }

    INSTANCE_ID=$(echo "$output" | grep '^INSTANCE_ID=' | cut -d= -f2-)
    chmod 600 "$TMP_DIR/credentials.env"
}

render_service() {
    if [ "${OS:-}" = "freebsd" ]; then
        SERVICE_NAME="gboard-node"
        SERVICE_PATH="/usr/local/etc/rc.d/gboard-node"
        write_rcd_script "${TMP_DIR}/gboard-node.rcd"
        return
    fi
    write_service_unit "$TMP_DIR/${SERVICE_NAME}"
}

backup_existing_state() {
    BACKUP_PATH="${BACKUP_DIR}/$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$BACKUP_PATH"
    if [ -x "$BINARY_PATH" ]; then
        cp "$BINARY_PATH" "$BACKUP_PATH/gboard-node"
    elif [ -x "$LEGACY_BINARY_PATH" ]; then
        cp "$LEGACY_BINARY_PATH" "$BACKUP_PATH/gboard-node"
    fi
    if [ -x "$CLI_PATH" ]; then
        cp "$CLI_PATH" "$BACKUP_PATH/gbctl"
    elif [ -x "$LEGACY_CLI_PATH" ] && [ ! -L "$LEGACY_CLI_PATH" ]; then
        cp "$LEGACY_CLI_PATH" "$BACKUP_PATH/gbctl"
    fi
    if [ -f "$CONFIG_FILE" ]; then
        cp "$CONFIG_FILE" "$BACKUP_PATH/config.yml"
    elif [ -f "$LEGACY_CONFIG_FILE" ]; then
        cp "$LEGACY_CONFIG_FILE" "$BACKUP_PATH/config.yml"
    fi
    if [ -f "$CREDENTIALS_FILE" ]; then
        cp "$CREDENTIALS_FILE" "$BACKUP_PATH/credentials.env"
    elif [ -f "${LEGACY_INSTALL_ROOT}/credentials.env" ]; then
        cp "${LEGACY_INSTALL_ROOT}/credentials.env" "$BACKUP_PATH/credentials.env"
    fi
    if [ -f "$INSTALL_META" ]; then
        cp "$INSTALL_META" "$BACKUP_PATH/install-meta.json"
    elif [ -f "$LEGACY_INSTALL_META" ]; then
        cp "$LEGACY_INSTALL_META" "$BACKUP_PATH/install-meta.json"
    fi
    if [ -f "$SERVICE_PATH" ]; then
        cp "$SERVICE_PATH" "$BACKUP_PATH/${SERVICE_NAME}"
        SERVICE_EXISTED=1
    elif [ -f "$LEGACY_SERVICE_PATH" ]; then
        SERVICE_EXISTED=1
        if [ -n "${TMP_DIR:-}" ] && [ -f "$TMP_DIR/${SERVICE_NAME}" ]; then
            cp "$TMP_DIR/${SERVICE_NAME}" "$BACKUP_PATH/${SERVICE_NAME}"
        else
            sed -e 's|/usr/local/bin/xboard-node|/usr/local/bin/gboard-node|g' \
                -e 's|/etc/xboard-node|/etc/gboard-node|g' \
                -e 's|Xboard Node Backend|Gboard Node Backend|g' \
                "$LEGACY_SERVICE_PATH" > "$BACKUP_PATH/${SERVICE_NAME}"
        fi
    else
        SERVICE_EXISTED=0
    fi
}

stop_existing_service() {
    if [ -f "$SERVICE_PATH" ] || systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
    if [ -f "$LEGACY_SERVICE_PATH" ] || systemctl is-active "$LEGACY_SERVICE_NAME" >/dev/null 2>&1; then
        systemctl stop "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
    fi
}

install_staged_files() {
    stop_existing_service
    install -m 755 "$TMP_DIR/gboard-node" "$BINARY_PATH"
    install -m 600 "$TMP_DIR/config.yml" "$CONFIG_FILE"
    install -m 600 "$TMP_DIR/credentials.env" "$CREDENTIALS_FILE"
    install -m 644 "$TMP_DIR/install-meta.json" "$INSTALL_META"
    if [ -f "$0" ] && [ "$(realpath "$0")" != "$(realpath "$INSTALLER_COPY_PATH" 2>/dev/null || echo "$INSTALLER_COPY_PATH")" ]; then
        install -m 755 "$0" "$INSTALLER_COPY_PATH"
    fi
    install -m 755 "$TMP_DIR/gbctl" "$CLI_PATH"
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    retire_legacy_runtime
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
}

wait_for_health() {
    if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        return 1
    fi
    if [ "$HEALTH_ENABLED" -eq 0 ]; then
        return 0
    fi
    local attempt=0
    local max_attempts=30
    while [ "$attempt" -lt "$max_attempts" ]; do
        if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
            return 1
        fi
        if curl -fsS "http://127.0.0.1:${HEALTH_PORT}/healthz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    return 1
}

show_recent_logs() {
    if command -v journalctl >/dev/null 2>&1; then
        journalctl -u "$SERVICE_NAME" -n 30 --no-pager || true
    fi
}

start_service() {
    if systemctl is-enabled "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl restart "$SERVICE_NAME"
    else
        systemctl start "$SERVICE_NAME"
    fi
    if ! wait_for_health; then
        log_error "Service failed health check"
        show_recent_logs
        return 1
    fi
}

perform_install() {
    validate_install_request
    maybe_migrate_legacy_install layout
    detect_current_state
    require_reconfigure_confirmation
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_binary
    stage_gbctl
    render_config
    render_service
    backup_existing_state
    install_staged_files
    start_service

    log_info "Installation succeeded"
    log_info "Service: ${SERVICE_NAME}"
    log_info "Config: ${CONFIG_FILE}"
    log_info "Credentials: ${CREDENTIALS_FILE}"
    if [ "$HEALTH_ENABLED" -eq 1 ]; then
        log_info "Health: http://127.0.0.1:${HEALTH_PORT}/healthz"
    fi
    log_info "CLI: ${CLI_PATH}  (run '${CLI_PATH} list' if gbctl is not in PATH)"
}

perform_upgrade() {
    maybe_migrate_legacy_install layout
    detect_current_state
    if [ "$CURRENT_STATE" = "fresh" ]; then
        log_warn "No existing install found; falling back to install"
        perform_install
        return
    fi
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_binary
    stage_gbctl
    render_service
    backup_existing_state
    stop_existing_service
    install -m 755 "$TMP_DIR/gboard-node" "$BINARY_PATH"
    install -m 755 "$TMP_DIR/gbctl" "$CLI_PATH"
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    retire_legacy_runtime
    systemctl daemon-reload
    systemctl restart "$SERVICE_NAME"
    if ! wait_for_health; then
        log_error "Upgrade health check failed"
        show_recent_logs
        return 1
    fi
    log_info "Upgrade succeeded"
}

confirm_uninstall() {
    if [ "$YES" -eq 1 ]; then
        return
    fi
    echo
    read -r -p "Proceed with uninstall? [y/N]: " answer
    if ! [[ "$answer" =~ ^[Yy]$ ]]; then
        log_warn "Uninstall cancelled"
        exit 0
    fi
}

perform_uninstall() {
    maybe_migrate_legacy_install layout
    confirm_uninstall
    if [ -f "$SERVICE_PATH" ] || [ -f "$LEGACY_SERVICE_PATH" ] \
        || systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1 \
        || systemctl is-active "$LEGACY_SERVICE_NAME" >/dev/null 2>&1; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl stop "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl disable "$LEGACY_SERVICE_NAME" >/dev/null 2>&1 || true
        rm -f "$SERVICE_PATH" "$LEGACY_SERVICE_PATH"
        systemctl daemon-reload || true
    fi
    rm -f "$BINARY_PATH" "$LEGACY_BINARY_PATH"
    rm -f "$CLI_PATH" "$CLI_PATH_USR_BIN"
    rm -f "$LEGACY_CLI_PATH" "$LEGACY_CLI_PATH_USR_BIN"
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$INSTALL_ROOT" "$LEGACY_INSTALL_ROOT"
        log_info "Removed ${INSTALL_ROOT}"
    else
        rm -f "$INSTALL_META" "$LEGACY_INSTALL_META"
        if [ -d "$INSTALL_ROOT" ]; then
            log_info "Config preserved under ${INSTALL_ROOT}"
        elif [ -d "$LEGACY_INSTALL_ROOT" ]; then
            log_info "Config preserved under ${LEGACY_INSTALL_ROOT}"
        fi
    fi
    log_info "Uninstall complete"
}

perform_status() {
    maybe_migrate_legacy_install runtime
    detect_current_state
    echo
    echo -e "${BOLD}gboard-node install status${NC}"
    echo "  state:   ${CURRENT_STATE}"
    local meta="$INSTALL_META"
    local cfg="$CONFIG_FILE"
    local svc="$SERVICE_NAME"
    local cli="$CLI_PATH"
    if [ ! -f "$meta" ] && [ -f "$LEGACY_INSTALL_META" ]; then
        meta="$LEGACY_INSTALL_META"
    fi
    if [ ! -f "$cfg" ] && [ -f "$LEGACY_CONFIG_FILE" ]; then
        cfg="$LEGACY_CONFIG_FILE"
    fi
    if [ ! -f "$SERVICE_PATH" ] && [ -f "$LEGACY_SERVICE_PATH" ]; then
        svc="$LEGACY_SERVICE_NAME"
    fi
    if [ ! -x "$cli" ] && [ -x "$LEGACY_CLI_PATH" ]; then
        cli="$LEGACY_CLI_PATH"
    fi
    if [ -f "$cfg" ]; then
        echo "  config:  ${cfg}"
    fi
    if [ -f "$meta" ]; then
        echo "  meta:    ${meta}"
        if [ -x "$cli" ]; then
            "$cli" list 2>/dev/null || true
        else
            # Simple key extraction from JSON (no Python needed)
            local val
            for key in config_mode version latest_instance_id instance_count updated_at; do
                val=$(sed -n "s/.*\"${key}\": *\"\{0,1\}\([^\"]*\)\"\{0,1\}.*/\1/p" "$meta" | head -1)
                val="${val%,}"  # strip trailing comma from numeric JSON values
                [ -n "$val" ] && echo "  ${key}: ${val}"
            done
        fi
    fi
    if [ -f "$SERVICE_PATH" ] || [ -f "$LEGACY_SERVICE_PATH" ]; then
        echo "  service: ${svc}"
        systemctl status "$svc" --no-pager || true
    fi
    if [ -d "$LEGACY_INSTALL_ROOT" ] && [ -d "$INSTALL_ROOT" ]; then
        echo "  leftover: ${LEGACY_INSTALL_ROOT} (not removed; ${INSTALL_ROOT} is active)"
    fi
}

main() {
    parse_args "$@"
    case "$ACTION" in
        help)
            usage
            exit 0
            ;;
        status)
            ensure_systemd
            perform_status
            exit 0
            ;;
    esac

    check_root
    detect_arch
    detect_os
    ensure_systemd
    install_dependencies

    case "$ACTION" in
        install)
            perform_install
            ;;
        upgrade)
            perform_upgrade
            ;;
        uninstall)
            perform_uninstall
            ;;
        *)
            log_error "Unknown action: $ACTION"
            usage
            exit 1
            ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
