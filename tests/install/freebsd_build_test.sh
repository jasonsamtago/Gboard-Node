#!/usr/bin/env bash
# 官方 cedar2025/Xboard-Node #62 — FreeBSD 編譯產物＋install.sh 自適應
# https://github.com/cedar2025/Xboard-Node/issues/62
# 審核 2：本輪只加鎖定測。不准改 production。
# 範圍：編譯產物＋install.sh 自適應。不准擴大到真機服務／kernel／jail。
# 不要重做 #18（本機 linux 安裝）；邊界只鎖不回歸。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${ROOT}/install.sh"
MAKEFILE="${ROOT}/Makefile"
CI_YML="${ROOT}/.github/workflows/ci.yml"
DATA="$(cd "$(dirname "${BASH_SOURCE[0]}")/testdata" && pwd)"
TIMEOUT_SECS=5
CASE="${1:-all}"

PASS=0
FAIL=0
RESOLVE_KIND=""
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
    # 先摘掉 link_essentials 的 uname symlink，避免 cat 寫穿系統 /usr/bin/uname
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

write_fake_curl() {
    local dest="$1"
    local log="$2"
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
if [ "\${1:-}" = "config" ] && [ "\${2:-}" = "init" ]; then
    output=""
    creds=""
    meta=""
    while [ \$# -gt 0 ]; do
        case "\$1" in
            --output) output="\$2"; shift 2 ;;
            --credentials-out) creds="\$2"; shift 2 ;;
            --meta) meta="\$2"; shift 2 ;;
            *) shift ;;
        esac
    done
    [ -n "\$output" ] || exit 1
    printf 'panel:\\n  url: https://panel.example.com\\n' >"\$output"
    [ -n "\$creds" ] && printf 'TOKEN=TOKEN\\n' >"\$creds"
    [ -n "\$meta" ] && printf '{}\\n' >"\$meta"
    echo "INSTANCE_ID=issue62-freebsd"
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

# 對 script source 後走 detect_* ＋ stage_binary／stage_gbctl。
# RESOLVE_KIND= freebsd | linux | reject | hang | mixed | other
resolve_with_uname() {
    local script="$1"
    local kernel="$2"
    local machine="$3"
    local work minpath curl_log empty
    work="$(mktemp -d)"
    minpath="$work/bin"
    link_essentials "$minpath"
    write_fake_uname "$minpath" "$kernel" "$machine"
    curl_log="$work/curl.urls"
    write_fake_curl "$minpath" "$curl_log"
    empty="$work/cwd"
    mkdir -p "$empty"
    : >"$curl_log"

    run_timeout RESOLVE_OUT RESOLVE_RC -- env PATH="$minpath" bash -c '
        set -euo pipefail
        cd "$3"
        # shellcheck disable=SC1090
        source "$1"
        trap - ERR EXIT
        for fn in detect_arch detect_os detect_goos detect_platform detect_kernel detect_os_family; do
            if declare -F "$fn" >/dev/null 2>&1; then
                "$fn"
            fi
        done
        TMP_DIR="$(mktemp -d)"
        if declare -F stage_binary >/dev/null 2>&1; then
            stage_binary
        fi
        if declare -F stage_gbctl >/dev/null 2>&1; then
            stage_gbctl
        fi
        echo "RESOLVE_DOWNLOAD_URL=${DOWNLOAD_URL:-}"
        echo "RESOLVE_ARCH=${ARCH:-}"
        echo "RESOLVE_OS=${OS:-}"
    ' _ "$script" "$curl_log" "$empty"

    RESOLVE_URLS="$(cat "$curl_log" 2>/dev/null || true)"
    local blob
    blob="${RESOLVE_URLS}"$'\n'"${RESOLVE_OUT}"
    if [ "$RESOLVE_RC" -eq 124 ] || [ "$RESOLVE_RC" -eq 137 ]; then
        RESOLVE_KIND="hang"
    elif [ "$RESOLVE_RC" -ne 0 ]; then
        RESOLVE_KIND="reject"
    elif echo "$blob" | grep -qiE 'gboard-node-freebsd-|gbctl-freebsd-'; then
        if echo "$blob" | grep -qiE 'gboard-node-linux-|gbctl-linux-'; then
            RESOLVE_KIND="mixed"
        else
            RESOLVE_KIND="freebsd"
        fi
    elif echo "$blob" | grep -qiE 'gboard-node-linux-|gbctl-linux-'; then
        RESOLVE_KIND="linux"
    else
        RESOLVE_KIND="other"
    fi
    rm -rf "$work"
}

