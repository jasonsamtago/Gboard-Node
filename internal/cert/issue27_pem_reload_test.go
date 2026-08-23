package cert

import (
	"context"
	"os"
	"path/filepath"
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
	firstPEM := first.TLSCert()

	// 程序重啟：新 Manager、同一 CertDir／同一 node。
	restart := newManagerForTest(cfg, issuer, logger)
	if err := restart.Start(ctx); err != nil {
		t.Fatalf("同 node 重啟 Start 不得失敗: %v\nlog=\n%s", err, zapBuf.String()+nlogBuf.String())
	}
	restartLog := zapBuf.String() + nlogBuf.String()
	if strings.Contains(restartLog, issue27PEMErr) {
		t.Fatalf("同 node 重啟不得再 %q:\n%s", issue27PEMErr, restartLog)
	}
	if !restart.HasCert() {
		t.Fatalf("同 node 重啟必須把 PEM 載回記憶體（官方 #27） log=\n%s", restartLog)
	}
	got := restart.TLSCert()
	if len(got.CertPEM) == 0 || len(got.KeyPEM) == 0 {
		t.Fatal("重啟後 TLSCert PEM 是空的，核會 tls config is nil")
	}
	if string(got.CertPEM) != string(firstPEM.CertPEM) && !restart.HasCert() {
		t.Fatal("重啟後必須有可用 PEM")
	}

	// 同 node 熱重載：記憶體 PEM 掉了（官方重啟／Reconfigure 路徑），
	// Obtain 已成功時仍必須載入，不准只回 acmeStarted=true。
	restart.mat.Store(nil)
	if restart.HasCert() {
		t.Fatal("precondition: 已清掉記憶體 PEM")
	}
	changed, err := restart.Reconfigure(ctx, cfg)
	_ = changed
	reloadLog := zapBuf.String() + nlogBuf.String()
	if err != nil {
		t.Fatalf("同 node Reconfigure 重載 PEM 不得失敗: %v\nlog=\n%s", err, reloadLog)
	}
	if strings.Contains(reloadLog, issue27PEMErr) {
		t.Fatalf("同 node 重載不得再 %q:\n%s", issue27PEMErr, reloadLog)
	}
	if !restart.HasCert() {
		t.Fatalf("同 node 重載必須再載入 PEM，不准 ACME obtained 卻沒載進記憶體 log=\n%s", reloadLog)
	}
}

func TestSameNodeSelfSignedRestartLoadsPersistedPEM(t *testing.T) {
	dir := t.TempDir()
	cfg := config.CertConfig{CertMode: "self", Domain: "node27.example.com", CertDir: dir}

	ctx := context.Background()
	first := NewManager(cfg)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("self Start: %v", err)
	}
	if !first.HasCert() {
		t.Fatal("self 簽完必須有 PEM")
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); err != nil {
		t.Fatalf("self 必須持久化 cert.pem: %v", err)
	}

	restart := NewManager(cfg)
	if err := restart.Start(ctx); err != nil {
		t.Fatalf("self 同 node 重啟: %v", err)
	}
	if !restart.HasCert() {
		t.Fatal("self 同 node 重啟必須載入已簽 PEM，不准當沒簽過")
	}
	if string(restart.TLSCert().CertPEM) != string(first.TLSCert().CertPEM) {
		t.Fatal("self 重啟應重用已簽 PEM，不是另簽一張讓舊連線全斷")
	}
}
