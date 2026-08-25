package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 官方 cedar2025/Xboard-Node #62：麻烦增加下FreeBSD编译文件
// https://github.com/cedar2025/Xboard-Node/issues/62
//
// 內文：1. freebsd 裝不了  2. 最好能配合 xboard 的安裝命令自適應安裝
//
// 審核 2：本輪只加失敗測／回歸鎖，不准改 production。
// 範圍只鎖編譯產物＋install.sh 自適應。不准擴大到真機跑 FreeBSD 服務、
// 改 kernel、整包移植 jail。
//
// 寫測鎖：
//  1. Happy：GOOS=freebsd 正式 build 目標存在（Makefile 或 CI）；
//     install.sh 在 FreeBSD（或模擬 uname=FreeBSD）必須解析到 freebsd
//     二進位 URL，不得硬編碼 linux。
//  2. 邊界：linux／amd64 本機安裝不回歸（#18）。
//  3. 失敗：uname=FreeBSD 仍下 linux 包、或腳本直接拒絕／掛死 → 必須紅。
//
// 以碼為準（不要為了紅去改 production）：
//  現 tip Makefile／CI 只有 linux；install.sh stage_binary／stage_gbctl
//  硬編碼 gboard-node-linux-${ARCH}／gbctl-linux-${ARCH}。
//  因此 Happy 與失敗鎖對現 tip 必須紅。現 tip 若已齊 → 回歸鎖綠。

func TestIssue62_FormalBuildMustHaveFreeBSDTarget(t *testing.T) {
	root := issue62RepoRoot(t)
	makefile := issue62Read(t, filepath.Join(root, "Makefile"))
	workflows := issue62ReadWorkflows(t, root)

	hits := issue62CollectFormalFreeBSD(makefile, workflows)
	if len(hits) == 0 {
		t.Fatalf("官方 #62：正式發布必須有 GOOS=freebsd 編譯目標（Makefile 如 build-freebsd／GOOS=freebsd，或 CI matrix goos: freebsd），產出 gboard-node-freebsd-*。現況只有 linux（Makefile build-linux／CI linux／amd64+arm64），不算修。")
	}
	for _, h := range hits {
		t.Logf("freebsd 正式目標：%s", h)
	}
}

func TestIssue62_InstallShOnFreeBSDMustResolveFreeBSDURL(t *testing.T) {
	runIssue62Case(t, "happy")
}

func TestIssue62_LinuxAmd64NativeInstallMustNotRegress(t *testing.T) {
	// 邊界：不要重做 #18 測檔；呼叫既有 Happy，再鎖 linux URL／Makefile linux 目標。
	runIssue18Case(t, "happy")
	runIssue62Case(t, "boundary")
}

func TestIssue62_FreeBSDLinuxPackageOrRejectMustFail(t *testing.T) {
	runIssue62Case(t, "fail")
}