# ---------------------------------------------------------------------------
# ATDD-1 Happy：正式 freebsd 目標由 Go 測鎖；這裡鎖 install.sh 自適應 URL
# ---------------------------------------------------------------------------

test_happy_freebsd_adaptive() {
    echo "=== ATDD-1 Happy: uname=FreeBSD 必須解析到 freebsd 二進位 URL ==="

    if grep -Eq 'gboard-node-linux-\$\{ARCH\}|gbctl-linux-\$\{ARCH\}' "$INSTALL_SH" \
        && ! grep -Eqi 'freebsd' "$INSTALL_SH"; then
        fail "install.sh 下載硬編碼 *-linux-\${ARCH}（stage_binary／stage_gbctl），全文無 freebsd 自適應"
    else
        pass "靜態：install.sh 下載路徑能依 OS 變化（含 freebsd）"
    fi

    resolve_with_uname "$INSTALL_SH" "FreeBSD" "amd64"
    echo "resolve kind=${RESOLVE_KIND} rc=${RESOLVE_RC}"
    echo "urls=${RESOLVE_URLS}"
    echo "out=${RESOLVE_OUT}"

    case "$RESOLVE_KIND" in
        hang)
            fail "uname=FreeBSD 時 install.sh 掛死（${TIMEOUT_SECS}s）。out=${RESOLVE_OUT}"
            ;;
        reject)
            fail "uname=FreeBSD 時 install.sh 直接拒絕 rc=${RESOLVE_RC}。out=${RESOLVE_OUT}"
            ;;
        linux)
            fail "uname=FreeBSD 仍下 linux 包（硬編碼 gboard-node-linux-\${ARCH}）。urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
            ;;
        mixed)
            fail "uname=FreeBSD 解析到混有 linux 的 URL。urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
            ;;
        freebsd)
            pass "uname=FreeBSD 解析到 freebsd 二進位 URL（urls=${RESOLVE_URLS}）"
            ;;
        *)
            fail "uname=FreeBSD 未解析到 freebsd 二進位 URL（kind=${RESOLVE_KIND}）。urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
            ;;
    esac
}

# ---------------------------------------------------------------------------
# ATDD-2 邊界：linux／amd64 本機安裝不回歸（#18）
# ---------------------------------------------------------------------------

test_boundary_linux_amd64() {
    echo "=== ATDD-2 邊界: linux／amd64 本機安裝不回歸（#18） ==="

    if ! grep -Eq 'GOOS=linux' "$MAKEFILE"; then
        fail "Makefile 失去 GOOS=linux 正式目標（#18／linux 發布回歸）"
    else
        pass "Makefile 仍有 GOOS=linux"
    fi

    if ! grep -Eq 'build-linux' "$MAKEFILE"; then
        fail "Makefile 失去 build-linux（linux／amd64 發布回歸）"
    else
        pass "Makefile 仍有 build-linux"
    fi

    if [ -f "$CI_YML" ]; then
        if ! grep -Eq 'goos:[[:space:]]*linux' "$CI_YML"; then
            fail "CI 失去 linux 矩陣（linux／amd64 發布回歸）"
        else
            pass "CI 仍有 linux 矩陣"
        fi
    fi

    resolve_with_uname "$INSTALL_SH" "Linux" "x86_64"
    echo "linux resolve kind=${RESOLVE_KIND} rc=${RESOLVE_RC}"
    echo "linux urls=${RESOLVE_URLS}"

    case "$RESOLVE_KIND" in
        hang)
            fail "uname=Linux 時 install.sh 掛死。out=${RESOLVE_OUT}"
            ;;
        reject)
            fail "uname=Linux 時 install.sh 拒絕（#18 回歸）。out=${RESOLVE_OUT}"
            ;;
        linux)
            pass "uname=Linux／amd64 仍解析到 linux 二進位 URL（#18 不回歸）"
            ;;
        freebsd)
            fail "uname=Linux 卻解析到 freebsd URL（linux 本機安裝回歸）。urls=${RESOLVE_URLS}"
            ;;
        *)
            fail "uname=Linux 未解析到 linux 二進位 URL（kind=${RESOLVE_KIND}）。urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
            ;;
    esac
}

