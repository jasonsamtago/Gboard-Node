#!/usr/bin/env bash
# 官方 #62 反例：缺 freebsd 成品就靜默改拿 linux（測必須紅）
ARCH=""
OS=""
DOWNLOAD_URL=""
SERVICE_NAME="gboard-node.service"
SERVICE_PATH="/etc/systemd/system/gboard-node.service"
DEFAULT_DOWNLOAD_BASE="https://example.invalid/releases"

detect_arch() { ARCH="amd64"; }

detect_os() {
    case "$(uname -s)" in
        FreeBSD|freebsd) OS="freebsd" ;;
        *) OS="linux" ;;
    esac
}

resolve_download_url() {
    DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/dev/${1}"
}

stage_binary() {
    resolve_download_url "gboard-node-freebsd-${ARCH}"
    echo "Downloading binary: ${DOWNLOAD_URL}"
    if ! curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR:-/tmp}/gboard-node"; then
        resolve_download_url "gboard-node-linux-${ARCH}"
        echo "Downloading binary: ${DOWNLOAD_URL}"
        curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR:-/tmp}/gboard-node" || true
    fi
}

render_service() {
    cat >"${TMP_DIR}/${SERVICE_NAME}" <<'EOF'
[Unit]
Description=Gboard Node Backend
EOF
}
