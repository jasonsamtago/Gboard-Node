#!/usr/bin/env bash
# 官方 #62 反例：uname=FreeBSD 仍硬編碼 linux 包（測必須紅）
# 函式名對齊 install.sh：detect_arch／detect_os／resolve_download_url／stage_binary／stage_gbctl
ARCH=""
OS=""
DOWNLOAD_URL=""
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
    OS="unknown"
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
