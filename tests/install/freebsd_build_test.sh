#!/usr/bin/env bash
# 官方 cedar2025/Xboard-Node #62 — 審核2 ATDD 鎖定測
# 限界：Makefile＋install.sh。編譯檔＝make build-freebsd → gboard-node-freebsd-${ARCH}
# 自適應＝同一條 curl|bash 依 uname 選該成品＋寫 rc.d（不是 systemd）
# 不做 ports/pkg、OpenBSD／macOS、核心移植、擴大發行面。
# 本輪只加測，不准改 production。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${ROOT}/install.sh"
MAKEFILE="${ROOT}/Makefile"
DATA="$(cd "$(dirname "${BASH_SOURCE[0]}")/testdata" && pwd)"
TIMEOUT_SECS=5
CASE="${1:-all}"

PASS=0
FAIL=0
RESOLVE_KIND=""
RESOLVE_SERVICE=""
RESOLVE_URLS=""
RESOLVE_OUT=""
RESOLVE_RC=""

pass() {
    PASS=$((PASS + 1))
    echo "PASS: $*"
}

fail() {
    FAIL=$((FAIL + 1))
    echo "FAIL: $*" >&2
}

die() {
    echo "FATAL: $*" >&2
    exit 1
}

[ -f "$INSTALL_SH" ] || die "install.sh not found: $INSTALL_SH"
[ -f "$MAKEFILE" ] || die "Makefile not found: $MAKEFILE"
command -v timeout >/dev/null 2>&1 || die "timeout(1) is required"

link_essentials() {
    local dest="$1"
    mkdir -p "$dest"
    local tools=(
        bash sh timeout cat grep sed awk mkdir mktemp rm cp ls mv
        uname id cut tr head find chmod date realpath dirname basename
        echo true false env sleep printf mkfifo touch wc install
    )
    local t src
    for t in "${tools[@]}"; do
        src="$(command -v "$t" 2>/dev/null || true)"
        if [ -n "$src" ] && [ ! -e "$dest/$t" ]; then
            ln -s "$src" "$dest/$t"
        fi
    done
}

write_fake_uname() {
    local dest="$1"
    local kernel="$2"
    local machine="$3"
    rm -f "$dest/uname"
    cat >"$dest/uname" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
    -s) echo ${kernel} ;;
    -m) echo ${machine} ;;
    -a) echo "${kernel} testhost 14.0-RELEASE ${machine}" ;;
    *) echo ${kernel} ;;
esac
EOF
    chmod +x "$dest/uname"
}

# curl_mode=ok：任何 URL 都寫假二進位
# curl_mode=freebsd_missing：含 freebsd 的 URL 回 404；linux 成功
write_fake_curl() {
    local dest="$1"
    local log="$2"
    local mode="${3:-ok}"
    cat >"$dest/curl" <<EOF
#!/usr/bin/env bash
out=""
url=""
while [ \$# -gt 0 ]; do
    case "\$1" in
        -o|--output)
            out="\$2"
            shift 2
            ;;
        -*)
            shift
            ;;
        *)
            url="\$1"
            shift
            ;;
    esac
done
printf '%s\n' "\$url" >>$(printf '%q' "$log")
if [ "$(printf '%q' "$mode")" = "freebsd_missing" ] && echo "\$url" | grep -q freebsd; then
    echo "404 missing freebsd artifact: \$url" >&2
    exit 1
fi
if [ -z "\$out" ]; then
    echo "fake-curl: missing -o" >&2
    exit 1
fi
cat >"\$out" <<'FAKEBIN'
#!/usr/bin/env bash
if [ "\${1:-}" = "-v" ] || [ "\${1:-}" = "version" ]; then
    echo "gboard-node 0.0.0-issue62"
    exit 0
fi
exit 0
FAKEBIN
chmod +x "\$out"
EOF
    chmod +x "$dest/curl"
}

run_timeout() {
    local _outvar="$1"
    local _rcvar="$2"
    shift 2
    if [ "${1:-}" = "--" ]; then
        shift
    fi
    local _out _rc
    set +e
    _out="$(timeout --signal=TERM "${TIMEOUT_SECS}" "$@" 2>&1)"
    _rc=$?
    set -e
    printf -v "$_outvar" '%s' "$_out"
    printf -v "$_rcvar" '%s' "$_rc"
}

