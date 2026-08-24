#!/usr/bin/env bash
# 官方 cedar2025/Xboard-Node #12 — install.sh 無 docker 不得卡死
# 以碼為準（不是修補）：現 tip install.sh 是原生 binary＋systemd，全文無 docker。
# 本輪只加鎖定測。預設路徑不呼叫 docker、不掛起；若未來可選路徑要 docker，
# 缺失時必須 ≤5s 非 0 並印含 docker 的錯誤，不准 read／無限 retry。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${ROOT}/install.sh"
TIMEOUT_SECS=5
CASE="${1:-all}"

PASS=0
FAIL=0

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
command -v timeout >/dev/null 2>&1 || die "timeout(1) is required"

# ---------------------------------------------------------------------------
# PATH helpers: 真的沒有 docker，或有會 hang 的 stub
# ---------------------------------------------------------------------------

link_essentials() {
    local dest="$1"
    mkdir -p "$dest"
    local tools=(
        bash sh timeout cat grep sed awk mkdir mktemp rm cp ls mv
        uname id cut tr head find chmod date realpath dirname basename
        echo true false env sleep printf mkfifo touch wc
    )
    local t src
    for t in "${tools[@]}"; do
        src="$(command -v "$t" 2>/dev/null || true)"
        if [ -n "$src" ] && [ ! -e "$dest/$t" ]; then
            ln -s "$src" "$dest/$t"
        fi
    done
    # 明確排除 docker 家族
    rm -f "$dest/docker" "$dest/docker-compose" "$dest/podman"
}

make_no_docker_path() {
    local dest="$1"
    link_essentials "$dest"
    echo "$dest"
}

make_hanging_docker_stub() {
    local dest="$1"
    local marker="$2"
    mkdir -p "$dest"
    cat >"$dest/docker" <<EOF
#!/usr/bin/env bash
printf 'called %s\n' "\$*" >>$(printf '%q' "$marker")
# 官方卡死＝呼叫 docker 後無進度、長時間不退出
exec sleep infinity
EOF
    chmod +x "$dest/docker"
    # compose 同契約：預設路徑也不准碰
    cat >"$dest/docker-compose" <<EOF
#!/usr/bin/env bash
printf 'called compose %s\n' "\$*" >>$(printf '%q' "$marker")
exec sleep infinity
EOF
    chmod +x "$dest/docker-compose"
}

