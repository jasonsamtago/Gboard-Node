package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteLegacyInstallPaths(t *testing.T) {
	in := "kernel:\n  config_dir: /etc/xboard-node/instances/abc\n# talks to the xboard panel API\n"
	got := rewriteLegacyInstallPaths(in)
	want := "kernel:\n  config_dir: /etc/gboard-node/instances/abc\n# talks to the xboard panel API\n"
	if got != want {
		t.Fatalf("rewrite:\n got: %q\nwant: %q", got, want)
	}
	if rewriteLegacyInstallPaths("xbctl / xboard") != "xbctl / xboard" {
		t.Fatal("must not rewrite generic xboard or xbctl tokens")
	}
}

func TestMigrateInstallRootMovesWhenNewAbsent(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "xboard-node")
	newRoot := filepath.Join(base, "gboard-node")
	if err := os.MkdirAll(filepath.Join(oldRoot, "instances", "n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "kernel:\n  config_dir: /etc/xboard-node/instances/n1\npanel:\n  url: https://xboard.example\n"
	if err := os.WriteFile(filepath.Join(oldRoot, "config.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "instances", "n1", "singbox.json"), []byte(`{"log":{"output":"/etc/xboard-node/instances/n1/log"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldRoot, "backups", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "backups", "old", "config.yml"), []byte("config_dir: /etc/xboard-node\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := migrateInstallRoot(oldRoot, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("expected directory move")
	}
	if dirExists(oldRoot) {
		t.Fatal("old root should be gone after move")
	}
	got, err := os.ReadFile(filepath.Join(newRoot, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "kernel:\n  config_dir: /etc/gboard-node/instances/n1\npanel:\n  url: https://xboard.example\n" {
		t.Fatalf("config rewrite mismatch:\n%s", got)
	}
	inst, err := os.ReadFile(filepath.Join(newRoot, "instances", "n1", "singbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(inst) != `{"log":{"output":"/etc/gboard-node/instances/n1/log"}}` {
		t.Fatalf("instance rewrite mismatch: %s", inst)
	}
	bak, err := os.ReadFile(filepath.Join(newRoot, "backups", "old", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != "config_dir: /etc/xboard-node\n" {
		t.Fatalf("backups must not be rewritten: %s", bak)
	}
}

func TestMigrateInstallRootKeepsOldWhenBothExist(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "xboard-node")
	newRoot := filepath.Join(base, "gboard-node")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "config.yml"), []byte("old: /etc/xboard-node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "config.yml"), []byte("new: /etc/xboard-node\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := migrateInstallRoot(oldRoot, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("must not move when both trees exist")
	}
	if !dirExists(oldRoot) {
		t.Fatal("old tree must be left in place")
	}
	oldCfg, _ := os.ReadFile(filepath.Join(oldRoot, "config.yml"))
	if string(oldCfg) != "old: /etc/xboard-node\n" {
		t.Fatalf("old tree must not be rewritten: %s", oldCfg)
	}
	newCfg, _ := os.ReadFile(filepath.Join(newRoot, "config.yml"))
	if string(newCfg) != "new: /etc/gboard-node\n" {
		t.Fatalf("active tree should rewrite leftover old paths: %s", newCfg)
	}
}

func TestResolveLayoutPrefersCurrent(t *testing.T) {
	// resolveLayout reads the real /etc paths; only assert the helper
	// preference logic via migrateInstallRoot + rewrite.
	if rewriteLegacyInstallPaths("/etc/xboard-node/config.yml") != "/etc/gboard-node/config.yml" {
		t.Fatal("path token rewrite failed")
	}
}

func TestShouldRewriteFile(t *testing.T) {
	if !shouldRewriteFile("config.yml") || !shouldRewriteFile("singbox.json") || !shouldRewriteFile("credentials.env") {
		t.Fatal("expected rewrite of yml/json/env")
	}
	if shouldRewriteFile("install.sh") || shouldRewriteFile("gboard-node") {
		t.Fatal("must not rewrite installer copy or binaries")
	}
}
