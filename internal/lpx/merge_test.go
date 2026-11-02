package lpx

// 同名插件融合的用例。真实的例子是可莉的「挖财记账去广告」：
// 上游把它拆成两份，各自补一半接口，两份都写同一个 #!name。

import (
	"slices"
	"strings"
	"testing"
)

const mergeBannerPlugin = `#!name=示例去广告
#!desc=移除横幅。
#!author=甲
#!tag=去广告

[General]
foo=1

[Rewrite]
request if ${url} ~= /^https:\/\/api\.example\.com\/banner/i then reject_dict(200)
request if ${url} ~= /^https:\/\/api\.example\.com\/feed$/i then reject_dict(200)

[MitM]
hostname=api.example.com
`

const mergeSplashPlugin = `#!name=示例去广告
#!desc=移除开屏。
#!author=甲
#!tag=去广告

[General]
bar=2

[Rewrite]
request if ${url} ~= /^https:\/\/api\.example\.com\/feed$/i then reject_dict(200)
request if ${url} ~= /^https:\/\/api\.example\.com\/splash/i then reject(404)

[MitM]
hostname=api.example.com
`

func convertPair(t *testing.T) Result {
	t.Helper()
	result := ConvertSources([]Source{
		{Name: "banner.lpx", Text: mergeBannerPlugin},
		{Name: "splash.lpx", Text: mergeSplashPlugin},
	}, defaultOptions())
	if !result.OK() {
		t.Fatalf("融合后应当可转换：%v", result.Fatal())
	}
	return result
}

func TestMergeUnionOfRules(t *testing.T) {
	result := convertPair(t)

	// feed 两份都写了、动作一样，只该留一行。
	lines := section(result, "Map Local")
	if len(lines) != 3 {
		t.Fatalf("应合并出 3 条 Map Local，得到 %d 条：\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if count := countContaining(lines, "feed"); count != 1 {
		t.Errorf("重复规则应只留一条，%q 出现了 %d 次", "feed", count)
	}
	for _, want := range []string{"banner", "splash"} {
		if countContaining(lines, want) != 1 {
			t.Errorf("缺 %s 的规则：\n%s", want, strings.Join(lines, "\n"))
		}
	}

	// MITM 两份都是同一个主机，输出只要一行。
	equalStrings(t, section(result, "MITM"), []string{"hostname = %APPEND% api.example.com"})
}

func TestMergeHeader(t *testing.T) {
	result := convertPair(t)

	if result.Name != "示例去广告" {
		t.Errorf("名字取主源：%q", result.Name)
	}
	if !strings.Contains(result.Text, "#!desc=移除横幅；移除开屏") {
		t.Errorf("两份 desc 应拼在头部：\n%s", result.Text)
	}
	// 作者和标签两边一样，不该重复
	if strings.Count(result.Text, "#!author=") != 1 || strings.Contains(result.Text, "甲, 甲") {
		t.Errorf("相同作者应去重：\n%s", result.Text)
	}
	if len(result.Sources) != 2 {
		t.Errorf("Result 应记下两个来源：%v", result.Sources)
	}
	if !slices.Contains(codes(result.Notes), "merged-source") {
		t.Errorf("缺融合诊断：%v", codes(result.Notes))
	}
}

func TestMergeKeepsNoteSource(t *testing.T) {
	result := convertPair(t)

	// 合并后行号是重排的，只说"第几行"会指错文件，所以每条带行号的诊断都要有来源。
	seen := map[string]bool{}
	for _, note := range result.Notes {
		if note.Code != "general-unsupported" {
			continue
		}
		if note.Source == "" {
			t.Fatalf("诊断没有来源：%s", note)
		}
		seen[note.Source] = true
		if !strings.Contains(note.String(), note.Source+" L") {
			t.Errorf("诊断应打印来源：%s", note)
		}
	}
	if !seen["banner.lpx"] || !seen["splash.lpx"] {
		t.Errorf("两个源的诊断应各自归位，得到 %v", seen)
	}
}

func TestMergeKeepsArgumentsWithoutCollision(t *testing.T) {
	// 两份都声明同一个参数：合并时按行去重，不能报"参数重名"这种致命错误。
	plugin := `#!name=示例
[Argument]
Mode=select, "A", "A", "B"

[Script]
http-request script-path=https://x/a.js, tag=t, argument=Mode if ${url} ~= /a/i
`
	result := ConvertSources([]Source{{Name: "a.lpx", Text: plugin}, {Name: "b.lpx", Text: plugin}}, defaultOptions())
	if fatal := result.Fatal(); len(fatal) > 0 {
		t.Fatalf("同名参数不该冲突：%v", fatal)
	}
	if len(section(result, "Script")) != 1 {
		t.Errorf("同一行脚本只该留一条：%q", section(result, "Script"))
	}
}

func TestConvertSourcesSingleMatchesText(t *testing.T) {
	// 只有一份时必须与 ConvertText 逐字节一致，否则同步和命令行的产例会分叉。
	plain := ConvertText(mergeBannerPlugin, defaultOptions())
	single := ConvertSources([]Source{{Name: "banner.lpx", Text: mergeBannerPlugin}}, defaultOptions())

	if plain.Text != single.Text {
		t.Fatalf("单源转换结果不一致：\n--- ConvertText ---\n%s\n--- ConvertSources ---\n%s", plain.Text, single.Text)
	}
	if len(single.Sources) != 0 {
		t.Errorf("单源不该记来源：%v", single.Sources)
	}
}

func TestMergeDescMatchesModuleHeader(t *testing.T) {
	// index.json 的 desc 来自上游列表，模块头部来自 #!desc；融合时两者必须用同一条拼接。
	sources := []Source{
		{Name: "banner.lpx", Text: mergeBannerPlugin},
		{Name: "splash.lpx", Text: mergeSplashPlugin},
	}
	got := MergeDesc(sources)
	want := "移除横幅；移除开屏"
	if got != want {
		t.Fatalf("拼接结果 = %q，想要 %q", got, want)
	}
	result := convertPair(t)
	if !strings.Contains(result.Text, "#!desc="+want) {
		t.Errorf("模块头部应与 MergeDesc 一致：\n%s", result.Text)
	}
	if got := MergeDesc([]Source{{Text: mergeBannerPlugin}}); got != "移除横幅" {
		t.Errorf("单源应原样保留（去句末标点），得到 %q", got)
	}
}

func TestConvertSourcesEmpty(t *testing.T) {
	result := ConvertSources(nil, defaultOptions())
	if result.OK() {
		t.Fatal("没有输入时不应产出模块")
	}
}

func countContaining(lines []string, want string) int {
	count := 0
	for _, line := range lines {
		if strings.Contains(line, want) {
			count++
		}
	}
	return count
}
