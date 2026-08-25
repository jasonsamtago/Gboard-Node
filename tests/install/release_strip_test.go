package install_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// 官方 cedar2025/Xboard-Node #48：Strip Go binary symbols to reduce image size by ~30%
// https://github.com/cedar2025/Xboard-Node/issues/48
//
// 內文：映像 binary layer ~66.8MB 帶 DWARF／符號表；應
//   go build -trimpath -ldflags="-s -w"
// -s 丟 symbol table、-w 丟 DWARF、-trimpath 去本機路徑。
// 不影響 panic 函式名、pprof、journal。
//
// 審核 2：本輪只加失敗測／回歸鎖，不准改 production。
// 範圍只鎖正式映像／發布二進位的 strip，不准擴大到壓基底／改 alpine／重寫多階段。
//
// 寫測鎖：
//  1. Happy：正式 build（Makefile 預設／CI Docker 產物指令）必須含
//     -ldflags 的 -s 與 -w，以及 -trimpath（或等價：GOFLAGS=-trimpath）。
//  2. 邊界：開發用 go test／未 strip 的本機 debug build 不回歸
//     （測不要強迫 test binary 也 strip）。
//  3. 失敗：正式 Dockerfile／workflow／Makefile build 漏 -s／-w → 必須紅。
//
// 以碼為準（不要為了紅去改 production）：
//  現 tip Makefile／Dockerfile 已有 -s -w，但正式 go build 都沒有 -trimpath。
//  因此 Happy 必須紅。測仍會在「拿掉 -s -w」時失敗（testdata 寫明）。

