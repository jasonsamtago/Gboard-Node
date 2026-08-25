# 官方 cedar2025/Xboard-Node #31 — VMess+HTTP

官方票：https://github.com/cedar2025/Xboard-Node/issues/31  
標題：`vmess+http`  
內文：`什么时候可以支持这个vmess协议啊大佬`

審核 2：本輪只交失敗測／回歸鎖，不准改 production。

## 上下文（DDD）

- 限界上下文：Node 内核 inbound（sing-box 為主；xray 若接 VMess HTTP 一併鎖）
- 通用語言：
  - 「VMess+HTTP」＝面板節點 `protocol=vmess` 且傳輸 `network=http`（含 host／path）
  - 核必須**真帶** HTTP transport；客戶端依 HTTP 握手／連線
  - 靜默降成純 TCP、丟掉 host／path、Start 成功卻連不上 HTTP 客戶端＝未完成
  - `httpupgrade`／`h2` 是別種傳輸，不可冒充成這個 HTTP
- 不在範圍：面板 UI、訂閱產出、#16 Host 拆 inbound（已另鎖）、最少實現／改切只走 xray

## 以碼為準的 hypothesis（不要當已修）

- sing-box `buildVMess` → `applyTransport`
  - `network=http`／`h2` 都寫 `transport.type=http`，只抄扁平 `path`／`host`
  - `network=tcp` 才走 `applyTCPHTTPTransport`（官方 TCP+HTTP 模板的 nested header）
- xray `buildVMess` → `applyStreamSettings`
  - `case "h2", "http"` 把 `network` **改寫成 `h2`**，host 再包一層陣列
- 若面板下發 `network=http`＋host／path，測鎖的是：inbound 不得只剩 raw TCP；host／path 不得丟；HTTP 客戶端要通

## 本特性（FDD）

- 用戶能：面板選 VMess + HTTP 傳輸（host／path）後，核真帶 HTTP transport，客戶端可走 VMess+HTTP 握手
- 不做：把 HTTP 當不支援忽略；改成只支援純 TCP；用 httpupgrade／h2 冒充；改切只走 xray 當修

## 驗收（ATDD/BDD）

1. Happy：Given 面板 `protocol=vmess` 且 `network=http`（含 host／path）；When 核 build＋Start；Then inbound 真帶 HTTP transport（host／path 仍在），VMess+HTTP 客戶端能握手／回聲
2. 邊界：VMess + 無 HTTP（普通 TCP／預設）不得回歸
3. 失敗：宣稱 HTTP 但 host／path 被丟、或靜默降 TCP、或 Start 成功卻連不上 HTTP 客戶端 → 測必須紅
4. 再補邊界：空 host 仍須是 HTTP transport（不得降 TCP），path 仍在
5. 再補失敗：`httpupgrade`／`h2` 不可冒充成這個 HTTP（HTTP 客戶端打 httpupgrade inbound 必須連不上；`network=http` 產出不得是 httpupgrade）

## 实现約束（TDD）

1. 本輪只加失敗測＋本規格
2. 現 tip 已完整支援 → 測綠，標明回歸鎖（測仍會在丟 HTTP／降 TCP 時紅）
3. 未完整支援 → 必須紅；不准改 production、不准 skip、不准改切 xray、不准把 HTTP 當不支援忽略
