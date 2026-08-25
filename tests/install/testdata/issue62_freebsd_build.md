# 官方 cedar2025/Xboard-Node #62 — 麻煩增加下 FreeBSD 編譯文件

官方票：https://github.com/cedar2025/Xboard-Node/issues/62  
標題：`[bug]麻烦增加下FreeBSD编译文件`  
內文：
1. freebsd 裝不了（有截圖）
2. 最好能配合 xboard 的安裝命令自適應安裝

審核 2 已把 ATDD 寫死。本輪只交失敗測，不准改 production。測必須對齊這份，不要自作主張擴大。

## 上下文（DDD）

- 限界上下文：發布／安裝（`Makefile`＋`install.sh`）
- 通用語言：
  - 「編譯檔」＝`GOOS=freebsd` 正式目標：`make build-freebsd` 產出 `gboard-node-freebsd-${ARCH}`
  - 「自適應」＝同一條 `curl | bash` 依 `uname` 選 freebsd 成品，並寫 **rc.d**（不是 systemd）
  - Linux 路徑＝仍走 systemd＋`*-linux-*`
  - 「當 linux」＝未知 OS 或缺 freebsd 成品時，靜默改拿 `gboard-node-linux-${ARCH}`
  - 「明示失敗」＝未知 OS／缺 freebsd 成品必須非 0 並說明原因，不准當 linux
- 不在範圍：ports／pkg、OpenBSD／macOS、核心移植、擴大發行面（CI 矩陣／多 OS 發布）、改 Linux 路徑、本輪改 production

## 本特性（FDD）

- 用戶能：在 FreeBSD 用同一安裝命令裝上可跑的 node（拿 `gboard-node-freebsd-${ARCH}`＋寫 rc.d）
- 不做：改 Linux 路徑；靜默拿 linux 二進位；擴大發行面；ports／pkg；OpenBSD／macOS；核心移植

## 驗收（ATDD/BDD）

1. Happy：Given `make build-freebsd`；When 正式交叉編；Then 產出 `gboard-node-freebsd-${ARCH}`。Given `uname=FreeBSD`；When 跑同一條 `install.sh`；Then 拿該成品並寫 rc.d（不是 systemd）
2. 邊界：Given `uname=Linux`；When 安裝；Then 仍走 systemd＋`*-linux-*`
3. 失敗：Given 未知 OS，或缺 freebsd 成品；When 安裝；Then **明示失敗**，不准當 linux

## 以碼為準的 hypothesis（不要當已修去改 production）

- 現 tip `d15d962` `Makefile` 只有 `build-linux`／`build-linux-arm64`，**沒有 `build-freebsd`**
- 現 tip `install.sh` 硬綁 systemd（`SERVICE_PATH=/etc/systemd/system/...`、`ensure_systemd`、`write_service_unit`）
- 現 tip `stage_binary` 硬編碼 `gboard-node-linux-${ARCH}`；`detect_os` 不讀 `uname -s`
- Happy **應紅**。不要為了紅去改 production。現 tip 若已齊 → 回歸鎖綠

## 实现約束（TDD）

1. 本輪只加測試＋本規格＋testdata fixture
2. 斷言只鎖：`make build-freebsd`、`gboard-node-freebsd-${ARCH}`、FreeBSD → 該成品＋rc.d、Linux → systemd＋`*-linux-*`、未知 OS／缺成品 → 明示失敗
3. 不准改 production、不准 skip
4. 不准擴大到 CI 發行、ports／pkg、OpenBSD／macOS、核心移植、改 Linux 路徑

## 本輪實測（dev d15d962，不改 production）

見 `issue62_freebsd_build_note.txt`、`issue62_freebsd_build_go_test.txt`。
