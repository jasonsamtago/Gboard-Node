package cert

import (
	"context"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #27 註解：面板配好 dns／http／self 後
// 當下能用，重啟同一 node 就
// `ACME obtained certificate but PEM was not loaded; check storage`，
// 核起不來（tls config is nil）。本票鎖「同 node 重載 PEM」，
// 與 #69（同機多節點重複簽）分開。

const issue27PEMErr = "ACME obtained certificate but PEM was not loaded"

func TestSameNodeACMERestartAndReloadLoadsPEM(t *testing.T) {
	dir := t.TempDir()
	cfg := config.CertConfig{
		CertMode: "http",
		Domain:   "node27.example.com",
		Email:    "ops@example.com",
		CertDir:  dir,
	}

	issuer := newCountingIssuer(t, "gboard-node-test-ca")
	logger, zapBuf := captureZap()
	var nlogBuf lockedBuf
	nlog.Init(&nlogBuf, 0, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := newManagerForTest(cfg, issuer, logger)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("第一次 ACME Start: %v", err)
	}
	if !first.HasCert() {
		t.Fatal("前置：剛簽完必須有 PEM")
	}

	// 程序重啟：新 Manager、同一 CertDir／同一 node。
	restart := newManagerForTest(cfg, issuer, logger)
	if err := restart.Start(ctx); err != nil {
		t.Fatalf("同 node 重啟 Start 不得失敗: %v\nlog=\n%s", err, zapBuf.String()+nlogBuf.String())
	}
	restartLog := zapBuf.String() + nlogBuf.String()
	if strings.Contains(restartLog, issue27PEMErr) {
		t.Fatalf("同 node 重啟不得再 %q:\n%s", issue27PEMErr, restartLog)
	}
	if !restart.HasCert() || !restart.TLSCert().HasCert() {
		t.Fatalf("同 node 重啟必須把 PEM 載回記憶體（官方 #27） log=\n%s", restartLog)
	}

	// 同 node 熱重載：記憶體 PEM 掉了（官方重啟／Reconfigure 路徑），
	// Obtain 已成功時仍必須載入，不准只回 acmeStarted=true。
	restart.mat.Store(nil)
	if restart.HasCert() {
		t.Fatal("precondition: 已清掉記憶體 PEM")
	}
	if _, err := restart.Reconfigure(ctx, cfg); err != nil {
		t.Fatalf("同 node Reconfigure 重載 PEM 不得失敗: %v\nlog=\n%s", err, zapBuf.String()+nlogBuf.String())
	}
	reloadLog := zapBuf.String() + nlogBuf.String()
	if strings.Contains(reloadLog, issue27PEMErr) {
		t.Fatalf("同 node 重載不得再 %q:\n%s", issue27PEMErr, reloadLog)
	}
	if !restart.HasCert() || !restart.TLSCert().HasCert() {
		t.Fatalf("同 node 重載必須再載入 PEM，不准 ACME obtained 卻沒載進記憶體 log=\n%s", reloadLog)
	}
}
