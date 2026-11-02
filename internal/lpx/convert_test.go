package lpx

import (
	"os"
	"strings"
	"testing"
)

func convert(t *testing.T, text string) Result {
	t.Helper()
	return ConvertText(text, DefaultOptions())
}

func section(result Result, name string) []string { return result.Sections[name] }

func codes(notes []Note) []string {
	out := make([]string, 0, len(notes))
	for _, note := range notes {
		out = append(out, note.Code)
	}
	return out
}

func equalStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("行数不符：得到 %d 行 %q，期望 %d 行 %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行不符：\n得到 %q\n期望 %q", i, got[i], want[i])
		}
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 文本切分
// ---------------------------------------------------------------------------

func TestSplitTop(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "保留嵌套括号",
			input: "AND, ((DOMAIN-KEYWORD, tnc), (DOMAIN-SUFFIX, a.com)), REJECT",
			want:  []string{"AND", "((DOMAIN-KEYWORD, tnc), (DOMAIN-SUFFIX, a.com))", "REJECT"},
		},
		{
			name:  "忽略正则与引号里的逗号",
			input: `/a{1,2}/, "x,y"`,
			want:  []string{`/a{1,2}/`, `"x,y"`},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			equalStrings(t, splitTop(test.input, ","), test.want)
		})
	}
}

// ---------------------------------------------------------------------------
// 参数
// ---------------------------------------------------------------------------

func TestArguments(t *testing.T) {
	t.Run("select 取默认值，未引用的参数被删除", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Cron=select, "0 1 * * *", "0 2 * * *"
Unused=switch, on, off
[Script]
cron {Cron} script-path=https://e.com/j.js, tag=Job
`)
		if !containsLine(strings.Split(result.Text, "\n"), "#!arguments=Cron:0 1 * * *") {
			t.Errorf("缺少 arguments 头部：\n%s", result.Text)
		}
		equalStrings(t, codes(result.Warnings()), []string{"argument-unused"})
	})

	t.Run("占位符改写成 Surge 语法", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Cron=select, "0 1 * * *"
[Script]
cron {Cron} script-path=https://e.com/j.js, tag=Job
`)
		equalStrings(t, section(result, "Script"), []string{
			`Job = type=cron, cronexp="{{{Cron}}}", script-path=https://e.com/j.js, timeout=300`,
		})
	})
}

// ---------------------------------------------------------------------------
// 规则
// ---------------------------------------------------------------------------

func TestRules(t *testing.T) {
	t.Run("裸域名变成拒绝规则", func(t *testing.T) {
		result := convert(t, "#!name=t\n[Rule]\nexample.com\n")
		equalStrings(t, section(result, "Rule"), []string{"DOMAIN,example.com,REJECT,extended-matching,pre-matching"})
	})

	t.Run("PROXY 策略只注释这一条", func(t *testing.T) {
		result := convert(t, "#!name=t\n[Rule]\nDOMAIN-SUFFIX, a.com, PROXY\nDOMAIN, b.com, REJECT\n")
		equalStrings(t, section(result, "Rule"), []string{"DOMAIN,b.com,REJECT,extended-matching,pre-matching"})
		equalStrings(t, codes(result.Warnings()), []string{"rule-policy"})
		if !containsLine(strings.Split(result.Text, "\n"), "# DOMAIN-SUFFIX, a.com, PROXY") {
			t.Errorf("原文没有被注释保留：\n%s", result.Text)
		}
	})

	t.Run("逻辑规则按类型补选项", func(t *testing.T) {
		result := convert(t, "#!name=t\n[Rule]\nAND, ((DOMAIN-KEYWORD, ads), (IP-CIDR, 1.2.3.4/32)), REJECT\n")
		equalStrings(t, section(result, "Rule"), []string{
			"AND,((DOMAIN-KEYWORD,ads,extended-matching),(IP-CIDR,1.2.3.4/32,no-resolve)),REJECT,pre-matching",
		})
	})

	t.Run("重复规则合并", func(t *testing.T) {
		result := convert(t, "#!name=t\n[Rule]\nDOMAIN, a.com, REJECT\nDOMAIN, a.com, REJECT\n")
		if len(section(result, "Rule")) != 1 {
			t.Errorf("期望 1 条规则，得到 %v", section(result, "Rule"))
		}
	})
}

