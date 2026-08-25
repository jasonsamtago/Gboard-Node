# 官方 cedar2025/Xboard-Node #62 — 麻煩增加下 FreeBSD 編譯文件

官方票：https://github.com/cedar2025/Xboard-Node/issues/62  
標題：`[bug]麻烦增加下FreeBSD编译文件`  
內文：
1. freebsd 裝不了（有截圖）
2. 最好能配合 xboard 的安裝命令自適應安裝

審核 2：本輪只交失敗測／回歸鎖，不准改 production。範圍只鎖 **編譯產物＋install.sh 自適應**。不准擴大到真機跑 FreeBSD 服務、改 kernel、整包移植 jail。

## 上下文（DDD）

- 限界上下文：Node 發布產物（Makefile／CI 正式交叉編）＋安裝器（`install.sh` 下載 URL）
- 通用語言：
  - 「FreeBSD 編譯文件」＝正式 `GOOS=freebsd` build 目標（Makefile 如 `build-freebsd`／`GOOS=freebsd`，或 CI matrix `goos: freebsd`），產出 `gboard-node-freebsd-*`／`gbctl-freebsd-*`
  - 「安裝命令自適應」＝`install.sh` 依 `uname -s`（或等價）解析對應 GOOS 的二進位 URL；在 FreeBSD 必須是 freebsd 包，不得硬編碼 linux
  - linux／amd64 本機安裝（官方 #18）是既有產品路徑，本票不得回歸
  - 「仍下 linux 包」＝`uname=FreeBSD` 時 `stage_binary`／`stage_gbctl` 仍走 `*-linux-${ARCH}`
  - 「直接拒絕／掛死」＝偵測到 FreeBSD 就 `exit 1`／Unsupported，或空等不退出
- 不在範圍：真機跑 FreeBSD 服務（rc.d／jail）、改 kernel、整包移植、重做 #18、為了紅去改 production、強迫 Docker

## 以碼為準的 hypothesis（不要當已修去改 production）

- 現 tip `Makefile`：只有 `build`／`build-linux`／`build-linux-arm64`；`build-all`＝後兩者。**沒有 `GOOS=freebsd`／`build-freebsd`**
- 現 tip CI `.github/workflows/ci.yml`：matrix 只有 `linux/amd64`、`linux/arm64`；Release `files:` 只有 linux 四個 artifact。**沒有 freebsd artifact**
- 現 tip `install.sh`：
  - `detect_arch` 只讀 `uname -m`（架構）
  - `detect_os` 只讀 `/etc/os-release` 的 Linux distro `ID`，不讀 `uname -s`
  - `stage_binary` 硬編碼 `resolve_download_url "gboard-node-linux-${ARCH}"`
  - `stage_gbctl` 硬編碼 `resolve_download_url "gbctl-linux-${ARCH}"`
  - `select_binary_source` 本機檔名也只認 `*-linux-${ARCH}`
- README「Installer (Linux systemd)」沒有 FreeBSD 編譯／自適應說明（本票不鎖文件改寫）
- Happy：正式 freebsd 目標＋FreeBSD 必須解析到 freebsd URL → 現況 **必須紅**
- 邊界：linux／amd64 本機安裝（#18）現況應綠，不得回歸
- 失敗：uname=FreeBSD 仍下 linux 包、或腳本直接拒絕／掛死 → **必須紅**
- 現 tip 若已有 freebsd 目標與自適應 → **回歸鎖綠**。不要為了紅去改 production

## 本特性（FDD）

- 用戶能：用正式 `GOOS=freebsd` 目標編出 freebsd 二進位；在 FreeBSD（或 `uname=FreeBSD`）跑與 xboard 相同的安裝命令時，`install.sh` 自適應下載 freebsd 包
- 不做：真機跑 FreeBSD 服務；改 kernel／jail；重做 #18；本輪改 production

## 驗收（ATDD/BDD）

1. Happy：Given 正式發布產物；When 讀 Makefile／CI；Then 必須有 `GOOS=freebsd` 正式 build 目標（Makefile 或 CI）。Given `uname=FreeBSD`（或模擬）；When `install.sh` 解析下載 URL；Then 必須是 freebsd 二進位 URL，不得硬編碼 linux
2. 邊界：linux／amd64 本機安裝不回歸（官方 #18：無 Docker 仍下 linux 包＋寫設定；Makefile 仍有 `GOOS=linux`）
3. 失敗：uname=FreeBSD 仍下 linux 包、或腳本直接拒絕／掛死 → 測必須紅

## 实现約束（TDD）

1. 本輪只加測試＋本規格＋testdata fixture
2. 現 tip 已有 freebsd 目標與自適應 → 測綠，標明回歸鎖（測仍會在「只編 linux／FreeBSD 仍下 linux／拒絕／掛死」時紅）
3. 未完整支援（現況缺 freebsd 目標、URL 硬編碼 linux）→ 必須紅；不准改 production、不准 skip、不准擴大到 jail／kernel
4. 測仍會在缺 `GOOS=freebsd`、FreeBSD 仍下 linux、拒絕／掛死時紅
5. 不要重做 #18 的測檔；邊界可呼叫既有 #18 Happy
6. 不鎖真機 systemd／rc.d 起服務（那是範圍外）

## 本輪實測（dev d15d962，不改 production）

見 `issue62_freebsd_build_note.txt`、`issue62_freebsd_build_go_test.txt`。
