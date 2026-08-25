#!/usr/bin/env bash
# 官方 cedar2025/Xboard-Node #18 — 本地安裝版本（無 Docker 必須可用）
# 官方回覆：用 install.sh 啊，不加 --docker 就是普通的下载 go 二进制+配置文件
# 以碼為準（不是修補）：現 tip install.sh 是原生 binary＋systemd，沒有 --docker。
# 本輪只加鎖定測。不要重做 #12（卡死）；本張鎖本機安裝產品路徑。
# 不准改 production。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${ROOT}/install.sh"
README="${ROOT}/README.md"
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
[ -f "$README" ] || die "README.md not found: $README"
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
    rm -f "$dest/docker" "$dest/docker-compose" "$dest/podman"
}

make_no_docker_path() {
    local dest="$1"
    link_essentials "$dest"
    echo "$dest"
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

assert_not_hung() {
    local label="$1"
    local rc="$2"
    local out="$3"
    if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
        fail "${label}: 在 ${TIMEOUT_SECS}s 內未退出（卡死／卡住 Docker）。output=${out}"
        return 1
    fi
    return 0
}

# 假 curl：寫可過 -v／version 的二進位，並記 URL。證明走下載，不是 docker pull。
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
    echo "gboard-node 0.0.0-issue18"
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
    cat >"\$output" <<'YML'
panel:
  url: https://panel.example.com
  token: TOKEN
  node_id: 1
YML
    [ -n "\$creds" ] && printf 'TOKEN=TOKEN\n' >"\$creds"
    [ -n "\$meta" ] && printf '{}\n' >"\$meta"
    echo "INSTANCE_ID=issue18-native"
    exit 0
fi
exit 0
FAKEBIN
chmod +x "\$out"
EOF
    chmod +x "$dest/curl"
}

# ---------------------------------------------------------------------------
# ATDD-1 Happy：無 Docker、不加 --docker，必須走到下載二進位＋寫設定
# ---------------------------------------------------------------------------