run_timeout() {
    # 用法：run_timeout <outvar> <rcvar> -- cmd...
    # timeout 124/137 ＝卡死，呼叫端必須當失敗。
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

assert_not_hung() {
    local label="$1"
    local rc="$2"
    local out="$3"
    if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
        fail "${label}: 在 ${TIMEOUT_SECS}s 內未退出（卡死）。output=${out}"
        return 1
    fi
    return 0
}

# ---------------------------------------------------------------------------
# ATDD-1 Happy：PATH 無 docker；靜態＋預檢不呼叫 docker、不掛起
# ---------------------------------------------------------------------------

test_happy_no_docker() {
    echo "=== ATDD-1 Happy: PATH 無 docker，預設原生路徑不呼叫 docker、不掛起 ==="

    local syntax_out syntax_rc
    run_timeout syntax_out syntax_rc -- bash -n "$INSTALL_SH"
    if assert_not_hung "bash -n install.sh" "$syntax_rc" "$syntax_out"; then
        if [ "$syntax_rc" -eq 0 ]; then
            pass "bash -n install.sh 在 ${TIMEOUT_SECS}s 內通過"
        else
            fail "bash -n install.sh rc=${syntax_rc}: ${syntax_out}"
        fi
    fi

    # 靜態：install.sh 不得出現 docker 安裝／等待／呼叫
    if grep -Eiq '(^|[^A-Za-z_])(docker|docker-compose)([^A-Za-z_]|$)' "$INSTALL_SH"; then
        fail "install.sh 出現 docker／docker-compose（預設路徑必須是原生安裝）"
    else
        pass "靜態：install.sh 無 docker／docker-compose 字樣"
    fi

    if grep -Eiq 'docker[-.]?(ce|io|engine|compose)|podman' "$INSTALL_SH"; then
        fail "install.sh 依賴 docker 套件或 compose"
    else
        pass "靜態：install.sh 不安裝 docker 套件"
    fi

    if grep -Eq 'sleep[[:space:]]+infinity|while[[:space:]]+true|until[[:space:]]+false' "$INSTALL_SH"; then
        fail "install.sh 含無限迴圈／sleep infinity（卡死路徑）"
    else
        pass "靜態：install.sh 無 while true／sleep infinity"
    fi

    # read -p 只准用在 uninstall 確認
    local reads
    reads="$(grep -n 'read[[:space:]]' "$INSTALL_SH" || true)"
    local bad_read=0
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        case "$line" in
            *'read -r -p "Proceed with uninstall?'*) continue ;;
            *'while IFS= read -r -d'*) continue ;;
            *)
                echo "  unexpected read: $line" >&2
                bad_read=1
                ;;
        esac
    done <<<"$reads"
    if [ "$bad_read" -eq 0 ]; then
        pass "靜態：read -p 只出現在 uninstall 確認（非 install 卡死）"
    else
        fail "install.sh 在非 uninstall 路徑使用 read（可能卡死）"
    fi

    local work minpath
    work="$(mktemp -d)"
    minpath="$(make_no_docker_path "$work/bin")"
    if [ -x "$minpath/docker" ]; then
        fail "測試夾具 PATH 不該有 docker"
        rm -rf "$work"
        return
    fi
    if PATH="$minpath" command -v docker >/dev/null 2>&1; then
        fail "PATH 無 docker 夾具失敗：command -v docker 仍成功"
        rm -rf "$work"
        return
    fi
    pass "夾具：PATH 無 docker（command -v docker 失敗）"

    local help_out help_rc
    run_timeout help_out help_rc -- env PATH="$minpath" bash "$INSTALL_SH" help
    if assert_not_hung "install.sh help（無 docker）" "$help_rc" "$help_out"; then
        if [ "$help_rc" -eq 0 ] && echo "$help_out" | grep -q 'Gboard-Node Installer'; then
            pass "install.sh help 無 docker 時 ${TIMEOUT_SECS}s 內退出 0"
        else
            fail "install.sh help rc=${help_rc}: ${help_out}"
        fi
    fi

    # source 預檢：parse／validate／detect，不進 install_dependencies／perform_*
    local pre_out pre_rc
    run_timeout pre_out pre_rc -- env PATH="$minpath" bash -c '
        set -euo pipefail
        # shellcheck disable=SC1090
        source "$1"
        parse_args --panel https://panel.example.com --token TOKEN --node-id 1
        validate_install_request
        detect_arch
        detect_os
        [ "$MODE" = "node" ]
        [ "$ACTION" = "install" ]
        echo PRECHECK_OK
    ' _ "$INSTALL_SH"
    if assert_not_hung "source 預檢（無 docker）" "$pre_rc" "$pre_out"; then
        if [ "$pre_rc" -eq 0 ] && echo "$pre_out" | grep -q 'PRECHECK_OK'; then
            pass "source 預檢（parse／validate／detect）無 docker、不掛起、預設原生 install"
        else
            fail "source 預檢 rc=${pre_rc}: ${pre_out}"
        fi
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------
# ATDD-2 邊界：PATH 有 hang 的 docker stub，預設預檢仍不准呼叫
# ---------------------------------------------------------------------------