// ---------------------------------------------------------------------------
// 改写
// ---------------------------------------------------------------------------

func TestRewrite(t *testing.T) {
	t.Run("V2 reject_dict 落到 Map Local", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
request if ${url} ~= /^https:\/\/a\.com\/ad/i then reject_dict(200)
`)
		equalStrings(t, section(result, "Map Local"), []string{
			`(?i:^https:\/\/a\.com\/ad) data-type=text data="{}" status-code=200 header="Content-Type:application/json"`,
		})
	})

	t.Run("V2 json.delete 生成 delpaths", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\/f$/ then response.json.delete(["ads", "b.list"])
`)
		equalStrings(t, section(result, "Body Rewrite"), []string{
			`http-response-jq ^https:\/\/a\.com\/f$ 'delpaths([["ads"],["b","list"]])'`,
		})
	})

	t.Run("redirect 参数顺序不影响结果", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
request if ${url} == "https://a.com/j" then redirect(302, "https://b.com")
`)
		equalStrings(t, section(result, "URL Rewrite"), []string{`^https://a\.com/j$ https://b.com 302`})
	})

	t.Run("旧语法", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
^https:\/\/t\.com\/promo url reject-dict
^https:\/\/t\.com\/jump url 302 https://e.com/f
^https:\/\/t\.com\/api response-header-add "X-A: 1"
`)
		if len(section(result, "Map Local")) != 1 {
			t.Errorf("期望 1 条 Map Local，得到 %v", section(result, "Map Local"))
		}
		equalStrings(t, section(result, "URL Rewrite"), []string{`^https:\/\/t\.com\/jump https://e.com/f 302`})
		equalStrings(t, section(result, "Header Rewrite"), []string{
			`http-response ^https:\/\/t\.com\/api header-add X-A "1"`,
		})
	})

	t.Run("无法映射的动作被注释", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
request if ${url} ~= /^https:\/\/a\.com\// then url.replace("a", "b")
`)
		equalStrings(t, codes(result.Warnings()), []string{"rewrite-action"})
		if !containsLine(strings.Split(result.Text, "\n"),
			`# request if ${url} ~= /^https:\/\/a\.com\// then url.replace("a", "b")`) {
			t.Errorf("原文没有被注释保留：\n%s", result.Text)
		}
	})
}

// ---------------------------------------------------------------------------
// 脚本
// ---------------------------------------------------------------------------

func TestScript(t *testing.T) {
	t.Run("补上 Loon 的 10 秒超时", func(t *testing.T) {
		result := convert(t, "#!name=t\n[Script]\nhttp-request ^https://a\\.com script-path=https://e.com/s.js, tag=T\n")
		equalStrings(t, section(result, "Script"), []string{
			`T = type=http-request, pattern=^https://a\.com, script-path=https://e.com/s.js, timeout=10`,
		})
	})

	t.Run("enable 变成行前缀开关", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Capture=switch, false
[Script]
http-request script-path=https://e.com/c.js, tag=C, enable={Capture} if ${url} ~= /^https:\/\/a\.com\/login/i
`)
		if !containsLine(strings.Split(result.Text, "\n"), "#!arguments=Capture:#") {
			t.Errorf("参数默认值应改成 #：\n%s", result.Text)
		}
		equalStrings(t, section(result, "Script"), []string{
			`{{{Capture}}}C = type=http-request, pattern=(?i:^https:\/\/a\.com\/login), ` +
				`script-path=https://e.com/c.js, timeout=10`,
		})
	})
}

// ---------------------------------------------------------------------------
// 头部
// ---------------------------------------------------------------------------

