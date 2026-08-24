package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Official cedar2025/Xboard-Node #12：使用 install.sh 安装 node，当机器没有
// 安装 docker 时，脚本会卡死。
//
// 以碼為準（不是修補）：現 tip install.sh 是原生 binary＋systemd，全文沒有
// docker。本測鎖定「PATH 無 docker／有 hang stub 都不呼叫 docker、不掛起」；
// 若未來可選路徑需要 docker，缺失必須 ≤5s 非 0 並印 docker。
// 不准改 production。官方卡死更像舊版或 README 的 Docker 部署路徑。

func testScript(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(wd, "no_docker_hang_test.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("missing %s: %v", script, err)
	}
	return script
}

func runIssue12Case(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("bash", testScript(t), name)
	cmd.Dir = filepath.Dir(testScript(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("官方 #12 %s 鎖定測失敗（應為回歸鎖綠，或仍有 hang／隱式 docker）:\n%s\nerr=%v", name, out, err)
	}
	t.Logf("%s", out)
}

func TestIssue12_InstallShHappyPathNoDockerDoesNotHang(t *testing.T) {
	runIssue12Case(t, "happy")
}

func TestIssue12_InstallShHangingDockerStubIsNotCalled(t *testing.T) {
	runIssue12Case(t, "stub")
}

func TestIssue12_OptionalDockerPathMustFailFastWhenMissing(t *testing.T) {
	runIssue12Case(t, "failfast")
}

func TestIssue12_InstallShSuiteFinishesWithinBudget(t *testing.T) {
	// 整包必須遠小於卡死；避免以後有人把 sleep／retry 加進預檢。
	start := time.Now()
	runIssue12Case(t, "all")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("issue12 整包過慢 (%s)，預檢不該接近卡死", elapsed)
	}
}