test_boundary_hanging_stub() {
    echo "=== ATDD-2 邊界: 假 docker stub 會 hang，預設預檢仍不呼叫 ==="

    local work minpath stubdir marker
    work="$(mktemp -d)"
    marker="$work/docker.called"
    minpath="$(make_no_docker_path "$work/bin")"
    stubdir="$work/stub"
    make_hanging_docker_stub "$stubdir" "$marker"
    local path_with_stub="${stubdir}:${minpath}"

    if ! PATH="$path_with_stub" command -v docker >/dev/null 2>&1; then
        fail "hanging stub 未進 PATH"
        rm -rf "$work"
        return
    fi

    local help_out help_rc
    run_timeout help_out help_rc -- env PATH="$path_with_stub" bash "$INSTALL_SH" help
    if assert_not_hung "install.sh help（hanging docker stub）" "$help_rc" "$help_out"; then
        if [ "$help_rc" -eq 0 ]; then
            pass "install.sh help 遇 hanging docker stub 仍 ${TIMEOUT_SECS}s 內退出"
        else
            fail "install.sh help rc=${help_rc}: ${help_out}"
        fi
    fi

    local pre_out pre_rc
    run_timeout pre_out pre_rc -- env PATH="$path_with_stub" bash -c '
        set -euo pipefail
        # shellcheck disable=SC1090
        source "$1"
        parse_args --mode node --panel https://panel.example.com --token TOKEN --node-id 1
        validate_install_request
        detect_arch
        detect_os
        echo PRECHECK_OK
    ' _ "$INSTALL_SH"
    if assert_not_hung "source 預檢（hanging docker stub）" "$pre_rc" "$pre_out"; then
        if [ "$pre_rc" -eq 0 ] && echo "$pre_out" | grep -q 'PRECHECK_OK'; then
            pass "source 預檢遇 hanging docker stub 仍不掛起"
        else
            fail "source 預檢 rc=${pre_rc}: ${pre_out}"
        fi
    fi

    if [ -f "$marker" ]; then
        fail "預設預檢呼叫了 docker stub（marker=$(cat "$marker")）— 預設不得依賴 docker"
    else
        pass "hanging docker stub 未被呼叫（證明預設不依賴 docker）"
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------
# ATDD-3 失敗：若未來可選路徑需要 docker，缺失時必須快速失敗
# ---------------------------------------------------------------------------

test_failfast_if_docker_required() {
    echo "=== ATDD-3 失敗: 可選路徑若需要 docker，缺失時 ≤5s 非 0 並印 docker ==="

    local docker_fns
    docker_fns="$(grep -E '^[[:space:]]*(ensure|require|check|wait)_?docker[[:space:]]*\(\)' "$INSTALL_SH" || true)"

    local work minpath
    work="$(mktemp -d)"
    minpath="$(make_no_docker_path "$work/bin")"

    if [ -n "$docker_fns" ]; then
        local fn fn_out fn_rc any_bad=0
        while IFS= read -r fnline; do
            fn="$(echo "$fnline" | sed -E 's/[[:space:]]*\(\).*//; s/^[[:space:]]*//')"
            [ -z "$fn" ] && continue
            run_timeout fn_out fn_rc -- env PATH="$minpath" bash -c '
                set -euo pipefail
                # shellcheck disable=SC1090
                source "$1"
                '"$fn"'
            ' _ "$INSTALL_SH"
            if ! assert_not_hung "$fn（無 docker）" "$fn_rc" "$fn_out"; then
                any_bad=1
                continue
            fi
            if [ "$fn_rc" -eq 0 ]; then
                fail "$fn 在 docker 缺失時退出 0（必須非 0）"
                any_bad=1
                continue
            fi
            if ! echo "$fn_out" | grep -qi 'docker'; then
                fail "$fn 失敗輸出必須含 docker，got: ${fn_out}"
                any_bad=1
                continue
            fi
            if echo "$fn_out" | grep -Eq 'read -p|Proceed with'; then
                fail "$fn 不准用 read 卡死等 docker"
                any_bad=1
                continue
            fi
            pass "$fn 無 docker 時 ${TIMEOUT_SECS}s 內非 0 並印 docker"
        done <<<"$docker_fns"
        if [ "$any_bad" -eq 0 ] && [ -z "${fn:-}" ]; then
            :
        fi
    else
        pass "現 tip 無 ensure／require／check_docker 函式（預設原生，不強制 docker）"
    fi

    # 契約夾具：未來可選路徑必須長這樣（本輪不改 production）
    local contract="$work/optional_docker_path.sh"
    cat >"$contract" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# 契約：可選路徑需要 docker 時，缺失必須快速失敗，不准 read／無限 retry。
if ! command -v docker >/dev/null 2>&1; then
    echo "[ERROR] docker is required for this optional path but was not found" >&2
    exit 1
fi
docker info >/dev/null
EOF
    chmod +x "$contract"

    local c_out c_rc
    run_timeout c_out c_rc -- env PATH="$minpath" bash "$contract"
    if assert_not_hung "可選 docker 路徑契約（無 docker）" "$c_rc" "$c_out"; then
        if [ "$c_rc" -ne 0 ] && echo "$c_out" | grep -qi 'docker'; then
            pass "契約夾具：docker 缺失時 ${TIMEOUT_SECS}s 內非 0 並印 docker"
        else
            fail "契約夾具未快速失敗或輸出不含 docker rc=${c_rc}: ${c_out}"
        fi
    fi

    # 反例夾具：沒 docker 就 read 空等。非 TTY 的 stdin 會立刻 EOF，
    # 所以用沒有 writer 的 FIFO，才是「卡死」而不是秒退。
    local hang="$work/hang_on_missing_docker.sh"
    cat >"$hang" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if ! command -v docker >/dev/null 2>&1; then
    fifo="$(mktemp -u)"
    mkfifo "$fifo"
    # 禁止的卡死：沒有 docker 就 read 等輸入，永遠沒人寫
    read -r -p "Install docker now? [y/N]: " answer <"$fifo"
fi
EOF
    chmod +x "$hang"
    local h_out h_rc
    run_timeout h_out h_rc -- env PATH="$minpath" bash "$hang"
    if [ "$h_rc" -eq 124 ] || [ "$h_rc" -eq 137 ]; then
        pass "反例夾具：read 等 docker 會在 ${TIMEOUT_SECS}s 被 timeout 抓到（測能打紅）"
    else
        fail "反例夾具應卡在 read，got rc=${h_rc} out=${h_out}"
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------

case "$CASE" in
    happy) test_happy_no_docker ;;
    stub) test_boundary_hanging_stub ;;
    failfast) test_failfast_if_docker_required ;;
    all)
        test_happy_no_docker
        test_boundary_hanging_stub
        test_failfast_if_docker_required
        ;;
    *)
        die "unknown case: $CASE (happy|stub|failfast|all)"
        ;;
esac

echo
echo "issue12 summary: PASS=${PASS} FAIL=${FAIL}"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
exit 0
