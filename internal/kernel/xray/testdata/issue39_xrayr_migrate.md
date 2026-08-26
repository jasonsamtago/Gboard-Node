# 官方 cedar2025/Xboard-Node #39 — 未能完全迁移xrayr

官方票：https://github.com/cedar2025/Xboard-Node/issues/39  
標題：`未能完全迁移xrayr`  
內文要點：舊 XrayR 配置難遷；舉例
1. inbound Dokodemo-Door 轉發到父節點（port、address、network tcp,udp、timeout）
2. router geosite/geoip（domain geosite:youtube → outboundTag）

審核 2 已把 ATDD 寫死。本輪只交失敗測／回歸鎖，不准改 production。  
丟掉先前較寬的 sing-box tun／WARP／DNS 解鎖假設。

## 上下文（DDD）

- 限界上下文：**Xray 核**（inbound＋routing）
- 通用語言：
  - Dokodemo-Door＝`protocol=dokodemo-door`（含 `Dokodemo-Door`／`dokodemo`）
  - 產出 inbound 必須寫 `settings.address`／`settings.port`／`settings.network`／`settings.timeout`
  - 起核後配置埠在聽，連上會轉到父節點 address:port
  - geosite／geoip＝routing `domain: ["geosite:…"]` **原樣**進規則且能起核
  - geo 走已有官方 #20（`ensureGeoData`／`geodata.Ensure`），不准改下載路徑
  - 靜默 `buildInbound=nil`、當 unsupported 丟 inbound＝未完成
- 不在範圍：WARP／DNS 解鎖產品、整份 XrayR 匯入、sing-box tun 替代、改 #20 下載路徑、面板 UI、訂閱產出

## 以碼為準的 hypothesis（不要當已修去改 production）

- `Xray.Protocols()` 只有 vmess／vless／trojan／shadowsocks／hysteria，**沒有 dokodemo-door**
  - `service.validateNodeRuntime` 會 `protocol "dokodemo-door" is not supported by kernel "xray"`
- `buildInbound` switch 無 `case "dokodemo-door"`（也無 Dokodemo-Door／dokodemo），走 `default` **回 nil**
- `buildConfig` 見 nil inbound 就不寫 `inbounds`，只印 WARN「unsupported protocol, no inbound」
- `Start` 仍成功、`IsRunning=true`、配置埠沒在聽——核活著、任意門轉發不存在
- 缺 `settings.address`／`port` 同樣走「未知協議丟 inbound」，不會明示失敗
- routing：面板 `custom_routes` 官方形狀 `domain: ["geosite:youtube"]` 已原樣進 `routing.rules`（#20 自訂 geo 下載已合）→ Happy B 可當回歸鎖

## 本特性（FDD）

- 用戶能：把舊 XrayR 的任意門（Dokodemo-Door）＋`geosite:youtube` 規則用在 **Xray 節點**
- 不做：拆掉 geosite 當修；靜默 `buildInbound=nil`；當 unsupported 丟 inbound；改切 sing-box tun；做 WARP／DNS 解鎖；整包重做 XrayR

## 驗收（ATDD/BDD）

1. Happy A：Given 面板／NodeSpec `protocol=dokodemo-door`（含別名）且帶 `settings.address/port/network/timeout`；When 核 build＋Start；Then 產出 inbound（protocol=dokodemo-door＋上述 settings），配置埠在聽，連上會轉到 address:port
2. Happy B：Given routing／custom_routes `domain: ["geosite:youtube"]`；When 核 build＋Start；Then 該字串**原樣**留在 `routing.rules` 並指向指定 outbound，且能起核（geo 走 #20）
3. 邊界：既有 vmess／vless 不回歸；Linux／#20 geo 下載路徑不動
4. 失敗：缺 address／port 必須明示失敗，不准當 unsupported 丟 inbound、不准 Start 成功當空核

## 实现約束（TDD）

1. 本輪只加測試＋本規格（放 `internal/kernel/xray/`）
2. 現 tip（`dev` `bb7a3bd`）Dokodemo 未支援 → Happy A／缺欄位失敗鎖必須紅
3. Happy B 若 #20 已覆蓋 geosite 原樣進 routing＋起核 → 回歸鎖綠，並在本檔註明
4. 不准改 production、不准 skip、不准改切 sing-box、不准改 #20 下載路徑

## 本輪實測（dev bb7a3bd，不改 production）

- Happy A（Dokodemo-Door）：**紅**。`Protocols()` 無 dokodemo；`buildInbound` 回 nil；`Start` 成功但不聽埠；只 WARN unsupported。
- Happy B（`geosite:youtube`）：**回歸鎖綠**。官方形狀原樣進 `routing.rules` 並指向 `outboundTag`，#20 能起核。測仍會在拆掉 geosite／改 #20 路徑時紅。
- 邊界：vmess／vless **綠**；`TestCustomRouteGeo_`（#20）**綠**。
- 失敗（缺 address／port）：**紅**。當 unsupported 丟 inbound，Start 成功當空核。
- 詳見 `issue39_xrayr_migrate_note.txt`／`issue39_xrayr_migrate_go_test.txt`
- 建議：合測即可；審核2過方向後才寫 Dokodemo inbound。勿重做 #20、勿改切 sing-box。
