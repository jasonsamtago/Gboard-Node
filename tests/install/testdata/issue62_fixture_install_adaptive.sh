#!/usr/bin/env bash
# 官方 #62 正例：uname=FreeBSD 拿 gboard-node-freebsd-${ARCH} 並寫 rc.d；
# Linux 仍走 systemd＋*-linux-*；未知 OS 明示失敗。
ARCH=""
OS=""
DOWNLOAD_URL=""
SERVICE_NAME=""
SERVICE_PATH=""
DEFAULT_DOWNLOAD_BASE="https://example.invalid/releases"

detect_arch() {
    local raw
    raw=$(uname -m)
    case "$raw" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) ARCH="$raw" ;;
    esac
}

detect_os() {
    case "$(uname -s)" in
        FreeBSD|freebsd) OS="freebsd" ;;
        Linux|linux) OS="linux" ;;
        *)
            echo "[ERROR] Unsupported OS: $(uname -s) (unknown; will not treat as linux)" >&2
            exit 1
            ;;
    esac
}

resolve_download_url() {
    DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/dev/${1}"
}

stage_binary() {
    resolve_download_url "gboard-node-${OS}-${ARCH}"
    echo "Downloading binary: ${DOWNLOAD_URL}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR:-/tmp}/gboard-node" || {
            echo "[ERROR] missing freebsd artifact: ${DOWNLOAD_URL}" >&2
            exit 1
        }
    fi
}

render_service() {
    if [ "$OS" = "freebsd" ]; then
        SERVICE_NAME="gboard-node"
        SERVICE_PATH="/usr/local/etc/rc.d/gboard-node"
        cat >"${TMP_DIR}/gboard-node" <<'EOF'
#!/bin/sh
# PROVIDE: gboard_node
# REQUIRE: NETWORKING
. /etc/rc.subr
name="gboard_node"
rcvar="gboard_node_enable"
command="/usr/local/bin/gboard-node"
load_rc_config $name
run_rc_command "$1"
EOF
        echo "Wrote rc.d: ${SERVICE_PATH}"
    else
        SERVICE_NAME="gboard-node.service"
        SERVICE_PATH="/etc/systemd/system/gboard-node.service"
        cat >"${TMP_DIR}/${SERVICE_NAME}" <<'EOF'
[Unit]
Description=Gboard Node Backend
[Service]
ExecStart=/usr/local/bin/gboard-node
[Install]
WantedBy=multi-user.target
EOF
        echo "Wrote systemd: ${SERVICE_PATH}"
    fi
}