func TestIssue48_FormalBuildMustStripSymbolsAndTrimpath(t *testing.T) {
	root := issue48RepoRoot(t)
	recipes := issue48CollectFormalRecipes(t, root)
	if len(recipes) == 0 {
		t.Fatal("官方 #48：找不到正式 go build（Makefile build／Dockerfile／CI），測無法鎖定 strip")
	}

	var missing []string
	for _, r := range recipes {
		flags := issue48InspectGoBuild(r.command, r.vars)
		var lack []string
		if !flags.hasS {
			lack = append(lack, "-ldflags -s")
		}
		if !flags.hasW {
			lack = append(lack, "-ldflags -w")
		}
		if !flags.hasTrimpath {
			lack = append(lack, "-trimpath（或 GOFLAGS=-trimpath）")
		}
		if len(lack) > 0 {
			missing = append(missing, r.source+": 缺 "+strings.Join(lack, "、")+"\n    cmd="+issue48OneLine(r.command))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("官方 #48：正式映像／發布二進位必須 go build -trimpath -ldflags=\"-s -w\"（-s 丟 symbol table、-w 丟 DWARF、-trimpath 去本機路徑）。漏了就不算修：\n%s",
			strings.Join(missing, "\n"))
	}
}

func TestIssue48_GoTestAndDebugBuildMustNotRequireStrip(t *testing.T) {
	root := issue48RepoRoot(t)
	makefile := issue48Read(t, filepath.Join(root, "Makefile"))
	testRecipes := issue48MakefileTargetRecipes(makefile, "test")
	if len(testRecipes) == 0 {
		t.Fatal("Makefile 沒有 test 目標，無法鎖定「go test 不必 strip」")
	}

	formal := issue48CollectFormalRecipes(t, root)
	for _, r := range formal {
		if r.kind == "makefile-test" || strings.Contains(r.source, "Makefile:test") {
			t.Errorf("正式 strip 鎖誤把 Makefile test 當發布產物：%s", r.source)
		}
		if issue48LooksLikeGoTest(r.command) {
			t.Errorf("正式 strip 鎖誤把 go test 當發布產物：%s cmd=%s", r.source, issue48OneLine(r.command))
		}
	}

	for _, rec := range testRecipes {
		if !issue48LooksLikeGoTest(rec) {
			continue
		}
		flags := issue48InspectGoBuild(rec, issue48ParseMakefileVars(makefile))
		if flags.hasS || flags.hasW || flags.hasTrimpath {
			t.Logf("Makefile test 帶了 strip 旗標（允許，但不應被本票要求）：%s", issue48OneLine(rec))
		}
		// 邊界：即使沒有 -s -w -trimpath，go test 也必須被接受。
		if issue48LooksLikeGoTest(rec) && !flags.hasS && !flags.hasW && !flags.hasTrimpath {
			t.Logf("邊界：Makefile test 未 strip（正確，測不強迫 test binary strip）：%s", issue48OneLine(rec))
		}
	}

	debugMakefile := issue48Read(t, filepath.Join(issue48TestdataDir(t), "issue48_fixture_makefile_debug.mk"))
	debugVars := issue48ParseMakefileVars(debugMakefile)
	debugBuild := issue48MakefileTargetRecipes(debugMakefile, "debug")
	if len(debugBuild) == 0 {
		t.Fatal("缺 testdata 本機 debug fixture")
	}
	debugFlags := issue48InspectGoBuild(debugBuild[0], debugVars)
	if debugFlags.hasS || debugFlags.hasW || debugFlags.hasTrimpath {
		t.Fatalf("debug fixture 應是未 strip 的本機 build，got s=%v w=%v trimpath=%v cmd=%s",
			debugFlags.hasS, debugFlags.hasW, debugFlags.hasTrimpath, debugBuild[0])
	}
	// 邊界契約：未 strip 的本機 debug build 不是正式產物，不得被本測打紅。
	if issue48RecipeIsFormalMakefileTarget("debug") {
		t.Fatal("本機 debug 目標被誤列為正式發布 build")
	}
	testFix := issue48MakefileTargetRecipes(debugMakefile, "test")
	if len(testFix) == 0 || !issue48LooksLikeGoTest(testFix[0]) {
		t.Fatal("debug fixture 的 test 應是 go test")
	}
	testFlags := issue48InspectGoBuild(testFix[0], debugVars)
	if testFlags.hasS || testFlags.hasW {
		t.Fatal("邊界 fixture 的 go test 不該帶 -s -w（測不要強迫 test binary strip）")
	}
}

func TestIssue48_FormalBuildMissingSWMustFail(t *testing.T) {
	// 失敗鎖＋回歸鎖：正式 Dockerfile／Makefile build 漏 -s／-w 必須紅。
	// testdata 寫明：即使現 tip 已有 -s -w，拿掉時本測仍必須失敗。
	dataDir := issue48TestdataDir(t)

	noSWMake := issue48Read(t, filepath.Join(dataDir, "issue48_fixture_makefile_no_sw.mk"))
	noSWVars := issue48ParseMakefileVars(noSWMake)
	noSWRecipes := issue48MakefileTargetRecipes(noSWMake, "build")
	if len(noSWRecipes) == 0 {
		t.Fatal("缺 Makefile 無 -s -w fixture")
	}
	noSWFlags := issue48InspectGoBuild(noSWRecipes[0], noSWVars)
	if noSWFlags.hasS || noSWFlags.hasW {
		t.Fatalf("無 -s -w fixture 不該被解析成已 strip：s=%v w=%v cmd=%s", noSWFlags.hasS, noSWFlags.hasW, noSWRecipes[0])
	}
	if err := issue48RequireSW(noSWFlags); err == nil {
		t.Fatal("回歸鎖失敗：Makefile build 拿掉 -s -w 必須被本測打紅")
	}

	noSWDocker := issue48Read(t, filepath.Join(dataDir, "issue48_fixture_dockerfile_no_sw"))
	dockerCmds := issue48DockerfileGoBuilds(noSWDocker)
	if len(dockerCmds) == 0 {
		t.Fatal("缺 Dockerfile 無 -s -w fixture")
	}
	dockerFlags := issue48InspectGoBuild(dockerCmds[0], nil)
	if dockerFlags.hasS || dockerFlags.hasW {
		t.Fatalf("Dockerfile 無 -s -w fixture 不該被解析成已 strip：s=%v w=%v", dockerFlags.hasS, dockerFlags.hasW)
	}
	if err := issue48RequireSW(dockerFlags); err == nil {
		t.Fatal("回歸鎖失敗：正式 Dockerfile 拿掉 -s -w 必須被本測打紅")
	}

	okMake := issue48Read(t, filepath.Join(dataDir, "issue48_fixture_makefile_stripped.mk"))
	okVars := issue48ParseMakefileVars(okMake)
	okRecipes := issue48MakefileTargetRecipes(okMake, "build")
	if len(okRecipes) == 0 {
		t.Fatal("缺已 strip fixture")
	}
	okFlags := issue48InspectGoBuild(okRecipes[0], okVars)
	if err := issue48RequireFormalStrip(okFlags); err != nil {
		t.Fatalf("已帶 -s -w -trimpath 的 fixture 應過，got %v cmd=%s", err, okRecipes[0])
	}
	strippedNoSW := strings.ReplaceAll(okMake, "-s -w ", "")
	strippedNoSW = strings.ReplaceAll(strippedNoSW, "-s -w", "")
	noSWFromOK := issue48InspectGoBuild(issue48MakefileTargetRecipes(strippedNoSW, "build")[0], issue48ParseMakefileVars(strippedNoSW))
	if err := issue48RequireSW(noSWFromOK); err == nil {
		t.Fatal("回歸鎖失敗：從已齊 fixture 拿掉 -s -w 必須紅（testdata 寫明）")
	}

	// 現 tip 若漏 -s／-w，本測必須紅（以碼為準；現況應已有 -s -w）。
	root := issue48RepoRoot(t)
	for _, r := range issue48CollectFormalRecipes(t, root) {
		flags := issue48InspectGoBuild(r.command, r.vars)
		if err := issue48RequireSW(flags); err != nil {
			t.Errorf("官方 #48 失敗鎖：%s 漏 -s／-w → 必須紅：%v\n    cmd=%s", r.source, err, issue48OneLine(r.command))
		}
	}
}

type issue48Flags struct {
	hasS, hasW, hasTrimpath bool
}

type issue48Recipe struct {
	source  string
	kind    string
	command string
	vars    map[string]string
}

func issue48RequireSW(f issue48Flags) error {
	var lack []string
	if !f.hasS {
		lack = append(lack, "-s")
	}
	if !f.hasW {
		lack = append(lack, "-w")
	}
	if len(lack) == 0 {
		return nil
	}
	return &issue48FlagError{msg: "ldflags 缺 " + strings.Join(lack, "／")}
}

func issue48RequireFormalStrip(f issue48Flags) error {
	if err := issue48RequireSW(f); err != nil {
		return err
	}
	if !f.hasTrimpath {
		return &issue48FlagError{msg: "缺 -trimpath（或 GOFLAGS=-trimpath）"}
	}
	return nil
}

type issue48FlagError struct{ msg string }

func (e *issue48FlagError) Error() string { return e.msg }

func issue48RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, name := range []string{"Makefile", "Dockerfile"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("repo root 缺 %s（wd=%s root=%s）: %v", name, wd, root, err)
		}
	}
	return root
}