test_happy_native_install() {
    echo "=== ATDD-1 Happy: 無 Docker、不加 --docker，下載二進位＋寫設定 ==="

    if grep -Eiq '(^|[^A-Za-z_])(docker|docker-compose)([^A-Za-z_]|$)' "$INSTALL_SH"; then
        fail "install.sh 出現 docker／docker-compose（本機安裝不得硬依賴 Docker）"
    else
        pass "靜態：install.sh 無 docker／docker-compose（不加 --docker 就是原生）"
    fi

    if grep -Eq '^[[:space:]]*(ensure|require|check|wait)_?docker[[:space:]]*\(\)' "$INSTALL_SH"; then
        fail "install.sh 有 ensure／require／check_docker（硬依賴 Docker）"
    else
        pass "靜態：install.sh 無 docker 硬依賴函式"
    fi

    if ! grep -Eq '^[[:space:]]*(resolve_download_url|stage_binary|render_config)[[:space:]]*\(\)' "$INSTALL_SH"; then
        fail "install.sh 缺少 resolve_download_url／stage_binary／render_config（本機安裝＝下二進位＋寫設定）"
    else
        pass "靜態：install.sh 有下載二進位＋寫設定函式"
    fi

    local work minpath curl_log
    work="$(mktemp -d)"
    minpath="$(make_no_docker_path "$work/bin")"
    curl_log="$work/curl.urls"
    write_fake_curl "$minpath" "$curl_log"
    : >"$curl_log"

    if PATH="$minpath" command -v docker >/dev/null 2>&1; then
        fail "PATH 無 docker 夾具失敗"
        rm -rf "$work"
        return
    fi
    pass "夾具：PATH 無 docker"

    local empty="$work/cwd"
    mkdir -p "$empty"

    local run_out run_rc
    run_timeout run_out run_rc -- env PATH="$minpath" bash -c '
        set -euo pipefail
        cd "$2"
        # shellcheck disable=SC1090
        source "$1"
        trap - ERR EXIT
        parse_args --panel https://panel.example.com --token TOKEN --node-id 1
        validate_install_request
        detect_arch
        [ "$MODE" = "node" ]
        [ "$ACTION" = "install" ]
        [ -z "${DOCKER:-}" ]
        resolve_download_url "gboard-node-linux-${ARCH}"
        case "$DOWNLOAD_URL" in
            *gboard-node-linux-*) ;;
            *)
                echo "BAD_DOWNLOAD_URL=$DOWNLOAD_URL"
                exit 1
                ;;
        esac
        case "$DOWNLOAD_URL" in
            *docker*)
                echo "DOCKER_IN_DOWNLOAD_URL=$DOWNLOAD_URL"
                exit 1
                ;;
        esac
        echo "DOWNLOAD_URL=$DOWNLOAD_URL"
        TMP_DIR="$(mktemp -d)"
        stage_binary
        stage_gbctl
        render_config
        [ -x "$TMP_DIR/gboard-node" ]
        [ -x "$TMP_DIR/gbctl" ]
        [ -f "$TMP_DIR/config.yml" ]
        grep -q "panel.example.com" "$TMP_DIR/config.yml"
        echo NATIVE_INSTALL_OK
    ' _ "$INSTALL_SH" "$empty"

    if ! assert_not_hung "本機安裝（無 docker、不加 --docker）" "$run_rc" "$run_out"; then
        rm -rf "$work"
        return
    fi

    if [ "$run_rc" -ne 0 ]; then
        fail "本機安裝路徑失敗 rc=${run_rc}: ${run_out}"
        rm -rf "$work"
        return
    fi

    if ! echo "$run_out" | grep -q 'NATIVE_INSTALL_OK'; then
        fail "未走到下載二進位＋寫設定: ${run_out}"
        rm -rf "$work"
        return
    fi
    pass "不加 --docker：parse／validate 預設原生 install"

    if ! echo "$run_out" | grep -q 'DOWNLOAD_URL='; then
        fail "未解析下載 URL: ${run_out}"
    else
        pass "resolve_download_url 產出 Go 二進位 URL（非 docker）"
    fi

    if [ ! -s "$curl_log" ]; then
        fail "未呼叫 curl 下載二進位（本機安裝必須下載 Go 二進位）"
    elif grep -qiE 'docker|compose' "$curl_log"; then
        fail "curl 下載 URL 含 docker：$(cat "$curl_log")"
    else
        pass "curl 下載 Go 二進位（urls=$(tr '\n' ' ' <"$curl_log")）"
    fi

    if echo "$run_out" | grep -qiE 'please install docker|install docker|docker is required|請.*[Dd]ocker|必須.*[Dd]ocker'; then
        fail "本機安裝輸出要求／只提示裝 Docker: ${run_out}"
    else
        pass "輸出未要求／卡住 Docker"
    fi

    if echo "$run_out" | grep -q 'NATIVE_INSTALL_OK'; then
        pass "走到下載二進位＋寫設定（config.yml 含 panel）"
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------
# ATDD-2 邊界：文件必須寫明本機安裝（不加 --docker 或等價）
# ---------------------------------------------------------------------------