# RESOLVE_KIND= freebsd | linux | explicit | other
# RESOLVE_SERVICE= rcd | systemd | none
resolve_with_uname() {
    local script="$1"
    local kernel="$2"
    local machine="$3"
    local curl_mode="${4:-ok}"
    local work minpath curl_log empty
    work="$(mktemp -d)"
    minpath="$work/bin"
    link_essentials "$minpath"
    write_fake_uname "$minpath" "$kernel" "$machine"
    curl_log="$work/curl.urls"
    write_fake_curl "$minpath" "$curl_log" "$curl_mode"
    empty="$work/cwd"
    mkdir -p "$empty"
    : >"$curl_log"

    run_timeout RESOLVE_OUT RESOLVE_RC -- env PATH="$minpath" bash -c '
        set -euo pipefail
        cd "$3"
        # shellcheck disable=SC1090
        source "$1"
        trap - ERR EXIT
        for fn in detect_arch detect_os detect_goos detect_platform; do
            if declare -F "$fn" >/dev/null 2>&1; then
                "$fn"
            fi
        done
        TMP_DIR="$(mktemp -d)"
        export TMP_DIR
        if declare -F stage_binary >/dev/null 2>&1; then
            stage_binary
        fi
        if declare -F render_service >/dev/null 2>&1; then
            render_service
        elif declare -F write_service_unit >/dev/null 2>&1; then
            write_service_unit "${TMP_DIR}/${SERVICE_NAME:-gboard-node.service}"
        fi
        echo "RESOLVE_DOWNLOAD_URL=${DOWNLOAD_URL:-}"
        echo "RESOLVE_ARCH=${ARCH:-}"
        echo "RESOLVE_OS=${OS:-}"
        echo "RESOLVE_SERVICE_PATH=${SERVICE_PATH:-}"
        echo "RESOLVE_SERVICE_NAME=${SERVICE_NAME:-}"
        if [ -n "${TMP_DIR:-}" ]; then
            for f in "$TMP_DIR"/*; do
                [ -f "$f" ] || continue
                echo "RESOLVE_FILE=$(basename "$f")"
                sed -n "1,20p" "$f" | sed "s/^/RESOLVE_FILEBODY /"
            done
        fi
    ' _ "$script" "$curl_log" "$empty"

    RESOLVE_URLS="$(cat "$curl_log" 2>/dev/null || true)"
    local blob
    blob="${RESOLVE_URLS}"$'\n'"${RESOLVE_OUT}"

    # 出現 gboard-node-linux-* ＝當 linux（含缺 freebsd 卻改拿 linux）
    if echo "$blob" | grep -q 'gboard-node-linux-'; then
        RESOLVE_KIND="linux"
    elif [ "$RESOLVE_RC" -ne 0 ]; then
        RESOLVE_KIND="explicit"
    elif echo "$blob" | grep -q 'gboard-node-freebsd-'; then
        RESOLVE_KIND="freebsd"
    else
        RESOLVE_KIND="other"
    fi

    if echo "$blob" | grep -q 'rc.d'; then
        RESOLVE_SERVICE="rcd"
    elif echo "$blob" | grep -qE 'rc\.subr|PROVIDE:|run_rc_command|rcvar='; then
        RESOLVE_SERVICE="rcd"
    elif echo "$blob" | grep -qE 'systemd|\[Unit\]'; then
        RESOLVE_SERVICE="systemd"
    else
        RESOLVE_SERVICE="none"
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------
# ATDD-1 Happy：uname=FreeBSD 拿 gboard-node-freebsd-${ARCH} 並寫 rc.d
# ---------------------------------------------------------------------------

test_happy_freebsd_artifact_and_rcd() {
    echo "=== ATDD-1 Happy: uname=FreeBSD 拿 gboard-node-freebsd-\${ARCH} 並寫 rc.d ==="

    if ! grep -Eq 'gboard-node-freebsd-\$\{ARCH\}|gboard-node-freebsd-' "$INSTALL_SH"; then
        fail "install.sh 沒有 gboard-node-freebsd-\${ARCH}（現況硬編碼 gboard-node-linux-\${ARCH}）"
    else
        pass "靜態：install.sh 會選 gboard-node-freebsd-\${ARCH}"
    fi

    if ! grep -q 'rc.d' "$INSTALL_SH"; then
        fail "install.sh 沒有 rc.d（現況硬綁 systemd）"
    else
        pass "靜態：install.sh 會寫 rc.d"
    fi

    resolve_with_uname "$INSTALL_SH" "FreeBSD" "amd64" ok
    echo "kind=${RESOLVE_KIND} service=${RESOLVE_SERVICE} rc=${RESOLVE_RC}"
    echo "urls=${RESOLVE_URLS}"
    echo "out=${RESOLVE_OUT}"

    if [ "$RESOLVE_KIND" != "freebsd" ]; then
        fail "uname=FreeBSD 必須拿 gboard-node-freebsd-\${ARCH}，不得當 linux（kind=${RESOLVE_KIND} urls=${RESOLVE_URLS}）"
    else
        pass "uname=FreeBSD 拿到 gboard-node-freebsd-\${ARCH}"
    fi

    if [ "$RESOLVE_SERVICE" != "rcd" ]; then
        fail "uname=FreeBSD 必須寫 rc.d，不是 systemd（service=${RESOLVE_SERVICE} path 見 out）"
    else
        pass "uname=FreeBSD 寫入 rc.d"
    fi
}

# ---------------------------------------------------------------------------
# ATDD-2 邊界：Linux 仍走 systemd＋*-linux-*
# ---------------------------------------------------------------------------

test_boundary_linux_systemd() {
    echo "=== ATDD-2 邊界: Linux 仍走 systemd＋*-linux-* ==="

    if ! grep -Eq 'gboard-node-linux-\$\{ARCH\}|gboard-node-linux-' "$INSTALL_SH"; then
        fail "install.sh 失去 *-linux-*（不准改 Linux 路徑）"
    else
        pass "install.sh 仍有 *-linux-*"
    fi

    if ! grep -q 'systemd' "$INSTALL_SH"; then
        fail "install.sh 失去 systemd（不准改 Linux 路徑）"
    else
        pass "install.sh 仍有 systemd"
    fi

    resolve_with_uname "$INSTALL_SH" "Linux" "x86_64" ok
    echo "linux kind=${RESOLVE_KIND} service=${RESOLVE_SERVICE} rc=${RESOLVE_RC}"
    echo "linux urls=${RESOLVE_URLS}"

    if [ "$RESOLVE_KIND" != "linux" ]; then
        fail "uname=Linux 必須仍拿 *-linux-*（kind=${RESOLVE_KIND} urls=${RESOLVE_URLS}）"
    else
        pass "uname=Linux 仍拿 *-linux-*"
    fi

    if [ "$RESOLVE_SERVICE" != "systemd" ]; then
        fail "uname=Linux 必須仍走 systemd（service=${RESOLVE_SERVICE}）"
    else
        pass "uname=Linux 仍走 systemd"
    fi
}

# ---------------------------------------------------------------------------
# ATDD-3 失敗：未知 OS／缺 freebsd 成品要明示失敗，不准當 linux
# ---------------------------------------------------------------------------

test_fail_unknown_or_missing_must_explicit() {
    echo "=== ATDD-3 失敗: 未知 OS／缺 freebsd 成品必須明示失敗，不准當 linux ==="

    resolve_with_uname "$INSTALL_SH" "UnknownOS" "amd64" ok
    echo "unknown kind=${RESOLVE_KIND} rc=${RESOLVE_RC} urls=${RESOLVE_URLS}"
    echo "unknown out=${RESOLVE_OUT}"
    if [ "$RESOLVE_KIND" = "linux" ]; then
        fail "未知 OS 被當 linux（靜默拿 gboard-node-linux-\${ARCH}）。urls=${RESOLVE_URLS}"
    elif [ "$RESOLVE_KIND" = "explicit" ]; then
        pass "未知 OS 明示失敗（不准當 linux）"
    else
        fail "未知 OS 必須明示失敗，不准當 linux（kind=${RESOLVE_KIND} out=${RESOLVE_OUT}）"
    fi

    resolve_with_uname "$INSTALL_SH" "FreeBSD" "amd64" freebsd_missing
    echo "missing kind=${RESOLVE_KIND} rc=${RESOLVE_RC} urls=${RESOLVE_URLS}"
    echo "missing out=${RESOLVE_OUT}"
    if [ "$RESOLVE_KIND" = "linux" ]; then
        fail "缺 freebsd 成品卻當 linux（靜默改拿 gboard-node-linux-\${ARCH}）。urls=${RESOLVE_URLS}"
    elif [ "$RESOLVE_KIND" = "explicit" ]; then
        pass "缺 freebsd 成品明示失敗（不准當 linux）"
    else
        fail "缺 freebsd 成品必須明示失敗，不准當 linux（kind=${RESOLVE_KIND} out=${RESOLVE_OUT}）"
    fi
}

# ---------------------------------------------------------------------------
# fixture 契約：對齊 ATDD 檔名與 rc.d（正例綠、反例被辨識）
# ---------------------------------------------------------------------------

test_fixture_contracts() {
    echo "=== fixture 契約: ATDD 檔名／rc.d／明示失敗 ==="

    resolve_with_uname "${DATA}/issue62_fixture_install_hardcode_linux.sh" "FreeBSD" "amd64" ok
    if [ "$RESOLVE_KIND" = "linux" ] && [ "$RESOLVE_SERVICE" = "systemd" ]; then
        pass "反例：硬編碼 linux＋systemd 被辨識"
    else
        fail "硬編碼夾具應為 linux＋systemd，got kind=${RESOLVE_KIND} service=${RESOLVE_SERVICE}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_unknown_as_linux.sh" "UnknownOS" "amd64" ok
    if [ "$RESOLVE_KIND" = "linux" ]; then
        pass "反例：未知 OS 當 linux 被辨識"
    else
        fail "未知當 linux 夾具應為 kind=linux，got ${RESOLVE_KIND}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_missing_fallback_linux.sh" "FreeBSD" "amd64" freebsd_missing
    if [ "$RESOLVE_KIND" = "linux" ]; then
        pass "反例：缺 freebsd 成品改拿 linux 被辨識"
    else
        fail "缺成品 fallback 夾具應為 kind=linux，got ${RESOLVE_KIND} urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "FreeBSD" "amd64" ok
    if [ "$RESOLVE_KIND" = "freebsd" ] && [ "$RESOLVE_SERVICE" = "rcd" ]; then
        pass "正例：FreeBSD → gboard-node-freebsd-\${ARCH}＋rc.d"
    else
        fail "自適應夾具 FreeBSD 應為 freebsd＋rcd，got kind=${RESOLVE_KIND} service=${RESOLVE_SERVICE} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "Linux" "x86_64" ok
    if [ "$RESOLVE_KIND" = "linux" ] && [ "$RESOLVE_SERVICE" = "systemd" ]; then
        pass "正例：Linux → systemd＋*-linux-*"
    else
        fail "自適應夾具 Linux 應為 linux＋systemd，got kind=${RESOLVE_KIND} service=${RESOLVE_SERVICE}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "UnknownOS" "amd64" ok
    if [ "$RESOLVE_KIND" = "explicit" ]; then
        pass "正例：未知 OS 明示失敗"
    else
        fail "自適應夾具未知 OS 應明示失敗，got kind=${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "FreeBSD" "amd64" freebsd_missing
    if [ "$RESOLVE_KIND" = "explicit" ]; then
        pass "正例：缺 freebsd 成品明示失敗"
    elif [ "$RESOLVE_KIND" = "linux" ]; then
        fail "自適應夾具缺成品不准當 linux（got linux）"
    else
        fail "自適應夾具缺成品應明示失敗，got kind=${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi
}

# ---------------------------------------------------------------------------

case "$CASE" in
    happy) test_happy_freebsd_artifact_and_rcd ;;
    boundary) test_boundary_linux_systemd ;;
    fail) test_fail_unknown_or_missing_must_explicit ;;
    fixtures) test_fixture_contracts ;;
    all)
        test_happy_freebsd_artifact_and_rcd
        test_boundary_linux_systemd
        test_fail_unknown_or_missing_must_explicit
        test_fixture_contracts
        ;;
    *)
        die "unknown case: $CASE (happy|boundary|fail|fixtures|all)"
        ;;
esac

echo
echo "issue62 summary: PASS=${PASS} FAIL=${FAIL}"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
exit 0
