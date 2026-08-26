#!/usr/bin/env bash
# 官方 #62 反例：未知 OS 靜默當 linux（測必須紅）
ARCH=""
OS=""
DOWNLOAD_URL=""
SERVICE_NAME="gboard-node.service"
SERVICE_PATH="/etc/systemd/system/gboard-node.service"
DEFAULT_DOWNLOAD_BASE="https://example.invalid/releases"

detect_arch() { ARCH="amd64"; }

detect_os() {
    # 未知 kernel 仍當 linux
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
EOF
}