test_docs_native_install() {
    echo "=== ATDD-2 邊界: 文件必須寫明本機安裝（不加 --docker 或等價） ==="

    if ! grep -q 'install.sh' "$README"; then
        fail "README 未宣告 install.sh（官方叫用本機安裝）"
    else
        pass "README 宣告 install.sh"
    fi

    if ! grep -Eiq 'Installer|systemd|本機|本地' "$README"; then
        fail "README 未寫本機／systemd 安裝（只剩 Docker 不算本地安裝版本）"
    else
        pass "README 有 Installer／systemd／本機安裝段"
    fi

    local examples
    examples="$(grep -n 'install.sh' "$README" || true)"
    if [ -z "$examples" ]; then
        fail "README 沒有 install.sh 範例"
    elif echo "$examples" | grep -q -- '--docker'; then
        fail "README 的 install.sh 範例要求 --docker（官方：不加 --docker）: ${examples}"
    else
        pass "README install.sh 範例不加 --docker"
    fi

    local help_out help_rc
    run_timeout help_out help_rc -- bash "$INSTALL_SH" help
    if ! assert_not_hung "install.sh help" "$help_rc" "$help_out"; then
        return
    fi
    if [ "$help_rc" -ne 0 ]; then
        fail "install.sh help rc=${help_rc}: ${help_out}"
        return
    fi

    if ! echo "$help_out" | grep -Eq 'Gboard-Node Installer|--binary|Downloading|binary'; then
        fail "install.sh help 未說明本機／下載二進位: ${help_out}"
    else
        pass "install.sh help 說明本機安裝／下載二進位"
    fi

    if echo "$help_out" | grep -q -- '--docker'; then
        fail "install.sh help 把 --docker 當本機安裝必要旗標"
    else
        pass "install.sh help 不加 --docker（等價：預設就是下載二進位＋設定檔）"
    fi

    if echo "$help_out" | grep -qiE 'please install docker|docker is required|only support docker'; then
        fail "install.sh help 只提示裝 Docker"
    else
        pass "install.sh help 不是「請用 docker」"
    fi

    # 官方等價句：不加 --docker = 普通下载 go 二进制 + 配置文件
    if ! grep -Eq 'resolve_download_url|Downloading binary|DEFAULT_DOWNLOAD_BASE' "$INSTALL_SH"; then
        fail "install.sh 沒有下載 Go 二進位路徑"
    else
        pass "install.sh 本機路徑＝下載 Go 二進位"
    fi
}

# ---------------------------------------------------------------------------
# ATDD-3 失敗：硬依賴 docker／沒 docker 掛死／只提示裝 Docker → 必須紅
# ---------------------------------------------------------------------------

