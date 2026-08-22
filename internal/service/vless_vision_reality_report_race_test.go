//go:build race

package service

import "testing"

func skipVisionSpliceUnderRace(t *testing.T) {
	t.Helper()
	t.Skip("xray VLESS vision 真實連線必然踩 checkptr（proxy/vless/outbound.(*Handler).Process unsafe 指標運算）。-race 下改 skip，不准關整個 -race。dispatcher SizeStatWriter 測試仍在 -race 跑。")
}
