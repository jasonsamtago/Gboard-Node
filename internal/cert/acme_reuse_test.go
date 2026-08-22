package cert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// countingIssuer is a local test CA / stub. It signs CSRs like an ACME
// issuer would, without contacting Let's Encrypt. Issue() is the obtain.
type countingIssuer struct {
	key   string
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	mu      sync.Mutex
	issues  int
	domains []string
}

func newCountingIssuer(t *testing.T, issuerKey string) *countingIssuer {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gboard-node test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &countingIssuer{key: issuerKey, ca: ca, caKey: caKey}
}

func (c *countingIssuer) IssuerKey() string { return c.key }

func (c *countingIssuer) Issue(_ context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues++
	names := append([]string{}, csr.DNSNames...)
	if cn := strings.TrimSpace(csr.Subject.CommonName); cn != "" {
		names = append(names, cn)
	}
	c.domains = append(c.domains, names...)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, c.ca, csr.PublicKey, c.caKey)
	if err != nil {
		return nil, err
	}
	return &certmagic.IssuedCertificate{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func (c *countingIssuer) issueCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues
}

func newManagerForTest(cfg config.CertConfig, issuer certmagic.Issuer, logger *zap.Logger) *Manager {
	return &Manager{cfg: cfg, testIssuers: []certmagic.Issuer{issuer}, acmeLogger: logger}
}

func machineACMEFixture(certDir, domain string) *config.Config {
	return &config.Config{
		Panel:   config.PanelConfig{URL: "https://panel.example.com"},
		Machine: &config.MachineConfig{MachineID: 7, Token: "machine-token"},
		Kernel:  config.KernelConfig{Type: "singbox", ConfigDir: "/etc/gboard-node/instances/m7"},
		Cert: config.CertConfig{
			CertMode: "http",
			Domain:   domain,
			Email:    "admin@example.com",
			CertDir:  certDir,
		},
	}
}

func captureZap() (*zap.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.InfoLevel)
	return zap.New(core), &buf
}

func countObtainedSuccessfully(log string) int {
	return strings.Count(log, "certificate obtained successfully")
}

// TestMachineSameDomainACMEObtainOnce is the locked review evidence:
// two machine nodes, same TLS domain, real ACME obtain path (test CA stub,
// not Let's Encrypt production). Obtain must happen once; a second node or
// restart must reuse. Different domains still obtain separately.
//
// cert_mode stays "http" (ACME). This test must not pass by switching to
// file-only or by disabling ACME.
func TestMachineSameDomainACMEObtainOnce(t *testing.T) {
	base := t.TempDir()
	domain := "node1.example.com"
	cfg := machineACMEFixture(base, domain)

	n96 := cfg.ExpandMachineNode(96, "hysteria2")
	n97 := cfg.ExpandMachineNode(97, "tuic")
	if n96.Cert.CertDir != n97.Cert.CertDir {
		t.Fatalf("precondition: same-domain machine nodes must share CertDir, got %q vs %q", n96.Cert.CertDir, n97.Cert.CertDir)
	}
	if n96.Cert.CertMode != "http" || n97.Cert.CertMode != "http" {
		t.Fatalf("cert_mode must remain ACME http, got %q / %q", n96.Cert.CertMode, n97.Cert.CertMode)
	}

	issuer := newCountingIssuer(t, "gboard-node-test-ca")
	logger, zapBuf := captureZap()
	var nlogBuf bytes.Buffer
	nlog.Init(&nlogBuf, 0, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m96 := newManagerForTest(n96.Cert, issuer, logger)
	if err := m96.Start(ctx); err != nil {
		t.Fatalf("node 96 Start: %v", err)
	}
	if !m96.HasCert() {
		t.Fatal("node 96 should have PEM after ACME obtain")
	}

	m97 := newManagerForTest(n97.Cert, issuer, logger)
	if err := m97.Start(ctx); err != nil {
		t.Fatalf("node 97 Start: %v", err)
	}
	if !m97.HasCert() {
		t.Fatal("node 97 should reuse PEM from shared storage")
	}

	// Restart: a new manager on the same storage must not obtain again.
	mRestart := newManagerForTest(n96.Cert, issuer, logger)
	if err := mRestart.Start(ctx); err != nil {
		t.Fatalf("restart Start: %v", err)
	}

	startupLog := zapBuf.String() + nlogBuf.String()
	t.Logf("machine same-domain ACME startup log:\n%s", startupLog)
	if dir := strings.TrimSpace(os.Getenv("CURSOR_ARTIFACTS_DIR")); dir != "" {
		path := filepath.Join(dir, "machine_same_domain_acme_startup.log")
		if err := os.WriteFile(path, []byte(startupLog), 0o644); err != nil {
			t.Fatalf("write startup log: %v", err)
		}
	}

	if got := issuer.issueCount(); got != 1 {
		t.Fatalf("test CA Issue() count = %d, want 1 (second node and restart must reuse)", got)
	}
	if got := countObtainedSuccessfully(startupLog); got != 1 {
		t.Fatalf("log %q count = %d, want 1; second node must not log another obtain", "certificate obtained successfully", got)
	}
}

func TestMachineSameDomainACMEObtainOnceConcurrent(t *testing.T) {
	base := t.TempDir()
	cfg := machineACMEFixture(base, "node1.example.com")
	n96 := cfg.ExpandMachineNode(96, "hysteria2")
	n97 := cfg.ExpandMachineNode(97, "tuic")

	issuer := newCountingIssuer(t, "gboard-node-test-ca")
	logger, zapBuf := captureZap()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, nodeCfg := range []config.CertConfig{n96.Cert, n97.Cert} {
		wg.Add(1)
		go func(c config.CertConfig) {
			defer wg.Done()
			errCh <- newManagerForTest(c, issuer, logger).Start(ctx)
		}(nodeCfg)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Start: %v", err)
		}
	}

	log := zapBuf.String()
	t.Logf("machine same-domain concurrent ACME startup log:\n%s", log)
	if got := issuer.issueCount(); got != 1 {
		t.Fatalf("concurrent same-domain Issue() count = %d, want 1", got)
	}
	if got := countObtainedSuccessfully(log); got != 1 {
		t.Fatalf("concurrent obtain log count = %d, want 1", got)
	}
}

