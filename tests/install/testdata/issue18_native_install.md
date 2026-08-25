# 官方 cedar2025/Xboard-Node #18 — 能不能出個本地安裝版本

官方票：https://github.com/cedar2025/Xboard-Node/issues/18  
標題：`能不能出個本地安裝版本`  
內文：`小雞節點性能差的裝Docker太臃腫了`  
官方回覆（yywudi）：`用install.sh啊，不加--docker就是普通的下载go二进制文件+配置文件`

審核 2：本輪只交失敗測／回歸鎖，不准改 production。本張鎖的是**本機／原生安裝**（不要強迫 Docker），不是重做 #12 的「無 docker 不得卡死」。

#12（已合 PR #31）鎖卡死；本張鎖產品：無 Docker 時仍能本機安裝（下 Go 二進位＋寫設定）。可引用 #12，不重做其測檔。

## 上下文（DDD）

- 限界上下文：Node 安裝器（`install.sh`／README 本機部署）
- 通用語言：
  - 「本地安裝版本」＝不加 `--docker`（或根本沒 docker 路徑）就下載 Go 二進位＋寫設定檔／systemd
  - Docker／Compose 是可選部署，不是本機安裝的硬依賴
  - 官方叫用：`install.sh` 不加 `--docker`
  - 硬依賴 Docker＝腳本／文件只走 docker／docker-compose，沒 docker 就掛死或只提示裝 Docker
- 不在範圍：重做 #12 卡死測、改 production、把預設改成「請用 docker」、Compose 說明改寫、面板心跳、FreeBSD

## 以碼為準的 hypothesis（不要當未修去改 production）

- 現 tip `install.sh` 是原生 binary＋systemd：`resolve_download_url`／`stage_binary`／`render_config`，全文沒有 docker，也沒有 `--docker` 旗標
- README「Installer (Linux systemd)」用 `install.sh`，不加 `--docker`
- #12 已鎖「無 docker 不掛」；本張要鎖「本機安裝這條產品路徑」
- 現 tip 若已完整支援 → **回歸鎖**：測必須仍會在「硬依賴 Docker／只提示裝 Docker／文件沒寫本機安裝」時失敗
- 不要為了紅去改 production

## 本特性（FDD）

- 用戶能：沒裝 Docker 的機器上跑文件宣告的本機安裝（`install.sh` 不加 `--docker`），走到下載二進位＋寫設定，不得要求／卡住 Docker
- 不做：強制 Docker；把預設改成只支援 Docker；重做 #12；改 production

## 驗收（ATDD/BDD）

1. Happy：Given PATH 無 docker、不加 `--docker`；When 走本機安裝路徑；Then 必須能下載 Go 二進位＋寫設定，不得要求／卡住 Docker
2. 邊界：README／`install.sh` 說明必須寫明本機安裝（不加 `--docker` 或等價：`install.sh`／systemd／下載二進位）
3. 失敗：腳本硬依賴 docker／docker-compose、沒 docker 就掛死或只提示裝 Docker → 測必須紅

## 实现約束（TDD）

1. 本輪只加測試＋本規格
2. 現 tip 已是原生二進位＋systemd → 測綠，標明回歸鎖（現況已綠、當回歸鎖；官方叫用 install.sh 不加 --docker）
3. 未完整支援 → 必須紅；不准改 production、不准 skip、不准改成「請用 docker」
4. 測仍會在硬依賴 Docker／只提示裝 Docker／文件沒寫本機安裝時紅
5. 不要重做 #12 的測檔

## 本輪實測（dev 6f1bf59，不改 production）

- bash `native_install_test.sh`：綠（回歸鎖）。PASS=23 FAIL=0
- `go test ./tests/install -run TestIssue18_`：綠。Happy／文件／硬依賴鎖／整包全 PASS
- 現況已綠、當回歸鎖；官方叫用 install.sh 不加 --docker
- 測仍會在硬依賴 Docker／只提示裝 Docker／文件沒寫本機安裝時紅
- 詳見 `issue18_native_install_go_test.txt`
- 建議：合測即可，不必再開本機安裝實作（#12 卡死鎖仍在）
