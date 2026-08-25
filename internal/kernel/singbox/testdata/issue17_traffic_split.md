# 官方 cedar2025/Xboard-Node #17 — 什么时候可以上分流

官方票：https://github.com/cedar2025/Xboard-Node/issues/17  
標題：`什么时候可以上分流`  
內文：空  
官方回覆（GCX-Monter）：Traffic splitting 已可配，去看 #14。  
https://github.com/cedar2025/Xboard-Node/issues/14

審核 2：本輪只交失敗測／回歸鎖，不准改 production。本張鎖的是**用戶能分流**，不是重做 #14 的 rule_set 形狀實作。

## 上下文（DDD）

- 限界上下文：Node 内核路由（sing-box；面板自訂路由／geosite／geoip／rule_set）
- 通用語言：
  - 「分流」＝面板自訂路由／geosite／geoip／rule_set 進 sing-box `route.rules`，流量依規則走對應 outbound
  - 核必須**真帶** `route.rules`（含 `rule_set`／geosite 等），不得靜默丟掉
  - 客戶端／探測必須能證明走對 outbound
  - 只印 WARN、rules 被丟、`rule_set` 不上＝未完成
  - 官方把「什麼時候可以上分流」指到 #14（sing-box `rule_set` geosite／geoip）；已合的 #14 是形狀／起核，本票鎖**用戶能分流**這件事
- 不在範圍：重開 #14 實作、#66 以外的新協議、改切 xray、面板 UI、訂閱產出、只靠本地 `custom_config` 檔（官方 #14 評論 workaround）當修

## 以碼為準的 hypothesis（不要當未修去改 production）

- `buildRoutes` 已吃 `CustomRouteRules`／`CustomRoutes`／kernel `custom_route`；#14 的 `splitCustomRouteRuleSet` 會把定義抬到 `route.rule_set`，rules 只留 tag
- 結構化 `custom_route_rules` 會編成 `domain`／`domain_suffix`＋`outbound`
- 官方 #17 空內文；官方回覆指 #14。現 tip（含 #14）若已完整支援 → **回歸鎖**：測必須仍會在「丟掉 rules／rule_set 不上／只 WARN」時失敗
- 不要為了紅去改 production

## 本特性（FDD）

- 用戶能：面板配分流規則（自訂路由／geosite／rule_set）後，核真帶 `route.rules`／`route.rule_set`，流量走對 outbound
- 不做：重做 #14；關自訂 route；改切 xray；只印 WARN；把分流當不支援忽略；順便做新協議

## 驗收（ATDD/BDD）

1. Happy：Given 面板 `custom_routes` 含官方 #14 wrapper（`rule_set` geosite／binary .srs＋rule 指到自訂 outbound）；When 核 build＋Start；Then sing-box config 有對應 `route.rules`／`route.rule_set`，客戶端／探測打中該 outbound
2. 邊界：無自訂路由的普通節點不回歸（能起核、沒有本票的 rule_set／分流 outbound）
3. 失敗：rules 被丟、`rule_set` 不上、只 WARN → 測必須紅
4. 再補邊界：結構化 `custom_route_rules`（無 rule_set）依 domain 走對 outbound，不得回歸
5. 再補失敗：兩條規則指兩個 outbound 時，走錯 outbound（A 打到 B、或未匹配也打到 A）→ 測必須紅

## 实现約束（TDD）

1. 本輪只加測試＋本規格
2. 現 tip 已完整支援 → 測綠，標明回歸鎖（現況已綠、當回歸鎖；官方指 #14）
3. 未完整支援 → 必須紅；不准改 production、不准 skip、不准改切 xray、不准重開 #14 實作
4. 測仍會在丟掉 rules／rule_set 不上／只 WARN 時紅

## 本輪實測（dev f37e453，不改 production）

- 待跑 `go test` 後回填（綠＝回歸鎖／官方指 #14；紅＝現況仍丟 rules 或走不到 outbound）