func TestMachineDifferentDomainACMEObtainSeparately(t *testing.T) {
	base := t.TempDir()
	cfg := machineACMEFixture(base, "a.example.com")
	nA := cfg.ExpandMachineNode(96, "hysteria2")
	cfg.Cert.Domain = "b.example.com"
	nB := cfg.ExpandMachineNode(97, "tuic")
	if nA.Cert.CertDir == nB.Cert.CertDir {
		t.Fatalf("different domains must not share CertDir: both %q", nA.Cert.CertDir)
	}
	if nA.Cert.CertMode != "http" || nB.Cert.CertMode != "http" {
		t.Fatalf("cert_mode must remain ACME http, got %q / %q", nA.Cert.CertMode, nB.Cert.CertMode)
	}

	issuer := newCountingIssuer(t, "gboard-node-test-ca")
	logger, zapBuf := captureZap()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := newManagerForTest(nA.Cert, issuer, logger).Start(ctx); err != nil {
		t.Fatalf("domain A Start: %v", err)
	}
	if err := newManagerForTest(nB.Cert, issuer, logger).Start(ctx); err != nil {
		t.Fatalf("domain B Start: %v", err)
	}

	if got := issuer.issueCount(); got != 2 {
		t.Fatalf("different domains must each obtain, Issue() count = %d, want 2", got)
	}
	if got := countObtainedSuccessfully(zapBuf.String()); got != 2 {
		t.Fatalf("different domains must each log obtain, got %d", got)
	}
}

// TestIsolatedCertDirsObtainTwice documents the #69 bug: when each node has
// its own FileStorage, the same identifier is obtained N times.
func TestIsolatedCertDirsObtainTwice(t *testing.T) {
	issuer := newCountingIssuer(t, "gboard-node-test-ca")
	logger, zapBuf := captureZap()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	domain := "node1.example.com"
	for i, dir := range []string{t.TempDir(), t.TempDir()} {
		cfg := config.CertConfig{CertMode: "http", Domain: domain, CertDir: dir}
		if err := newManagerForTest(cfg, issuer, logger).Start(ctx); err != nil {
			t.Fatalf("isolated node %d Start: %v", i, err)
		}
	}
	if got := issuer.issueCount(); got != 2 {
		t.Fatalf("isolated storages should obtain twice (old bug), got %d", got)
	}
	if got := countObtainedSuccessfully(zapBuf.String()); got != 2 {
		t.Fatalf("isolated storages should log two obtains, got %d", got)
	}
}