# ---------------------------------------------------------------------------
# ATDD-3 失敗：uname=FreeBSD 仍下 linux／拒絕／掛死 → 必須紅
# ---------------------------------------------------------------------------

test_fail_freebsd_linux_or_reject() {
    echo "=== ATDD-3 失敗: uname=FreeBSD 仍下 linux 包、或拒絕／掛死必須紅 ==="

    resolve_with_uname "$INSTALL_SH" "FreeBSD" "amd64"
    echo "current tip kind=${RESOLVE_KIND} rc=${RESOLVE_RC}"
    echo "current tip urls=${RESOLVE_URLS}"
    echo "current tip out=${RESOLVE_OUT}"

    case "$RESOLVE_KIND" in
        linux)
            fail "現 tip：uname=FreeBSD 仍下 linux 包（stage_binary／stage_gbctl 硬編碼 *-linux-\${ARCH}）。urls=${RESOLVE_URLS}"
            ;;
        reject)
            fail "現 tip：uname=FreeBSD 時腳本直接拒絕 rc=${RESOLVE_RC}。out=${RESOLVE_OUT}"
            ;;
        hang)
            fail "現 tip：uname=FreeBSD 時腳本掛死。out=${RESOLVE_OUT}"
            ;;
        mixed)
            fail "現 tip：uname=FreeBSD 仍混有 linux 包。urls=${RESOLVE_URLS}"
            ;;
        freebsd)
            pass "現 tip：uname=FreeBSD 已解析到 freebsd URL（回歸鎖綠）"
            ;;
        *)
            fail "現 tip：uname=FreeBSD 未自適應（kind=${RESOLVE_KIND}）。urls=${RESOLVE_URLS} out=${RESOLVE_OUT}"
            ;;
    esac
}

# ---------------------------------------------------------------------------
# fixture 契約：checker 能辨識硬編碼 linux／拒絕／掛死／自適應
# ---------------------------------------------------------------------------

test_fixture_contracts() {
    echo "=== fixture 契約: 反例必須被辨識；正例必須是 freebsd／linux ==="

    resolve_with_uname "${DATA}/issue62_fixture_install_hardcode_linux.sh" "FreeBSD" "amd64"
    if [ "$RESOLVE_KIND" = "linux" ]; then
        pass "反例夾具：硬編碼 linux 被辨識（kind=linux）"
    else
        fail "硬編碼 linux 夾具應為 kind=linux，got ${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_reject_freebsd.sh" "FreeBSD" "amd64"
    if [ "$RESOLVE_KIND" = "reject" ]; then
        pass "反例夾具：直接拒絕 FreeBSD 被辨識（kind=reject）"
    else
        fail "拒絕夾具應為 kind=reject，got ${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_hang_freebsd.sh" "FreeBSD" "amd64"
    if [ "$RESOLVE_KIND" = "hang" ]; then
        pass "反例夾具：FreeBSD 掛死被 timeout 抓到（kind=hang）"
    else
        fail "掛死夾具應為 kind=hang，got ${RESOLVE_KIND} rc=${RESOLVE_RC} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "FreeBSD" "amd64"
    if [ "$RESOLVE_KIND" = "freebsd" ]; then
        pass "正例夾具：uname=FreeBSD → freebsd URL"
    else
        fail "自適應夾具在 FreeBSD 應為 kind=freebsd，got ${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi

    resolve_with_uname "${DATA}/issue62_fixture_install_adaptive.sh" "Linux" "x86_64"
    if [ "$RESOLVE_KIND" = "linux" ]; then
        pass "正例夾具：uname=Linux → linux URL（#18 不回歸）"
    else
        fail "自適應夾具在 Linux 應為 kind=linux，got ${RESOLVE_KIND} out=${RESOLVE_OUT}"
    fi
}

# ---------------------------------------------------------------------------

case "$CASE" in
    happy) test_happy_freebsd_adaptive ;;
    boundary) test_boundary_linux_amd64 ;;
    fail) test_fail_freebsd_linux_or_reject ;;
    fixtures) test_fixture_contracts ;;
    all)
        test_happy_freebsd_adaptive
        test_boundary_linux_amd64
        test_fail_freebsd_linux_or_reject
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