func issue48TestdataDir(t *testing.T) string {
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

func issue48Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func issue48RecipeIsFormalMakefileTarget(name string) bool {
	switch name {
	case "build", "build-linux", "build-linux-arm64":
		return true
	default:
		return false
	}
}

func issue48CollectFormalRecipes(t *testing.T, root string) []issue48Recipe {
	t.Helper()
	var out []issue48Recipe

	makefile := issue48Read(t, filepath.Join(root, "Makefile"))
	vars := issue48ParseMakefileVars(makefile)
	for _, target := range []string{"build", "build-linux", "build-linux-arm64"} {
		for i, rec := range issue48MakefileTargetRecipes(makefile, target) {
			if !issue48LooksLikeGoBuild(rec) {
				continue
			}
			out = append(out, issue48Recipe{
				source:  "Makefile:" + target + "#" + strconv.Itoa(i),
				kind:    "makefile",
				command: rec,
				vars:    vars,
			})
		}
	}

	dockerfile := issue48Read(t, filepath.Join(root, "Dockerfile"))
	dockerVars := map[string]string{}
	if issue48DockerfileHasTrimpathGOFLAGS(dockerfile) {
		dockerVars["GOFLAGS"] = "-trimpath"
	}
	for i, rec := range issue48DockerfileGoBuilds(dockerfile) {
		out = append(out, issue48Recipe{
			source:  "Dockerfile#" + strconv.Itoa(i),
			kind:    "dockerfile",
			command: rec,
			vars:    dockerVars,
		})
	}

	workflowDir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		src := issue48Read(t, filepath.Join(workflowDir, name))
		for i, rec := range issue48WorkflowRawGoBuilds(src) {
			out = append(out, issue48Recipe{
				source:  ".github/workflows/" + name + "#go-build-" + strconv.Itoa(i),
				kind:    "workflow",
				command: rec,
				vars:    nil,
			})
		}
	}
	return out
}