func TestMetadata(t *testing.T) {
	t.Run("system 与未知字段", func(t *testing.T) {
		result := convert(t, "#!name=t\n#!system=macOS\n#!loon_version=3.5.1\n")
		if !containsLine(strings.Split(result.Text, "\n"), "#!system=mac") {
			t.Errorf("macOS 应映射成 mac：\n%s", result.Text)
		}
		if strings.Contains(result.Text, "loon_version") {
			t.Errorf("loon_version 不该出现在输出里：\n%s", result.Text)
		}
	})

	t.Run("双平台时丢掉 system", func(t *testing.T) {
		result := convert(t, "#!name=t\n#!system=iOS/iPadOS, macOS\n")
		if strings.Contains(result.Text, "#!system") {
			t.Errorf("双平台不该写 system：\n%s", result.Text)
		}
	})

	t.Run("版本门槛按需抬高", func(t *testing.T) {
		plain := convert(t, "#!name=t\n[Rule]\nDOMAIN, a.com, DIRECT\n")
		modern := convert(t, "#!name=t\n[Rewrite]\nresponse if ${url} ~= /^https:\\/\\/a\\.com\\// then response.json.jq(\".\")\n")
		if !strings.Contains(plain.Text, "#!requirement=CORE_VERSION>=20") {
			t.Errorf("普通规则应停留在 20：\n%s", plain.Text)
		}
		if !strings.Contains(modern.Text, "#!requirement=CORE_VERSION>=6008000") {
			t.Errorf("JQ 应抬到 6008000：\n%s", modern.Text)
		}
	})
}

// ---------------------------------------------------------------------------
// 真实插件回归：用上游插件盯住语法解析，防止改动悄悄破坏输出
// ---------------------------------------------------------------------------

func zhihuResult(t *testing.T) Result {
	t.Helper()
	// 与 sync 流水线保持一致：category=可莉插件，带转换说明
	options := Options{Category: "可莉插件", Banner: true}
	result, err := ConvertFile("testdata/zhihu.lpx", options)
	if err != nil {
		t.Fatalf("读不到夹具：%v", err)
	}
	return result
}

func TestRealWorldZhihu(t *testing.T) {
	result := zhihuResult(t)

	t.Run("没有致命诊断", func(t *testing.T) {
		if fatal := result.Fatal(); len(fatal) > 0 {
			t.Fatalf("不该有致命诊断：%v", fatal)
		}
	})

	t.Run("section 顺序固定", func(t *testing.T) {
		equalStrings(t, result.Order, []string{"Body Rewrite", "Map Local", "MITM"})
	})

	t.Run("改写条数", func(t *testing.T) {
		if got := len(section(result, "Body Rewrite")); got != 15 {
			t.Errorf("Body Rewrite 期望 15 条，得到 %d", got)
		}
		if got := len(section(result, "Map Local")); got != 15 {
			t.Errorf("Map Local 期望 15 条，得到 %d", got)
		}
	})

	t.Run("MITM 只补一次 %APPEND%", func(t *testing.T) {
		equalStrings(t, section(result, "MITM"), []string{
			"hostname = %APPEND% m-cloud.zhihu.com, api.zhihu.com, www.zhihu.com, " +
				"page-info.zhihu.com, zhida.zhihu.com",
		})
	})

	t.Run("行内 flag 保留", func(t *testing.T) {
		for _, line := range section(result, "Body Rewrite") {
			if !strings.HasPrefix(line, "http-response-jq") && !strings.HasPrefix(line, "http-response-replace-regex") {
				t.Errorf("意外的改写行：%s", line)
			}
		}
		if !strings.Contains(section(result, "Body Rewrite")[12], `(?i:^https:\/\/api\.zhihu\.com\/search_v\d\?)`) {
			t.Errorf("第 13 条改写的行内 flag 丢了：%s", section(result, "Body Rewrite")[12])
		}
	})

	t.Run("整篇与金标准一致", func(t *testing.T) {
		golden, err := os.ReadFile("testdata/zhihu.sgmodule")
		if err != nil {
			t.Fatalf("读不到金标准：%v", err)
		}
		if result.Text != string(golden) {
			t.Errorf("输出与金标准不一致，若确认是新行为请更新 testdata/zhihu.sgmodule")
		}
	})
}

func BenchmarkConvert(b *testing.B) {
	data, err := os.ReadFile("testdata/zhihu.lpx")
	if err != nil {
		b.Fatal(err)
	}
	text := string(data)
	options := DefaultOptions()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if result := ConvertText(text, options); !result.OK() {
			b.Fatal("转换失败")
		}
	}
}
