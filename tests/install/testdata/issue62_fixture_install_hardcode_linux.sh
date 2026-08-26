#!/usr/bin/env bash
# 官方 #62 反例：不理 uname，硬拿 gboard-node-linux-${ARCH} 並寫 systemd
ARCH=""
OS=""
DOWNLOAD_URL=""
SERVICE_NAME="gboard-node.service"
SERVICE_PATH="/etc/systemd/system/gboard-node.service"
DEFAULT_DOWNLOAD_BASE="https://example.invalid/releases"

detect_arch() {
    local raw
    raw=$(uname -m)
    case "$raw" in
        x86_64|amd64) ARCH="amd64" ;;
        *) ARCH="$raw" ;;
    esac
}

detect_os() {
    OS="linux"
}

resolve_download_url() {
    DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/dev/${1}"
}

stage_binary() {
    resolve_download_url "gboard-node-linux-${ARCH}"
    echo "Downloading binary: ${DOWNLOAD_URL}"
}

render_service() {
    cat >"${TMP_DIR}/${SERVICE_NAME}" <<'EOF'
[Unit]
Description=Gboard Node Backend
[Service]
ExecStart=/usr/local/bin/gboard-node
EOF
    echo "Wrote systemd: ${SERVICE_PATH}"
}
