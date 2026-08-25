# 官方 cedar2025/Xboard-Node #66 — SSH protocol for sing-box

官方票：https://github.com/cedar2025/Xboard-Node/issues/66  
標題：`SSH and new feaures support for sing-box`  
內文：`Is there any chance for adding SSH protocol for sing-box core?`

審核 2：本輪只交失敗測／回歸鎖，不准改 production。範圍只鎖 **SSH protocol**；票上「new features」其餘不開做。

## 上下文（DDD）

- 限界上下文：Node 内核 inbound（sing-box；面板節點 `protocol=ssh`）
- 通用語言：
  - 「SSH protocol for sing-box」＝面板節點 `protocol=ssh`（`kernel_type=singbox`）
  - 核必須**真帶** sing-box inbound `type=ssh`（或官方等價），配置埠在聽
  - 面板下發的 user／password（或 private key）必須進 inbound；SSH 客戶端能握手
  - 靜默當未知協議丟掉、Start 成功卻不聽埠、只印 WARN 不建 inbound＝未完成
  - 不得把 SSH 冒充成 shadowsocks／plugin，也不得改切只走 xray
- 不在範圍：面板 UI、訂閱產出、sing-box SSH outbound 當節點協議、票上其他 new features、最少實現

## 以碼為準的 hypothesis（不要當已修）

- `SingBox.Protocols()` 只有 vmess／vless／trojan／ss／hy2／tuic／naive／socks／http／anytls／mieru，**沒有 ssh**
  - `service.validateNodeRuntime` 會 `protocol "ssh" is not supported by kernel "singbox"`
- `buildInbound` switch 無 `case "ssh"`，走 `default` **回 nil**（與 `unknown-proto` 同一條）
- `buildConfig` 見 nil inbound 就不寫 `inbounds`，連 WARN 都沒有（比 xray 的 unsupported WARN 更靜默）
- `Start` 仍成功、`IsRunning=true`、配置埠沒在聽——官方「核活著、客戶端連不上」
- 現況沒有把 SSH 改寫成 shadowsocks 的路徑；測仍鎖不得走 SS／plugin ignoring

## 本特性（FDD）

- 用戶能：面板選 SSH（sing-box）並帶 user／認證後，核真起 SSH inbound，客戶端可 SSH 握手／連線
- 不做：當不支援忽略；Start 成功但不聽；只 WARN；改成只支援 VMess／SS／VLESS；把 SSH 當 shadowsocks plugin；改切 xray；順便做票上其他 new features

## 驗收（ATDD/BDD）

1. Happy：Given 面板 `protocol=ssh`（sing-box）且有 user／password（或 private key）；When 核 build＋Start；Then inbound **type=ssh**（或官方等價），埠在聽，SSH 客戶端能握手
2. 邊界：既有 VMess／Shadowsocks／VLESS 不得回歸
3. 失敗：未知／忽略 SSH、Start 成功但不聽、或只印 WARN 不建 inbound → 測必須紅
4. 再補邊界：帶 `server_key`（private key）仍須是 SSH inbound，不得丟掉或改寫成 shadowsocks
5. 再補失敗：缺認證（無 user／password／key）必須明確錯誤，不得 Start 成功當空核

## 实现約束（TDD）

1. 本輪只加失敗測＋本規格
2. 現 tip 已完整支援 → 測綠，標明回歸鎖（測仍會在忽略 SSH／不聽埠時紅）
3. 未完整支援 → 必須紅；不准改 production、不准 skip、不准改切 xray、不准只改 WARN
