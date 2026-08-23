package singbox

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
// 面板預設 kernel 是 singbox。ws／tcp+http 設了 Host 時，產出的 inbound
// 必須仍含 Host。

const (
	singboxHostValue = "cdn.example.com"
	singboxWSPath    = "/ws"
)

func TestHost_WS_InboundKeepsHost(t *testing.T) {
	nc := &panel.NodeConfig{
		Protocol:   "vless",
		ServerPort: 10086,
		Network:    "ws",
		NetworkSettings: map[string]interface{}{
			"path": singboxWSPath,
			"headers": map[string]interface{}{
				"Host": singboxHostValue,
			},
		},
	}
	inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
	raw := marshalInbound(t, inbound)
	if !inboundKeepsHost(inbound, singboxHostValue) {
		t.Fatalf("singbox ws 產出 inbound 沒有 Host %q（把 Host 拆掉當修）:\n%s", singboxHostValue, raw)
	}
	t.Logf("singbox ws inbound Host 證據: %s", raw)
}

func TestHost_TCPHTTP_InboundKeepsHost(t *testing.T) {
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
						"Host": []interface{}{singboxHostValue},
					},
				},
			},
		},
	}
	inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
	raw := marshalInbound(t, inbound)
	if !inboundKeepsHost(inbound, singboxHostValue) {
		t.Fatalf("singbox tcp+http 產出 inbound 沒有 Host %q（applyTransport 對 tcp 直接 return，Host 沒進 inbound）:\n%s",
			singboxHostValue, raw)
	}
	transport, _ := inbound["transport"].(M)
	if transport == nil {
		t.Fatalf("singbox tcp+http 產出沒有 transport，Host 無法掛在 inbound 上:\n%s", raw)
	}
	t.Logf("singbox tcp+http inbound Host 證據: %s", raw)
}

func TestHost_EmptySettings_StillBuilds(t *testing.T) {
	for _, network := range []string{"ws", "tcp"} {
		t.Run(network, func(t *testing.T) {
			settings := map[string]interface{}{}
			if network == "ws" {
				settings["path"] = singboxWSPath
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
			raw := marshalInbound(t, inbound)
			if inboundKeepsHost(inbound, singboxHostValue) {
				t.Fatalf("Host 留空不該寫入偽裝 Host: %s", raw)
			}
			t.Logf("singbox %s 空 Host inbound 證據: %s", network, raw)
		})
	}
}

func inboundKeepsHost(inbound M, host string) bool {
	if inbound == nil {
		return false
	}
	if transport, ok := inbound["transport"].(M); ok {
		if headers, ok := transport["headers"].(map[string]interface{}); ok {
			if fmt.Sprint(headers["Host"]) == host {
				return true
			}
		}
		if headers, ok := transport["headers"].(M); ok {
			if fmt.Sprint(headers["Host"]) == host {
				return true
			}
		}
		if fmt.Sprint(transport["host"]) == host {
			return true
		}
	}
	raw, _ := json.Marshal(inbound)
	return strings.Contains(string(raw), `"Host":"`+host+`"`) ||
		strings.Contains(string(raw), `"host":"`+host+`"`) ||
		strings.Contains(string(raw), `"`+host+`"`)
}

func marshalInbound(t *testing.T, inbound M) string {
	t.Helper()
	data, err := json.MarshalIndent(inbound, "", "  ")
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return string(data)
}

func TestHost_ApplyTransport_TCPHTTPMustNotDropHost(t *testing.T) {
	base := M{}
	nc := &model.NodeSpec{
		Network: "tcp",
		NetworkSettings: map[string]any{
			"header": map[string]any{
				"type": "http",
				"request": map[string]any{
					"headers": map[string]any{
						"Host": []any{singboxHostValue},
					},
				},
			},
		},
	}
	applyTransport(base, nc)
	raw, _ := json.Marshal(base)
	if !strings.Contains(string(raw), singboxHostValue) {
		t.Fatalf("applyTransport(tcp+http) 產出沒有 Host %q（拆掉 Host 當修）: %s", singboxHostValue, raw)
	}
	t.Logf("applyTransport tcp+http Host 證據: %s", raw)
}
