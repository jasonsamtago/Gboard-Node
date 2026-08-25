#!/usr/bin/env bash
# 官方 #62 反例：偵測到 FreeBSD 就直接拒絕（測必須紅）
ARCH=""
OS=""
DOWNLOAD_URL=""
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
    if [ "$(uname -s)" = "FreeBSD" ]; then
        echo "[ERROR] Unsupported OS: FreeBSD" >&2
        exit 1
    fi
    OS="linux"
}

resolve_download_url() {
    DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/dev/${1}"
}

stage_binary() {
    resolve_download_url "gboard-node-linux-${ARCH}"
    echo "Downloading binary: ${DOWNLOAD_URL}"
}

stage_gbctl() {
    resolve_download_url "gbctl-linux-${ARCH}"
    echo "Downloading gbctl: ${DOWNLOAD_URL}"
}
