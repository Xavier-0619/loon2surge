package external

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleTemplate = `#!name=%s
#!desc=测试模块
#!author=上游作者
#!category=测试

[Script]
main = type=http-response, pattern=^https://example\.com/api, script-path=https://example.com/a.js
`

const moduleWithRule = `#!name=%s
#!desc=测试模块

[Rule]
DOMAIN,example.com,REJECT

[Script]
main = type=http-response, pattern=^https://example\.com/api, script-path=https://example.com/a.js
`

func write(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTidyNormalizesModule(t *testing.T) {
	input := "\ufeff#!name = 乱序模块   \r\n#!desc= 测试  \r\n\r\n[MITM]\r\nhostname = %APPEND% example.com:8443, example.com\r\n\r\n[Script]\r\nmain = type=http-response, pattern=^https://api.example.com/v1, requires-body=true, script-path=https://example.com/a.js,\r\n"
	got, notes := tidy(input, "ios151/Rewrite")

	if strings.Contains(got, "\ufeff") || strings.Contains(got, "\r") {
		t.Errorf("BOM / CRLF 没清掉：\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "#!") && strings.Contains(line, " = ") {
			t.Errorf("头部等号两侧还留着空格：%q", line)
		}
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("行尾还留着空白：%q", line)
		}
	}
	if !strings.HasPrefix(got, "#!name=乱序模块\n") {
		t.Errorf("头部没按 MetaOrder 排好：\n%s", got)
	}
	for _, want := range []string{
		"#!desc=测试",
		"#!author=ios151[https://github.com/ios151/Rewrite]",
		"#!category=ios151",
		"[Script]\nmain = type=http-response, pattern=^https://api\\.example\\.com/v1, requires-body=1, script-path=https://example.com/a.js\n",
		"hostname = %APPEND% example.com\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：\n%s", want, got)
		}
	}
	if strings.Index(got, "[Script]") > strings.Index(got, "[MITM]") {
		t.Errorf("section 顺序不对：\n%s", got)
	}
	if !strings.Contains(got, "]\n\n[") && !strings.Contains(got, "\n\n[") {
		t.Errorf("段与段之间没有空行：\n%s", got)
	}
	if strings.Contains(got, ":8443") {
		t.Errorf("MITM 端口没去掉：\n%s", got)
	}
	if len(notes) == 0 {
		t.Error("整理动作没记录")
	}
}