func issue48ParseMakefileVars(src string) map[string]string {
	vars := map[string]string{}
	assign := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(?::=|\?=|=)\s*(.*)$`)
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ".") {
			continue
		}
		if strings.Contains(line, ":") && !strings.Contains(line, ":=") && !strings.Contains(line, "?=") {
			// 目標列，例如 build: 或 install: build
			if !assign.MatchString(line) {
				continue
			}
		}
		m := assign.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		vars[m[1]] = strings.TrimSpace(m[2])
	}
	return vars
}

func issue48MakefileTargetRecipes(src, target string) []string {
	var recipes []string
	header := regexp.MustCompile(`^` + regexp.QuoteMeta(target) + `\s*:`)
	in := false
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			trimmed := strings.TrimSpace(line)
			if header.MatchString(trimmed) {
				in = true
				continue
			}
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ".") {
				if strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "\t") {
					in = false
				}
			}
			continue
		}
		if !in {
			continue
		}
		rec := strings.TrimLeft(line, "\t ")
		if rec == "" || strings.HasPrefix(rec, "#") {
			continue
		}
		recipes = append(recipes, rec)
	}
	return recipes
}

func issue48DockerfileGoBuilds(src string) []string {
	joined := issue48JoinContinuations(src)
	var out []string
	for _, line := range strings.Split(joined, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if issue48LooksLikeGoBuild(trim) {
			out = append(out, trim)
		}
	}
	return out
}

func issue48DockerfileHasTrimpathGOFLAGS(src string) bool {
	joined := issue48JoinContinuations(src)
	re := regexp.MustCompile(`(?i)(?:ENV|ARG)\s+GOFLAGS=(\S+)`)
	for _, m := range re.FindAllStringSubmatch(joined, -1) {
		if strings.Contains(m[1], "-trimpath") {
			return true
		}
	}
	return false
}

func issue48WorkflowRawGoBuilds(src string) []string {
	joined := issue48JoinContinuations(src)
	var out []string
	for _, line := range strings.Split(joined, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if issue48LooksLikeGoBuild(trim) && !strings.Contains(trim, "make ") {
			out = append(out, trim)
		}
	}
	return out
}

func issue48LooksLikeGoBuild(cmd string) bool {
	tokens := issue48Tokenize(issue48ExpandMakeVars(cmd, nil))
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i] == "go" && tokens[i+1] == "build" {
			return true
		}
	}
	return false
}

func issue48LooksLikeGoTest(cmd string) bool {
	tokens := issue48Tokenize(cmd)
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i] == "go" && tokens[i+1] == "test" {
			return true
		}
	}
	return false
}

func issue48InspectGoBuild(cmd string, vars map[string]string) issue48Flags {
	expanded := issue48ExpandMakeVars(cmd, vars)
	tokens := issue48Tokenize(expanded)
	var f issue48Flags
	if v, ok := vars["GOFLAGS"]; ok && strings.Contains(v, "-trimpath") {
		f.hasTrimpath = true
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "-trimpath" {
			f.hasTrimpath = true
		}
		if strings.HasPrefix(tok, "GOFLAGS=") && strings.Contains(tok, "-trimpath") {
			f.hasTrimpath = true
		}
		ld := ""
		switch {
		case tok == "-ldflags" && i+1 < len(tokens):
			ld = tokens[i+1]
			i++
		case strings.HasPrefix(tok, "-ldflags="):
			ld = strings.TrimPrefix(tok, "-ldflags=")
		}
		if ld == "" {
			continue
		}
		ld = issue48ExpandMakeVars(ld, vars)
		for _, flag := range issue48Tokenize(ld) {
			switch flag {
			case "-s":
				f.hasS = true
			case "-w":
				f.hasW = true
			}
		}
	}
	return f
}

func issue48ExpandMakeVars(s string, vars map[string]string) string {
	if vars == nil {
		vars = map[string]string{}
	}
	re := regexp.MustCompile(`\$(\([A-Za-z_][A-Za-z0-9_]*\)|\{[A-Za-z_][A-Za-z0-9_]*\})`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(m, "$"), ")")
		name = strings.TrimSuffix(strings.TrimPrefix(name, "("), ")")
		name = strings.TrimSuffix(strings.TrimPrefix(name, "{"), "}")
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

func issue48JoinContinuations(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for strings.Contains(s, "\\\n") {
		s = strings.ReplaceAll(s, "\\\n", " ")
	}
	return s
}

func issue48Tokenize(s string) []string {
	var out []string
	var buf strings.Builder
	quote := rune(0)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		out = append(out, buf.String())
		buf.Reset()
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				break
			}
			buf.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			buf.WriteRune(r)
		}
	}
	flush()
	return out
}

func issue48OneLine(s string) string {
	s = issue48JoinContinuations(s)
	return strings.Join(strings.Fields(s), " ")
}
