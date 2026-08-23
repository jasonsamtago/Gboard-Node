package xray

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
)

// Official cedar2025/Xboard-Node #16：不准把 Host 從 inbound 拆掉當修。
// xray streamSettings 的 ws／tcp+http 設了 Host 時，產出必須仍含 Host。

const (
	xrayHostValue = "cdn.example.com"
	xrayWSPath    = "/ws"
)

func TestHost_WS_StreamKeepsHost(t *testing.T) {
	nc := &panel.NodeConfig{
		Protocol:   "vless",
		ServerPort: 10086,
		Network:    "ws",
		NetworkSettings: map[string]interface{}{
			"path": xrayWSPath,
			"headers": map[string]interface{}{
				"Host": xrayHostValue,
			},
		},
	}
	inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
	raw := marshalStream(t, inbound)
	if !streamKeepsHost(inbound, xrayHostValue) {
		t.Fatalf("xray ws streamSettings 沒有 Host %q（把 Host 拆掉當修）:\n%s", xrayHostValue, raw)
	}
	t.Logf("xray ws stream Host 證據: %s", raw)
}

func TestHost_TCPHTTP_StreamKeepsHost(t *testing.T) {
	nc := &panel.NodeConfig{
		Protocol:   "vless",
		ServerPort: 10086,
		Network:    "tcp",
		NetworkSettings: map[string]interface{}{
			"header": map[string]interface{}{
				"type": "http",
				"request": map[string]interface{}{
					"path": []interface{}{"/"},
					"headers": map[string]interface{}{
						"Host": []interface{}{xrayHostValue},
					},
				},
			},
		},
	}
	inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
	raw := marshalStream(t, inbound)
	if !streamKeepsHost(inbound, xrayHostValue) {
		t.Fatalf("xray tcp+http streamSettings 沒有 Host %q（applyStreamSettings case tcp 沒寫 header）:\n%s",
			xrayHostValue, raw)
	}
	ss, _ := inbound["streamSettings"].(M)
	tcpSettings, _ := ss["tcpSettings"].(M)
	if tcpSettings == nil {
		t.Fatalf("xray tcp+http 產出沒有 tcpSettings，Host 無法掛在 inbound 上:\n%s", raw)
	}
	t.Logf("xray tcp+http stream Host 證據: %s", raw)
}

func TestHost_EmptySettings_StillBuilds(t *testing.T) {
	for _, network := range []string{"ws", "tcp"} {
		t.Run(network, func(t *testing.T) {
			settings := map[string]interface{}{}
			if network == "ws" {
				settings["path"] = xrayWSPath
			}
			nc := &panel.NodeConfig{
				Protocol:        "vless",
				ServerPort:      10086,
				Network:         network,
				NetworkSettings: settings,
			}
			inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
			if inbound == nil {
				t.Fatalf("Host 留空時 %s inbound 不該是 nil", network)
			}
			raw := marshalStream(t, inbound)
			if streamKeepsHost(inbound, xrayHostValue) {
				t.Fatalf("Host 留空不該寫入偽裝 Host: %s", raw)
			}
			t.Logf("xray %s 空 Host stream 證據: %s", network, raw)
		})
	}
}

func TestHost_ApplyStreamSettings_TCPHTTPMustNotDropHost(t *testing.T) {
	base := M{"streamSettings": M{}}
	nc := &model.NodeSpec{
		Network: "tcp",
		NetworkSettings: map[string]any{
			"header": map[string]any{
				"type": "http",
				"request": map[string]any{
					"headers": map[string]any{
						"Host": []any{xrayHostValue},
					},
				},
			},
		},
	}
	applyStreamSettings(base, nc, kernel.TLSCert{})
	raw, _ := json.Marshal(base)
	if !strings.Contains(string(raw), xrayHostValue) {
		t.Fatalf("applyStreamSettings(tcp+http) 產出沒有 Host %q（拆掉 Host 當修）: %s", xrayHostValue, raw)
	}
	ss, _ := base["streamSettings"].(M)
	if _, ok := ss["tcpSettings"].(M); !ok {
		t.Fatalf("applyStreamSettings(tcp+http) 沒有 tcpSettings: %s", raw)
	}
	t.Logf("applyStreamSettings tcp+http Host 證據: %s", raw)
}

func streamKeepsHost(inbound M, host string) bool {
	if inbound == nil {
		return false
	}
	ss, _ := inbound["streamSettings"].(M)
	if ss == nil {
		return false
	}
	if ws, ok := ss["wsSettings"].(M); ok {
		if headers, ok := ws["headers"].(M); ok && fmt.Sprint(headers["Host"]) == host {
			return true
		}
		if headers, ok := ws["headers"].(map[string]interface{}); ok && fmt.Sprint(headers["Host"]) == host {
			return true
		}
	}
	if tcp, ok := ss["tcpSettings"].(M); ok {
		raw, _ := json.Marshal(tcp)
		if strings.Contains(string(raw), host) {
			return true
		}
	}
	raw, _ := json.Marshal(ss)
	return strings.Contains(string(raw), host)
}

func marshalStream(t *testing.T, inbound M) string {
	t.Helper()
	data, err := json.MarshalIndent(inbound, "", "  ")
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return string(data)
}
