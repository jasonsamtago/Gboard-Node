# 官方 cedar2025/Xboard-Node #48 — Strip Go binary symbols to reduce image size by ~30%

官方票：https://github.com/cedar2025/Xboard-Node/issues/48  
標題：`Strip Go binary symbols to reduce image size by ~30%`  
內文要點：Docker 映像 binary layer ~66.8MB 帶 DWARF／符號表；應 `go build -trimpath -ldflags="-s -w"`。`-s` 丟 symbol table、`-w` 丟 DWARF、`-trimpath` 去本機路徑。預期映像約小 30%。不影響 panic 函式名、pprof、journal。

審核 2：本輪只交失敗測／回歸鎖，不准改 production。範圍只鎖 **正式映像／發布二進位的 strip**，不准擴大到壓縮映像基底、改 alpine、多階段重寫整份 Dockerfile。

## 上下文（DDD）

- 限界上下文：Node 發布產物（正式 Docker 映像／Makefile 發布二進位）
- 通用語言：
  - 「strip」＝正式 `go build` 帶 `-ldflags` 的 `-s`（丟 symbol table）與 `-w`（丟 DWARF）
  - 「-trimpath」＝去掉本機路徑（或等價 `GOFLAGS=-trimpath`）
  - 正式產物＝Makefile 預設 `build`、`build-linux`／`build-linux-arm64`（CI 上傳／Release）、Dockerfile builder 的 `go build`（`build-docker` 產物）
  - 開發產物＝`go test`／未 strip 的本機 debug `go build`，本票不強迫 strip
- 不在範圍：壓 alpine／換基底、多階段重寫整份 Dockerfile、改 test binary、改 panic／pprof／journal 行為、為了紅去改 production

## 以碼為準的 hypothesis（不要當已修去改 production）

- 現 tip `Makefile`：`LDFLAGS := -s -w -X ...`，`build`／`build-linux`／`build-linux-arm64` 用 `-ldflags "$(LDFLAGS)"`，**沒有 `-trimpath`**
- 現 tip `Dockerfile`：`go build -ldflags "-s -w ..."`，**沒有 `-trimpath`**
- CI `build` job 呼叫 `make build-linux`／`make build-linux-arm64`；`build-docker` 用 Dockerfile。workflow 本身沒有另一條 raw `go build`
- Happy 必須含 `-s`、`-w` **以及** `-trimpath`（或等價）→ 現況 **缺 -trimpath，測必須紅**
- `-s -w` 已在正式 Makefile／Dockerfile → 失敗鎖對現 tip 綠；**回歸鎖**：testdata fixture 拿掉 `-s -w` 時測必須仍紅。不要為了紅去改 production

## 本特性（FDD）

- 用戶能：拉正式映像／發布二進位時，binary 已 strip 符號表與 DWARF，並 trimpath，映像／層較小
- 不做：強迫 `go test`／本機 debug build 也 strip；改 alpine／壓基底；重寫多階段；改 production（本輪）

## 驗收（ATDD/BDD）

1. Happy：Given 正式 build（Makefile 預設／CI Docker 產物指令）；When 讀產物指令；Then 必須含 `-ldflags` 的 `-s` 與 `-w`，以及 `-trimpath`（或等價）
2. 邊界：開發用 `go test`／未 strip 的本機 debug build 不回歸（測不要強迫 test binary 也 strip）
3. 失敗：正式 Dockerfile／workflow／Makefile `build` 漏 `-s`／`-w` → 必須紅

## 实现約束（TDD）

1. 本輪只加測試＋本規格
2. 現 tip 已全部帶上 → 測綠，標明回歸鎖（測仍會在拿掉 `-s -w` 時紅）
3. 未完整支援（現況缺 `-trimpath`）→ 必須紅；不准改 production、不准 skip、不准擴大到壓基底
4. 測仍會在正式產物漏 `-s`／`-w`／`-trimpath` 時紅

## 本輪實測（dev c95a5da，不改 production）

- Happy：紅（失敗測成立）。Makefile `build`／`build-linux`／`build-linux-arm64` 與 Dockerfile 的正式 `go build` 都缺 `-trimpath`
- 邊界：綠。`make test`／debug fixture 未 strip，測不強迫 test binary strip
- 失敗／回歸鎖：綠。現 tip 已有 `-s -w`；fixture 拿掉 `-s -w` 仍紅（testdata 寫明）
- 詳見 `issue48_strip_symbols_go_test.txt`
- 不准改 production；方向過再補 `-trimpath`（不要為了紅去改 production）

## 實作輪（方向過後只補 -trimpath）

基底：`cursor/strip-go-binary-symbols-de05` tip `df3c188`（base `dev` `c95a5da`）

- 生產檔只改 `Makefile`／`Dockerfile`：正式 `go build` 加 `-trimpath`，與既有 `-ldflags "-s -w"` 並存
- 不改 alpine、不重寫多階段、不改 `test`／debug build、不改 CI workflow（workflow 只呼叫 make／Dockerfile）
- `go test ./tests/install -count=1 -timeout 30s -run 'TestIssue48_'`：**全綠**（Happy／邊界／-s -w 回歸鎖）
- 正式 `make build-linux`：展開指令含 `-trimpath -ldflags "-s -w ..."`；`go version -m` 見 `-trimpath=true`；`file` 標 stripped；對照未 strip 約小 28–31%
- 詳見 `issue48_strip_symbols_impl_go_test.txt`、`issue48_formal_build_evidence.txt`
