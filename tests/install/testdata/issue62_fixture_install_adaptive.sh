#!/usr/bin/env bash
# 官方 #62 正例：uname=FreeBSD 解析到 freebsd 二進位 URL；linux 仍走 linux（#18 不回歸）
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
    local kernel
    kernel=$(uname -s)
    case "$kernel" in
        FreeBSD|freebsd) OS="freebsd" ;;
        Linux|linux) OS="linux" ;;
        Darwin|darwin) OS="darwin" ;;
        *) OS=$(echo "$kernel" | tr 'A-Z' 'a-z') ;;
    esac
}

resolve_download_url() {
    DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/dev/${1}"
}

stage_binary() {
    resolve_download_url "gboard-node-${OS}-${ARCH}"
    echo "Downloading binary: ${DOWNLOAD_URL}"
}

stage_gbctl() {
    resolve_download_url "gbctl-${OS}-${ARCH}"
    echo "Downloading gbctl: ${DOWNLOAD_URL}"
}