func TestTidyIsIdempotent(t *testing.T) {
	input := "#!name=模块\n#!desc=说明\n\n[MITM]\nhostname = %APPEND% a.com, a.com\n\n[Script]\nmain = type=http-response,pattern=^https://a.com/v1,requires-body=true,script-path=https://a.com/a.js\n"
	once, _ := tidy(input, "ios151/Rewrite")
	twice, _ := tidy(once, "ios151/Rewrite")
	if once != twice {
		t.Errorf("二次整理又改了东西：\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestTidyLeavesComplicatedRegexAlone(t *testing.T) {
	line := "main = type=http-response, pattern=^https?://(a|b)\\.example\\.com/x.y, script-path=https://a.com/a.js"
	got, _ := tidy("#!name=x\n\n[Script]\n"+line+"\n", "")
	if !strings.Contains(got, line) {
		t.Errorf("带分组的正则被改动了：\n%s", got)
	}
}

func TestTidyDropsDuplicateLinesAndEmptySections(t *testing.T) {
	input := "#!name=x\n\n[Rule]\nDOMAIN,a.com,REJECT\nDOMAIN,a.com,REJECT\n\n[Panel]\n"
	got, _ := tidy(input, "")
	if strings.Count(got, "DOMAIN,a.com,REJECT") != 1 {
		t.Errorf("重复规则没去掉：\n%s", got)
	}
	if strings.Contains(got, "[Panel]") {
		t.Errorf("空 section 没删：\n%s", got)
	}
}

func TestMatchExistingKeepsKeleeModule(t *testing.T) {
	existing := []existingModule{
		{Stem: "酷狗音乐去广告", Name: "酷狗音乐去广告"},
		{Stem: "Telegram重定向", Name: "Telegram重定向"},
		{Stem: "快对去广告", Name: "快对去广告"},
		{Stem: "Line去广告", Name: "Line去广告"},
		{Stem: "Line去广告-2", Name: "linearitcurve"},
	}
	cases := []struct {
		name string
		dup  bool
	}{
		{"酷狗音乐会员解锁", true},      // 同一个 App，功能不同也算重复
		{"Telegram跳转", true},    // 摘掉"跳转/重定向"后同名
		{"快对答案", true},          // 前缀重合
		{"linearitcurve", true}, // 与同名模块内容重复
		{"咪咕视频", false},         // 不同 App
		{"Speedtest 广告vip & 净化", false},
		{"ins社区", false},
	}
	for _, item := range cases {
		_, reason, ok := matchExisting(item.name, existing)
		if ok != item.dup {
			t.Errorf("%s 判成 %v（%s），期望 %v", item.name, ok, reason, item.dup)
		}
	}
}

func TestRunMergesOnlyMissingModules(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "upstream")
	surge := filepath.Join(root, "surge")
	snapshot := filepath.Join(root, "external", "ios151")
	if err := os.MkdirAll(surge, 0o755); err != nil {
		t.Fatal(err)
	}

	write(t, surge, "已有模块.sgmodule", "#!name=已有模块\n\n[Rule]\nDOMAIN,a.com,REJECT\n")
	write(t, source, "new.sgmodule", strings.Replace(moduleTemplate, "%s", "全新模块", 1))
	write(t, source, "dup.sgmodule", strings.Replace(moduleWithRule, "%s", "已有模块", 1))
	write(t, source, "no-name.sgmodule", "[Rule]\nDOMAIN,b.com,REJECT\n")
	write(t, source, "shadow.sgmodule", "#!name=小火箭模块\n#!desc=小火箭专用模块\n\n[Rule]\nDOMAIN,c.com,REJECT\n")
	// 上游仓库里大量是脚本，不是模块；这些不该被当成模块处理
	write(t, source, "script.js", "console.log('hello')\n")
	write(t, source, "assets/icon.png", "not really a png\n")

	report, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(surge, "全新模块.sgmodule")); err != nil {
		t.Error("缺的模块应该并进 surge/")
	}
	if _, err := os.Stat(filepath.Join(snapshot, "new.sgmodule")); err != nil {
		t.Error("快照应该保留上游原文件")
	}
	if len(report.Added) != 1 {
		t.Errorf("新增应该是 1 个，实际 %d：%+v", len(report.Added), report.Added)
	}
	if len(report.Skipped) < 3 {
		t.Errorf("同名、无名字、小火箭专用都应该跳过，实际 %+v", report.Skipped)
	}

	// 再跑一次：内容没变就不该再报"新增"，也不能把自己的产物当成别人的而自我下线
	again, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Added) != 0 || len(again.Updated) != 0 {
		t.Errorf("重复运行不该产生变化：新增 %d 更新 %d", len(again.Added), len(again.Updated))
	}
	if len(again.Removed) != 0 {
		t.Errorf("重复运行不该下线任何模块：%+v", again.Removed)
	}
	if _, err := os.Stat(filepath.Join(surge, "全新模块.sgmodule")); err != nil {
		t.Error("上一轮并进来的模块不该被自己顶掉")
	}
	status := map[string]string{}
	for _, entry := range again.Manifest.Entries {
		status[entry.Name] = entry.Status
	}
	if status["全新模块"] != "merged" {
		t.Errorf("第二轮仍然应该记为 merged，实际 %q", status["全新模块"])
	}

	// 上游内容变了要认成"更新"，而不是新增一个同名文件
	write(t, source, "new.sgmodule", strings.Replace(moduleWithRule, "%s", "全新模块", 1))
	third, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Added) != 0 || len(third.Updated) != 1 {
		t.Errorf("内容变化应记为更新：新增 %d 更新 %d", len(third.Added), len(third.Updated))
	}
	if _, err := os.Stat(filepath.Join(surge, "全新模块-2.sgmodule")); err == nil {
		t.Error("更新不该生成带后缀的第二个文件")
	}
}

