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

// Official cedar2025/Xboard-Node #2：不准把 acceptProxyProtocol 關掉／
// 忽略、改走非 TCP、拆掉 VMess 當修。面板預設 kernel 是 singbox。
// inbound 必須仍是 type=vmess、純 TCP（沒被改成 ws／grpc），且
// proxy_protocol 仍為 true。

func TestVMess_TCP_AcceptProxyProtocol_InboundStillVMessTCP(t *testing.T) {
	nc := &panel.NodeConfig{
		Protocol:   "vmess",
		ServerPort: 24022,
		Network:    "tcp",
		NetworkSettings: map[string]interface{}{
			"acceptProxyProtocol": true,
		},
	}
	spec := testNodeSpec(nc)
	if spec.Protocol != "vmess" || !strings.EqualFold(spec.Network, "tcp") {
		t.Fatalf("NodeSpec 必須仍是 VMess TCP，got protocol=%q network=%q", spec.Protocol, spec.Network)
	}
	if !spec.GetProxyProtocol() {
		t.Fatal("不准忽略面板 tcp acceptProxyProtocol=true")
	}

	inbound := buildInbound(spec, testUsers, kernel.TLSCert{})
	raw := marshalVMessPPInbound(t, inbound)
	if fmt.Sprint(inbound["type"]) != "vmess" {
		t.Fatalf("不准拆掉 VMess，inbound.type=%v\n%s", inbound["type"], raw)
	}
	if transport, ok := inbound["transport"]; ok && transport != nil {
		t.Fatalf("純 TCP 不准長出非 TCP transport（改走非 TCP 當修）: %v\n%s", transport, raw)
	}
	if !singboxInboundKeepsProxyProtocol(inbound) {
		t.Fatalf("singbox VMess TCP 產出沒有 proxy_protocol=true（applyProxyProtocol 關掉／忽略面板這欄）:\n%s", raw)
	}
	t.Logf("singbox VMess TCP+PP inbound 證據: %s", raw)
}

func TestVMess_TCP_NoProxyProtocol_DoesNotForcePP(t *testing.T) {
	nc := &panel.NodeConfig{
		Protocol:        "vmess",
		ServerPort:      24022,
		Network:         "tcp",
		NetworkSettings: map[string]interface{}{},
	}
	spec := testNodeSpec(nc)
	if spec.GetProxyProtocol() {
		t.Fatal("沒設 acceptProxyProtocol 不該被當成開 PP")
	}
	inbound := buildInbound(spec, testUsers, kernel.TLSCert{})
	raw := marshalVMessPPInbound(t, inbound)
	if fmt.Sprint(inbound["type"]) != "vmess" {
		t.Fatalf("回歸：無 PP 仍要是 VMess，got %v\n%s", inbound["type"], raw)
	}
	if singboxInboundKeepsProxyProtocol(inbound) {
		t.Fatalf("沒開 PP 不該寫入 proxy_protocol: %s", raw)
	}
}

func TestVMess_ApplyProxyProtocol_TCPMustKeepFlag(t *testing.T) {
	base := M{"type": "vmess"}
	nc := &model.NodeSpec{
		Protocol: "vmess",
		Network:  "tcp",
		NetworkSettings: map[string]any{
			"acceptProxyProtocol": true,
		},
	}
	applyProxyProtocol(base, nc)
	raw, _ := json.Marshal(base)
	if !singboxInboundKeepsProxyProtocol(base) {
		t.Fatalf("applyProxyProtocol(vmess+tcp) 產出沒有 proxy_protocol=true（函式被註解掉／忽略面板）: %s", raw)
	}
	if fmt.Sprint(base["type"]) != "vmess" {
		t.Fatalf("applyProxyProtocol 不准拆掉 VMess: %s", raw)
	}
	t.Logf("applyProxyProtocol tcp+PP 證據: %s", raw)
}

func singboxInboundKeepsProxyProtocol(inbound M) bool {
	if inbound == nil {
		return false
	}
	if v, ok := inbound["proxy_protocol"]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			return strings.EqualFold(b, "true")
		}
	}
	raw, _ := json.Marshal(inbound)
	return strings.Contains(string(raw), `"proxy_protocol":true`)
}

func marshalVMessPPInbound(t *testing.T, inbound M) string {
	t.Helper()
	data, err := json.MarshalIndent(inbound, "", "  ")
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return string(data)
}
