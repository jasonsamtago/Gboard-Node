package ssinbound

import (
	"fmt"
	"path/filepath"
	"strings"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/sip003"
	"github.com/sagernet/sing/common/json/badoption"
)

// TransportFromPlugin 把面板 SIP003 plugin／opts 轉成 sing-box 官方
// V2Ray websocket transport。v2ray-plugin／gost-plugin 的 ws／websocket
// 模式對應 inbound.transport；未知 plugin 回明確錯誤，不准 ignoring。
func TransportFromPlugin(plugin, pluginOpts string) (*option.V2RayTransportOptions, error) {
	ws, err := parseWSPlugin(plugin, pluginOpts)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, nil
	}
	headers := make(badoption.HTTPHeader)
	if ws.Host != "" {
		headers["Host"] = []string{ws.Host}
	}
	return &option.V2RayTransportOptions{
		Type: C.V2RayTransportTypeWebsocket,
		WebsocketOptions: option.V2RayWebsocketOptions{
			Path:    ws.Path,
			Headers: headers,
		},
	}, nil
}

// TransportMap 給 buildShadowsocks 寫進 inbound JSON（type=ws＋path＋host）。
func TransportMap(plugin, pluginOpts string) (map[string]any, error) {
	ws, err := parseWSPlugin(plugin, pluginOpts)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, nil
	}
	transport := map[string]any{
		"type": C.V2RayTransportTypeWebsocket,
		"path": ws.Path,
	}
	if ws.Host != "" {
		transport["headers"] = map[string]any{
			"Host": []string{ws.Host},
		}
	}
	return transport, nil
}

// Validate 在起核前擋未知／不支援的 SS plugin。空 plugin 放行。
func Validate(protocol, plugin, pluginOpts string) error {
	if !strings.EqualFold(strings.TrimSpace(protocol), "shadowsocks") {
		return nil
	}
	if strings.TrimSpace(plugin) == "" {
		return nil
	}
	_, err := parseWSPlugin(plugin, pluginOpts)
	return err
}

type wsPlugin struct {
	Host string
	Path string
}

func parseWSPlugin(plugin, pluginOpts string) (*wsPlugin, error) {
	name := canonicalPluginName(plugin)
	if name == "" {
		return nil, nil
	}
	if !supportedSSPlugin(name) {
		return nil, fmt.Errorf("shadowsocks inbound unsupported plugin %s", plugin)
	}

	args, err := sip003.ParsePluginOptions(pluginOpts)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks inbound plugin %s: parse plugin_opts: %w", plugin, err)
	}

	mode := "websocket"
	if modeOpt, ok := args.Get("mode"); ok && strings.TrimSpace(modeOpt) != "" {
		mode = strings.ToLower(strings.TrimSpace(modeOpt))
	}
	switch mode {
	case "websocket", "ws":
	default:
		return nil, fmt.Errorf("shadowsocks inbound plugin %s: unsupported mode %s", plugin, mode)
	}

	host, _ := args.Get("host")
	path, _ := args.Get("path")
	if strings.TrimSpace(path) == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &wsPlugin{
		Host: strings.TrimSpace(host),
		Path: path,
	}, nil
}

func canonicalPluginName(plugin string) string {
	plugin = strings.TrimSpace(plugin)
	if plugin == "" {
		return ""
	}
	plugin = filepath.Base(strings.ReplaceAll(plugin, "\\", "/"))
	return strings.ToLower(plugin)
}

func supportedSSPlugin(name string) bool {
	switch name {
	case "v2ray-plugin", "v2ray_plugin", "gost-plugin", "gost_plugin":
		return true
	default:
		return false
	}
}