func TestRunWaivesChineseArgumentNamesOnly(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "upstream")
	surge := filepath.Join(root, "surge")
	snapshot := filepath.Join(root, "external", "ios151")

	write(t, source, "args.sgmodule", `#!name=中文参数模块
#!desc=上游写法
#!arguments=日志级别:info, 播放器:lenna

[Script]
main = type=http-response, pattern=^https://example\.com/api, script-path=https://example.com/a.js
`)
	write(t, source, "loon.sgmodule", `#!name=洛恩语法模块
#!desc=其实是 Loon 写法

[Script]
http-request ^https://example\.com/api script-path=https://example.com/a.js
`)

	report, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Added) != 1 || report.Added[0].Name != "中文参数模块" {
		t.Fatalf("中文参数名应该被收下：%+v", report.Added)
	}
	if len(report.Added[0].Accepted) != 1 || report.Added[0].Accepted[0] != "argument-name" {
		t.Errorf("豁免要写进清单：%+v", report.Added[0].Accepted)
	}
	if _, err := os.Stat(filepath.Join(surge, "洛恩语法模块.sgmodule")); err == nil {
		t.Error("混着 Loon 语法的模块不该进 surge/")
	}
}

func TestRunRetiresModuleDroppedUpstream(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "upstream")
	surge := filepath.Join(root, "surge")
	snapshot := filepath.Join(root, "external", "ios151")
	write(t, source, "gone.sgmodule", strings.Replace(moduleTemplate, "%s", "会下线的模块", 1))

	if _, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(surge, "会下线的模块.sgmodule")
	if _, err := os.Stat(target); err != nil {
		t.Fatal("首轮应该并进来")
	}

	if err := os.Remove(filepath.Join(source, "gone.sgmodule")); err != nil {
		t.Fatal(err)
	}
	report, err := Run(Options{SourceDir: source, SurgeDir: surge, Dir: snapshot, Repo: "ios151/Rewrite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 1 {
		t.Fatalf("上游下线的应该被回收：%+v", report.Removed)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("上游没了，surge/ 里的副本也该删掉")
	}
	if _, err := os.Stat(filepath.Join(snapshot, "gone.sgmodule")); err == nil {
		t.Error("快照也要跟着清掉")
	}
}

// 解包时路径完全来自远端仓库，而快照和 surge/ 都按它落盘：
// `../../` 一旦穿过 filepath.Join，写到的就是仓库里毫不相干的文件。
func TestReadTarballRejectsEscapingPaths(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	for _, item := range []struct{ name, text string }{
		{"Rewrite-main/good.sgmodule", fmt.Sprintf(moduleTemplate, "正常模块")},
		{"Rewrite-main/../../evil.sgmodule", fmt.Sprintf(moduleTemplate, "越界模块")},
	} {
		header := &tar.Header{Name: item.name, Mode: 0o644, Size: int64(len(item.text)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(item.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	sources, _, err := readTarball(buf.Bytes(), "ios151/Rewrite")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != "good.sgmodule" {
		t.Fatalf("越界条目不该收进来，得到 %+v", paths(sources))
	}
}

func paths(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Path)
	}
	return out
}

func TestEscapesDir(t *testing.T) {
	cases := map[string]bool{
		"a/b.sgmodule":       false,
		"good.sgmodule":      false,
		"/abs/good.sgmodule": false, // 绝对路径会被 Join 收到目录里，不算越界
		"../x.sgmodule":      true,
		"a/../../x.sgmodule": true,
		"a/b/../../../x":     true,
	}
	for path, want := range cases {
		if got := escapesDir(path); got != want {
			t.Errorf("escapesDir(%q) = %v，期望 %v", path, got, want)
		}
	}
}