test_hard_docker_dep_must_fail() {
    echo "=== ATDD-3 失敗: 硬依賴 Docker／只提示裝 Docker 必須紅 ==="

    if grep -Eiq '(^|[^A-Za-z_])(docker|docker-compose)([^A-Za-z_]|$)' "$INSTALL_SH"; then
        fail "現 install.sh 硬依賴 docker／docker-compose（本機安裝版本不存在）"
    else
        pass "現 tip 無 docker 硬依賴（回歸鎖綠；若有人加上必須紅）"
    fi

    if grep -qiE 'please install docker|install docker first|docker is required' "$INSTALL_SH"; then
        fail "install.sh 只提示裝 Docker（官方叫用本機 install.sh 不加 --docker）"
    else
        pass "install.sh 不是只提示裝 Docker"
    fi

    # README 可以有 Docker 段，但不能沒有本機安裝
    if grep -Eiq '^###[[:space:]]*Docker' "$README" && ! grep -q 'install.sh' "$README"; then
        fail "README 只剩 Docker、沒有 install.sh 本機安裝"
    else
        pass "README 未把本機安裝收成只剩 Docker"
    fi

    local work minpath
    work="$(mktemp -d)"
    minpath="$(make_no_docker_path "$work/bin")"

    # 反例夾具：只提示裝 Docker（產品失敗樣態）。測必須能打紅。
    local only_docker="$work/only_docker_install.sh"
    cat >"$only_docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "Please install Docker. This installer only supports Docker." >&2
exit 1
EOF
    chmod +x "$only_docker"

    local o_out o_rc
    run_timeout o_out o_rc -- env PATH="$minpath" bash "$only_docker"
    if ! assert_not_hung "只提示裝 Docker 夾具" "$o_rc" "$o_out"; then
        rm -rf "$work"
        return
    fi
    if [ "$o_rc" -ne 0 ] && echo "$o_out" | grep -qi 'docker'; then
        pass "反例夾具：只提示裝 Docker 會被本測當產品失敗（測能打紅）"
    else
        fail "反例夾具應非 0 並印 docker，got rc=${o_rc} out=${o_out}"
    fi

    # 反例：硬依賴 docker-compose，沒有就掛
    local compose_dep="$work/compose_required.sh"
    cat >"$compose_dep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if ! command -v docker >/dev/null 2>&1 && ! command -v docker-compose >/dev/null 2>&1; then
    echo "[ERROR] docker / docker-compose is required for local install" >&2
    exit 1
fi
docker compose up -d
EOF
    chmod +x "$compose_dep"
    local c_out c_rc
    run_timeout c_out c_rc -- env PATH="$minpath" bash "$compose_dep"
    if ! assert_not_hung "硬依賴 compose 夾具" "$c_rc" "$c_out"; then
        rm -rf "$work"
        return
    fi
    if [ "$c_rc" -ne 0 ] && echo "$c_out" | grep -qi 'docker'; then
        pass "反例夾具：硬依賴 docker-compose 在無 docker 時失敗（測能打紅）"
    else
        fail "compose 硬依賴夾具應失敗，got rc=${c_rc} out=${c_out}"
    fi

    # 反例：沒 docker 就 read 空等（卡死）。用沒 writer 的 FIFO。
    local hang="$work/hang_ask_docker.sh"
    cat >"$hang" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if ! command -v docker >/dev/null 2>&1; then
    fifo="$(mktemp -u)"
    mkfifo "$fifo"
    read -r -p "Install docker now? [y/N]: " answer <"$fifo"
fi
EOF
    chmod +x "$hang"
    local h_out h_rc
    run_timeout h_out h_rc -- env PATH="$minpath" bash "$hang"
    if [ "$h_rc" -eq 124 ] || [ "$h_rc" -eq 137 ]; then
        pass "反例夾具：沒 docker 就掛死會被 timeout 抓到（測能打紅）"
    else
        fail "掛死夾具應被 timeout，got rc=${h_rc} out=${h_out}"
    fi

    # 契約：本機安裝必須是 curl 下二進位，不是 docker pull
    local contract="$work/native_contract.sh"
    cat >"$contract" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# 契約：不加 --docker = 下載 go 二進位＋寫設定，不准要求 docker。
if command -v docker >/dev/null 2>&1; then
    echo "docker present but must not be required" >&2
fi
echo "Downloading binary: https://example.invalid/gboard-node-linux-amd64"
printf 'panel:\n  url: https://panel.example.com\n' >"$1/config.yml"
echo NATIVE_CONTRACT_OK
EOF
    chmod +x "$contract"
    local n_out n_rc
    run_timeout n_out n_rc -- env PATH="$minpath" bash "$contract" "$work"
    if assert_not_hung "本機安裝契約（無 docker）" "$n_rc" "$n_out"; then
        if [ "$n_rc" -eq 0 ] && [ -f "$work/config.yml" ] && echo "$n_out" | grep -q NATIVE_CONTRACT_OK; then
            pass "契約夾具：無 docker 也能下二進位＋寫設定"
        else
            fail "本機安裝契約失敗 rc=${n_rc}: ${n_out}"
        fi
    fi

    rm -rf "$work"
}

# ---------------------------------------------------------------------------

case "$CASE" in
    happy) test_happy_native_install ;;
    docs) test_docs_native_install ;;
    harddep) test_hard_docker_dep_must_fail ;;
    all)
        test_happy_native_install
        test_docs_native_install
        test_hard_docker_dep_must_fail
        ;;
    *)
        die "unknown case: $CASE (happy|docs|harddep|all)"
        ;;
esac

echo
echo "issue18 summary: PASS=${PASS} FAIL=${FAIL}"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
exit 0
