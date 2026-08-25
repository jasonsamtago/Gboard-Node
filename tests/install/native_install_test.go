package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Official cedar2025/Xboard-Node #18：能不能出個本地安裝版本
// 內文：小雞節點性能差的裝Docker太臃腫了
// 官方回覆（yywudi）：用 install.sh 啊，不加 --docker 就是普通的下载
// go 二进制文件+配置文件
//
// 以碼為準（不是修補）：現 tip install.sh 是原生 binary＋systemd，沒有
// --docker。本測鎖定「無 Docker 時本機安裝必須走到下載二進位＋寫設定」；
// 文件必須寫明本機安裝；硬依賴 Docker／只提示裝 Docker 必須紅。
// 不准改 production。不要重做 #12 卡死測（見 no_docker_hang_test.go）。

func issue18Script(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(wd, "native_install_test.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("missing %s: %v", script, err)
	}
	return script
}

func runIssue18Case(t *testing.T, name string) {
	t.Helper()
	script := issue18Script(t)
	cmd := exec.Command("bash", script, name)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("官方 #18 %s 鎖定測失敗（應為回歸鎖綠，或本機安裝被改成硬依賴 Docker）:\n%s\nerr=%v", name, out, err)
	}
	t.Logf("%s", out)
}

func TestIssue18_NativeInstallHappyPathDownloadsBinaryAndWritesConfig(t *testing.T) {
	runIssue18Case(t, "happy")
}

func TestIssue18_DocsMustDocumentNativeInstallWithoutDocker(t *testing.T) {
	runIssue18Case(t, "docs")
}

func TestIssue18_HardDockerDependencyMustFail(t *testing.T) {
	runIssue18Case(t, "harddep")
}

func TestIssue18_NativeInstallSuiteFinishesWithinBudget(t *testing.T) {
	start := time.Now()
	runIssue18Case(t, "all")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("issue18 整包過慢 (%s)，本機安裝預檢不該接近卡死", elapsed)
	}
}
