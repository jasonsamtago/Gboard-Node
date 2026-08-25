#!/usr/bin/env bash
# 官方 #62 反例：uname=FreeBSD 就空等掛死（測必須紅）
ARCH=""
OS=""
DOWNLOAD_URL=""
DEFAULT_DOWNLOAD_BASE="https://example.invalid/releases"

detect_arch() {
    ARCH="amd64"
}

detect_os() {
    if [ "$(uname -s)" = "FreeBSD" ]; then
        fifo="$(mktemp -u)"
        mkfifo "$fifo"
        read -r -p "FreeBSD unsupported, waiting... " _unused <"$fifo"
    fi
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
