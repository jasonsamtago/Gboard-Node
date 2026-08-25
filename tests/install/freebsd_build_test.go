package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 官方 cedar2025/Xboard-Node #62：麻烦增加下FreeBSD编译文件
// https://github.com/cedar2025/Xboard-Node/issues/62
//
// 審核 2 已把 ATDD 寫死。本輪只加失敗測，不准改 production。
// 限界：發布／安裝（Makefile＋install.sh）。
// 編譯檔＝make build-freebsd 產出 gboard-node-freebsd-${ARCH}。
// 自適應＝同一條 curl|bash 依 uname 選該成品＋寫 rc.d（不是 systemd）。
// 不做 ports/pkg、OpenBSD／macOS、核心移植、擴大發行面。
//
// ATDD：
//  1. Happy：make build-freebsd 產出 gboard-node-freebsd-${ARCH}；
//     uname=FreeBSD 時 install.sh 拿該成品並寫 rc.d
//  2. 邊界：Linux 仍走 systemd＋*-linux-*
//  3. 失敗：未知 OS／缺 freebsd 成品要明示失敗，不准當 linux
//
// 現 tip d15d962 只有 build-linux*，install.sh 硬綁 systemd＋
// gboard-node-linux-${ARCH}。Happy 應紅。不要為了紅去改 production。

func TestIssue62_MakeBuildFreeBSDProducesFreeBSDArtifact(t *testing.T) {
	root := issue62RepoRoot(t)
	out, err := issue62MakeNBuildFreeBSD(root, filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("官方 #62 Happy：必須有 make build-freebsd，產出 gboard-node-freebsd-${ARCH}（GOOS=freebsd）。現況只有 build-linux*：\n%s\nerr=%v", out, err)
	}
	if !issue62HasGOOSFreeBSD(out) || !issue62HasFreeBSDArtifact(out) {
		t.Fatalf("官方 #62 Happy：make build-freebsd 必須 GOOS=freebsd 且產出 gboard-node-freebsd-${ARCH}。got:\n%s", out)
	}
	t.Logf("make -n build-freebsd:\n%s", out)
}

func TestIssue62_InstallShOnFreeBSDTakesArtifactAndWritesRcd(t *testing.T) {
	runIssue62Case(t, "happy")
}

func TestIssue62_LinuxStillSystemdAndLinuxArtifact(t *testing.T) {
	runIssue62Case(t, "boundary")
}

func TestIssue62_UnknownOSOrMissingFreeBSDMustExplicitFail(t *testing.T) {
	runIssue62Case(t, "fail")
}

func TestIssue62_ATDDFixtures(t *testing.T) {
	root := issue62RepoRoot(t)
	dataDir := issue62TestdataDir(t)

	linuxOnly := filepath.Join(dataDir, "issue62_fixture_makefile_linux_only.mk")
	if out, err := issue62MakeNBuildFreeBSD(root, linuxOnly); err == nil {
		t.Fatalf("linux-only fixture 不該有 make build-freebsd：\n%s", out)
	}

	okMake := filepath.Join(dataDir, "issue62_fixture_makefile_freebsd.mk")
	out, err := issue62MakeNBuildFreeBSD(root, okMake)
	if err != nil {
		t.Fatalf("freebsd fixture 的 make build-freebsd 應能 dry-run：\n%s\nerr=%v", out, err)
	}
	if !issue62HasGOOSFreeBSD(out) || !issue62HasFreeBSDArtifact(out) {
		t.Fatalf("freebsd fixture 必須產出 gboard-node-freebsd-${ARCH}／GOOS=freebsd。got:\n%s", out)
	}

	runIssue62Case(t, "fixtures")
}

func issue62Script(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(wd, "freebsd_build_test.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("missing %s: %v", script, err)
	}
	return script
}

func runIssue62Case(t *testing.T, name string) {
	t.Helper()
	script := issue62Script(t)
	cmd := exec.Command("bash", script, name)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("官方 #62 %s 鎖定測失敗（現況 Happy／失敗應紅；或 build-freebsd／rc.d／明示失敗被拿掉）：\n%s\nerr=%v", name, out, err)
	}
	t.Logf("%s", out)
}

func issue62RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, name := range []string{"Makefile", "install.sh"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("repo root 缺 %s（wd=%s root=%s）: %v", name, wd, root, err)
		}
	}
	return root
}

func issue62TestdataDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(wd, "testdata")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func issue62MakeNBuildFreeBSD(dir, makefile string) (string, error) {
	cmd := exec.Command("make", "-n", "-f", makefile, "build-freebsd")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var (
	issue62ReGOOS     = regexp.MustCompile(`(?i)(?:^|[\s"'=])GOOS=freebsd(?:[\s"']|$)`)
	issue62ReArtifact = regexp.MustCompile(`gboard-node-freebsd-(\$\{?ARCH\}?|\$\(?ARCH\)?|[A-Za-z0-9_]+)`)
)

func issue62HasGOOSFreeBSD(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if issue62ReGOOS.MatchString(trim) {
			return true
		}
	}
	return false
}

func issue62HasFreeBSDArtifact(s string) bool {
	return issue62ReArtifact.MatchString(s)
}
