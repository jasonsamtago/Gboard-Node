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

// Official cedar2025/Xboard-Node #2：不准把 acceptProxyProtocol 關掉／
// 忽略、改走非 TCP、拆掉 VMess 當修。xray inbound／stream 必須仍是
// VMess + TCP，且面板 tcp {"acceptProxyProtocol": true} 仍生效。

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
	if fmt.Sprint(inbound["protocol"]) != "vmess" {
		t.Fatalf("不准拆掉 VMess，inbound.protocol=%v\n%s", inbound["protocol"], raw)
	}
	ss, _ := inbound["streamSettings"].(M)
	if ss == nil {
		t.Fatalf("VMess TCP 產出沒有 streamSettings:\n%s", raw)
	}
	network := fmt.Sprint(ss["network"])
	if network != "tcp" && network != "" {
		t.Fatalf("不准改走非 TCP，streamSettings.network=%q\n%s", network, raw)
	}
	if !xrayInboundKeepsProxyProtocol(inbound) {
		t.Fatalf("xray VMess TCP 產出沒有 acceptProxyProtocol=true（關掉／忽略面板這欄當修）:\n%s", raw)
	}
	t.Logf("xray VMess TCP+PP inbound 證據: %s", raw)
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
	if fmt.Sprint(inbound["protocol"]) != "vmess" {
		t.Fatalf("回歸：無 PP 仍要是 VMess，got %v\n%s", inbound["protocol"], raw)
	}
	if xrayInboundKeepsProxyProtocol(inbound) {
		t.Fatalf("沒開 PP 不該寫入 acceptProxyProtocol: %s", raw)
	}
}

func TestVMess_ApplyStreamSettings_TCPMustKeepAcceptProxyProtocol(t *testing.T) {
	base := M{"streamSettings": M{"sockopt": M{"reusePort": true}}}
	nc := &model.NodeSpec{
		Protocol: "vmess",
		Network:  "tcp",
		NetworkSettings: map[string]any{
			"acceptProxyProtocol": true,
		},
	}
	applyStreamSettings(base, nc, kernel.TLSCert{})
	raw, _ := json.Marshal(base)
	if !strings.Contains(string(raw), `"acceptProxyProtocol":true`) {
		t.Fatalf("applyStreamSettings(tcp+PP) 產出沒有 acceptProxyProtocol=true: %s", raw)
	}
	ss, _ := base["streamSettings"].(M)
	if ss == nil {
		t.Fatalf("applyStreamSettings 弄丟 streamSettings: %s", raw)
	}
	network := fmt.Sprint(ss["network"])
	if network != "tcp" && network != "" {
		t.Fatalf("applyStreamSettings 不准改走非 TCP: %s", raw)
	}
	t.Logf("applyStreamSettings tcp+PP 證據: %s", raw)
}

func xrayInboundKeepsProxyProtocol(inbound M) bool {
	if inbound == nil {
		return false
	}
	raw, _ := json.Marshal(inbound)
	return strings.Contains(string(raw), `"acceptProxyProtocol":true`)
}

func marshalVMessPPInbound(t *testing.T, inbound M) string {
	t.Helper()
	data, err := json.MarshalIndent(inbound, "", "  ")
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return string(data)
}