func TestIssue62_CheckerFixturesAndFormalTargetLock(t *testing.T) {
	// 回歸鎖：linux-only fixture 必須被打紅；已有 freebsd 的 fixture 必須綠。
	// testdata 寫明：即使現 tip 以後補上 freebsd，拿掉時本測仍必須失敗。
	dataDir := issue62TestdataDir(t)

	linuxMake := issue62Read(t, filepath.Join(dataDir, "issue62_fixture_makefile_linux_only.mk"))
	linuxCI := map[string]string{"linux-only.yml": issue62Read(t, filepath.Join(dataDir, "issue62_fixture_ci_linux_only.yml"))}
	if hits := issue62CollectFormalFreeBSD(linuxMake, linuxCI); len(hits) != 0 {
		t.Fatalf("linux-only fixture 不該被解析成已有 freebsd 目標：%v", hits)
	}

	okMake := issue62Read(t, filepath.Join(dataDir, "issue62_fixture_makefile_freebsd.mk"))
	if hits := issue62CollectFormalFreeBSD(okMake, nil); len(hits) == 0 {
		t.Fatal("已有 GOOS=freebsd 的 Makefile fixture 應被認作正式目標")
	}

	okCI := map[string]string{"freebsd.yml": issue62Read(t, filepath.Join(dataDir, "issue62_fixture_ci_freebsd.yml"))}
	if hits := issue62CollectFormalFreeBSD("# no makefile\n", okCI); len(hits) == 0 {
		t.Fatal("已有 goos: freebsd 的 CI fixture 應被認作正式目標")
	}

	// 從正例拿掉 freebsd 必須再紅。
	stripped := strings.ReplaceAll(okMake, "freebsd", "linux")
	stripped = strings.ReplaceAll(stripped, "build-linux:", "build-linux-dup:")
	if hits := issue62CollectFormalFreeBSD(stripped, nil); len(hits) != 0 {
		t.Fatalf("回歸鎖失敗：從已齊 Makefile fixture 拿掉 freebsd 必須紅，got %v", hits)
	}
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
		t.Fatalf("官方 #62 %s 鎖定測失敗（現況應為失敗測紅，或 freebsd 目標／自適應被拿掉）：\n%s\nerr=%v", name, out, err)
	}
	t.Logf("%s", out)
}

func TestIssue62_SuiteFinishesWithinBudget(t *testing.T) {
	start := time.Now()
	// 不跑 all（含 fail／happy 對現 tip 會紅）；只鎖腳本本身不會卡死。
	// hang fixture 最多 5s。這裡單獨跑 fixture 契約。
	script := issue62Script(t)
	cmd := exec.Command("bash", script, "fixtures")
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("官方 #62 fixture 契約失敗:\n%s\nerr=%v", out, err)
	}
	t.Logf("%s", out)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("issue62 fixture 整包過慢 (%s)，預檢不該接近卡死", elapsed)
	}
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

func issue62Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func issue62ReadWorkflows(t *testing.T, root string) map[string]string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	out := map[string]string{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		out[name] = issue62Read(t, filepath.Join(dir, name))
	}
	return out
}

var (
	issue62ReGOOS     = regexp.MustCompile(`(?i)(?:^|[\s"'=])GOOS=freebsd(?:[\s"']|$)`)
	issue62ReYAMLGoos = regexp.MustCompile(`(?i)goos:\s*freebsd\b`)
	issue62ReTarget   = regexp.MustCompile(`(?m)^build-freebsd(?:-[A-Za-z0-9]+)?\s*:`)
	issue62ReArtifact = regexp.MustCompile(`(?i)(?:gboard-node|gbctl)-freebsd-[A-Za-z0-9]+`)
	issue62ReMakeFB   = regexp.MustCompile(`(?i)make\s+build-freebsd(?:-[A-Za-z0-9]+)?\b`)
)

func issue62CollectFormalFreeBSD(makefile string, workflows map[string]string) []string {
	var hits []string
	hits = append(hits, issue62ScanFreeBSDSignals("Makefile", makefile)...)
	for name, src := range workflows {
		hits = append(hits, issue62ScanFreeBSDSignals(".github/workflows/"+name, src)...)
	}
	return hits
}

func issue62ScanFreeBSDSignals(source, src string) []string {
	var hits []string
	for i, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		n := strconv.Itoa(i + 1)
		switch {
		case issue62ReGOOS.MatchString(trim):
			hits = append(hits, source+":"+n+": GOOS=freebsd")
		case issue62ReYAMLGoos.MatchString(trim):
			hits = append(hits, source+":"+n+": goos: freebsd")
		case issue62ReTarget.MatchString(trim):
			hits = append(hits, source+":"+n+": "+trim)
		case issue62ReArtifact.MatchString(trim):
			hits = append(hits, source+":"+n+": freebsd artifact")
		case issue62ReMakeFB.MatchString(trim):
			hits = append(hits, source+":"+n+": make build-freebsd")
		}
	}
	return hits
}
