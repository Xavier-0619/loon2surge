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
request if ${url} ~= /^https:\/\/a\.com\// then url.header.set("a", "b")
`)
		equalStrings(t, codes(result.Warnings()), []string{"rewrite-action"})
		if !containsLine(strings.Split(result.Text, "\n"),
			`# request if ${url} ~= /^https:\/\/a\.com\// then url.header.set("a", "b")`) {
			t.Errorf("原文没有被注释保留：\n%s", result.Text)
		}
	})

	t.Run("url.replace 只改 URL", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
request if ${url} ~= /^https:\/\/a\.com\// then url.replace("https://b.com/")
`)
		equalStrings(t, section(result, "URL Rewrite"), []string{
			`^https:\/\/a\.com\/ https://b.com/ header`,
		})
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
			// Surge 的 Body Rewrite 只有 `http-response <url> <正则> <替换>` 与
			// `http-response-jq <url> <表达式>` 两种写法，没有 *-replace-regex 变体。
			if !strings.HasPrefix(line, "http-response-jq ") && !strings.HasPrefix(line, "http-response ") {
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

// TestScriptV2Action 覆盖 Loon V2 的 `… then script("url") with …` 写法。
//
// 这类插件以前整批被判成"缺少 script-path"：路径其实写在 script(…) 里，
// 不是 script-path= 属性。可莉的节点检测类插件几乎全是这种写法。
func TestScriptV2Action(t *testing.T) {
	t.Run("generic 取 script(…) 里的路径", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
generic then script("https://e.com/n.js") with tag="代理链路检测", img_url="link.circle.system"
`)
		equalStrings(t, section(result, "Script"), []string{
			`代理链路检测 = type=generic, script-path=https://e.com/n.js, timeout=300`,
		})
	})

	t.Run("request if … then script(…) 转成 http-request", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
request if ${url} ~= /^https?:\/\/a\.com\/v3\/.*\/apps$/i then script("https://e.com/c.js") with tag="取令牌"
`)
		equalStrings(t, section(result, "Script"), []string{
			`取令牌 = type=http-request, pattern=(?i:^https?:\/\/a\.com\/v3\/.*\/apps$), ` +
				`script-path=https://e.com/c.js, timeout=10`,
		})
	})

	t.Run("cron 表达式留在左边当 cronexp", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Cron=input, "0 0/2 * * * ?"
[Script]
cron ${Cron} then script("https://e.com/j.js") with tag="定时", timeout=600
`)
		equalStrings(t, section(result, "Script"), []string{
			`定时 = type=cron, cronexp="{{{Cron}}}", script-path=https://e.com/j.js, timeout=600`,
		})
	})

	t.Run("img_url 是显示元数据，静默丢弃", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
generic then script("https://e.com/n.js") with tag="T", img_url="a.b"
`)
		for _, note := range result.Notes {
			if note.Code == "script-attribute" {
				t.Errorf("img_url 不该报未知属性：%v", note)
			}
		}
	})
}

// TestModuleExcluded 说明什么样的插件算"Surge 表达不了"，而不是源文件坏了。
func TestModuleExcluded(t *testing.T) {
	proxyOnly := `#!name=t
[Rule]
DOMAIN-SUFFIX, a.com, PROXY
DOMAIN, b.com, PROXY
`
	result := convert(t, proxyOnly)
	if result.OK() {
		t.Fatalf("整份规则都是 PROXY，不该产出模块：\n%s", result.Text)
	}
	if !result.Excluded() {
		t.Errorf("应判定为排除而不是失败：%v", result.Notes)
	}
	if !strings.Contains(result.ExcludeReason(), "PROXY") {
		t.Errorf("排除理由要提到 PROXY：%q", result.ExcludeReason())
	}

	// 有脚本可用时仍要产出，规则那行留在末尾注释里等人处理
	mixed := `#!name=t
[Rule]
DOMAIN, a.com, PROXY
[Script]
request if ${url} ~= /x/ then script("https://e.com/a.js") with tag="A"
`
	ok := convert(t, mixed)
	if !ok.OK() {
		t.Fatalf("有可用脚本就该产出：%v", ok.Notes)
	}
	if !strings.Contains(ok.Text, "# DOMAIN, a.com, PROXY") {
		t.Errorf("无法转换的规则应保留为注释：\n%s", ok.Text)
	}

	// 源文件本身有毛病时不能被当成"排除"，那是真要人看的失败
	broken := convert(t, "#!name=t\n[Argument]\nBroken=input\n[Script]\nhttp-request ^https://a script-path=https://e.com/a.js\n")
	if broken.Excluded() {
		t.Errorf("参数缺默认值是源文件问题，不该算排除：%v", broken.Notes)
	}
}

// TestScriptAttributeAlias 锁住一次真实事故：可莉 185 处脚本用 requires_body 写，
// 不归一就会被当作未知属性丢掉——Surge 那边脚本读不到响应体，去广告静默失效。
func TestScriptAttributeAlias(t *testing.T) {
	result := convert(t, `#!name=t
[Script]
response if ${url} ~= /^https://////a//.com///x/i then script("https://e.com/a.js") with tag="去广告", requires_body=true, binary_body_mode=true
`)
	line := section(result, "Script")[0]
	for _, want := range []string{"requires-body=true", "binary-body-mode=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("下划线属性没有换成连字符，缺 %q：\n%s", want, line)
		}
	}
	if strings.Contains(line, "_") && strings.Contains(line, "requires_body") {
		t.Errorf("原始下划线写法还在：\n%s", line)
	}
	if got := codes(result.Adapted()); len(got) != 2 {
		t.Errorf("每条归一都该记一次 adapted，得到 %v", got)
	}
	if got := codes(result.NeedsReview()); len(got) != 0 {
		t.Errorf("属性名归一是等价改写，不该进待确认：%v", got)
	}
}

// TestScriptObjectArgs 覆盖 script("url", {${A}, ${B}}) 这种带参数清单的写法。
// 旧实现会把清单当成路径的一部分，生成 script-path="url", {…} 这种导不进去的行。
func TestScriptObjectArgs(t *testing.T) {
	t.Run("声明过的参数转成 argument JSON", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
tab=switch, false, true
useractivity=switch, true, false
[Script]
response if ${url} ~= /^https://////a//.com///x/i then script("https://e.com/a.js", {${tab}, ${useractivity}}) with tag="去广告", requires_body=true
`)
		line := section(result, "Script")[0]
		want := `去广告 = type=http-response, pattern=(?i:^https://////a//.com///x), ` +
			`script-path=https://e.com/a.js, requires-body=true, timeout=10, ` +
			`argument="{\"tab\":{{{tab}}},\"useractivity\":{{{useractivity}}}}"`
		if line != want {
			t.Fatalf("产物不符：\n得到 %s\n期望 %s", line, want)
		}
		// 参数只出现在清单里，也要留在 #!arguments，否则 Surge 里就没人能改它了
		if args := result.Sections["Script"]; len(args) == 0 {
			t.Fatal("没有 Script 段")
		}
	})

	t.Run("点号参数名：键保持原样，值用下划线名", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Weather.Provider=select, "ColorfulClouds", "WeatherKit"
[Script]
response if ${url} ~= /^https://////a//.com///x/i then script("https://e.com/a.js", {${Weather.Provider}}) with tag="天气"
`)
		line := section(result, "Script")[0]
		// 脚本是按 "Weather.Provider" 这个键取值的，键被改掉设置就失效了
		if !strings.Contains(line, `{\"Weather.Provider\":{{{Weather_Provider}}}}`) {
			t.Errorf("点号参数名处理不对：\n%s", line)
		}
	})

	t.Run("没声明的参数按 null 传并进待确认", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
response if ${url} ~= /^https://////a//.com///x/i then script("https://e.com/a.js", {${ghost}}) with tag="去广告"
`)
		line := section(result, "Script")[0]
		if !strings.Contains(line, `{\"ghost\":null}`) {
			t.Errorf("未声明参数应传 null：\n%s", line)
		}
		if got := codes(result.NeedsReview()); len(got) != 1 || got[0] != "script-argument" {
			t.Errorf("未声明参数要进待确认，得到 %v", got)
		}
	})
}

// TestReviewAndAdaptedSeparated 确认两种诊断不会混在一个桶里：
// 「替你改好了」和「你得看一眼」对使用者的意义完全不同。
func TestReviewAndAdaptedSeparated(t *testing.T) {
	result := convert(t, `#!name=t
[Script]
generic then script("https://e.com/n.js") with tag="链路检测"
`)
	if got := codes(result.NeedsReview()); len(got) != 1 || got[0] != "script-generic" {
		t.Errorf("generic 脚本应进待确认，得到 %v", got)
	}

	skipped := convert(t, `#!name=t
[Rule]
DOMAIN, a.com, PROXY
[Script]
response if ${url} ~= /^https://////a//.com///x/i then script("https://e.com/a.js") with tag="去广告"
`)
	if skipped.Skipped != 1 {
		t.Errorf("应记 1 行未能转换，得到 %d", skipped.Skipped)
	}
	summary := Summarize(skipped.NeedsReview())
	if len(summary) != 0 {
		t.Errorf("逐行注释的诊断应被压成一句计数，不该单独列出：%v", summary)
	}
}

// ---------------------------------------------------------------------------
// 与上游 loon_to_surge 对齐的行为
// ---------------------------------------------------------------------------

func TestBodyRewriteSyntax(t *testing.T) {
	t.Run("V2 无 flag 时显式关掉 Surge 的按行锚定", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.body.replace(/foo/, "bar")
`)
		equalStrings(t, section(result, "Body Rewrite"), []string{
			`http-response ^https:\/\/a\.com\/ (?-ims)foo bar`,
		})
	})

	t.Run("flags 与 Surge 默认值互补", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.body.replace(/foo/m, "bar")
response if ${url} ~= /^https:\/\/a\.com\// then response.body.replace(/foo/is, "bar")
`)
		equalStrings(t, section(result, "Body Rewrite"), []string{
			`http-response ^https:\/\/a\.com\/ (?m-is)foo bar`,
			`http-response ^https:\/\/a\.com\/ (?is-m)foo bar`,
		})
	})

	t.Run("不支持的正则 flag 不硬转", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.body.replace(/foo/x, "bar")
`)
		if len(section(result, "Body Rewrite")) != 0 {
			t.Errorf("带 /x 的正则应拒绝：%v", section(result, "Body Rewrite"))
		}
	})

	t.Run("旧语法同样是 http-response 三段式", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
^https:\/\/a\.com\/ response-body-replace-regex foo bar
`)
		equalStrings(t, section(result, "Body Rewrite"), []string{
			`http-response ^https:\/\/a\.com\/ (?-m)foo bar`,
		})
	})
}

func TestJQSerialization(t *testing.T) {
	t.Run("行尾注释被删掉而不是吞掉后续代码", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.json.jq(".a = 1 # 去掉广告")
`)
		equalStrings(t, section(result, "Body Rewrite"), []string{
			`http-response-jq ^https:\/\/a\.com\/ '.a = 1'`,
		})
	})

	t.Run("字符串里的 # 不算注释", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.json.jq("\"#tag\" | ltrimstr(\"#\")")
`)
		lines := section(result, "Body Rewrite")
		if len(lines) != 1 || !strings.Contains(lines[0], `"#tag"`) || !strings.Contains(lines[0], `"#"`) {
			t.Errorf("字符串里的 # 被当成注释了：%v", lines)
		}
	})

	t.Run("$__loc__ 依赖行号，直接拒绝", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\// then response.json.jq("$__loc__")
`)
		if len(section(result, "Body Rewrite")) != 0 {
			t.Errorf("$__loc__ 不该被转换：%v", section(result, "Body Rewrite"))
		}
		if !strings.Contains(strings.Join(codes(result.Warnings()), ","), "rewrite-jq") {
			t.Errorf("应记录 rewrite-jq 诊断：%v", codes(result.Warnings()))
		}
	})
}

func TestMockLocal(t *testing.T) {
	t.Run("内联 JSON mock 走 base64 保住引号", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\/cf/ then response.body.mock("{\"ok\":true}")
`)
		equalStrings(t, section(result, "Map Local"), []string{
			`^https:\/\/a\.com\/cf data="eyJvayI6dHJ1ZX0=" data-type=base64 status-code=200 header="Content-Type:application/json"`,
		})
	})

	t.Run("远程 mock_file 转 data-type=file", func(t *testing.T) {
		result := convert(t, `#!name=t
[Rewrite]
response if ${url} ~= /^https:\/\/a\.com\/cf/ then response.body.mock_file("https://e.com/a.json")
`)
		equalStrings(t, section(result, "Map Local"), []string{
			`^https:\/\/a\.com\/cf data-type=file data="https://e.com/a.json" status-code=200`,
		})
	})
}

func TestScriptRobustness(t *testing.T) {
	t.Run("enable 与入参共用同一参数时不改默认值", func(t *testing.T) {
		result := convert(t, `#!name=t
[Argument]
Capture=switch, false
[Script]
cron "0 8 * * *" script-path=https://e.com/c.js, tag=C, enable={Capture}, argument={"day": {Capture}}
`)
		lines := section(result, "Script")
		if len(lines) != 1 {
			t.Fatalf("应产出 1 条脚本：%v", lines)
		}
		if strings.HasPrefix(lines[0], "{{{Capture}}}") {
			t.Errorf("共用参数不该变成行前缀：%s", lines[0])
		}
		if strings.HasPrefix(lines[0], "# ") {
			t.Logf("按默认值静态注释：%s", lines[0])
		}
		if !strings.Contains(result.Text, "#!arguments=Capture:false") {
			t.Errorf("默认值被改写了：\n%s", result.Text)
		}
		if !strings.Contains(strings.Join(codes(result.NeedsReview()), ","), "script-enable-shared") {
			t.Errorf("应记录 script-enable-shared：%v", codes(result.NeedsReview()))
		}
	})

	t.Run("cron 段数不对直接拒绝", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
cron "0 8 * *" script-path=https://e.com/c.js, tag=C
`)
		if len(section(result, "Script")) != 0 {
			t.Errorf("4 段 cron 应拒绝：%v", section(result, "Script"))
		}
	})

	t.Run("同一属性两个不同值时不猜", func(t *testing.T) {
		result := convert(t, `#!name=t
[Script]
http-request ^https://a\.com script-path=https://e.com/a.js, tag=T, timeout=5, timeout=15
`)
		if len(section(result, "Script")) != 0 {
			t.Errorf("冲突属性应拒绝：%v", section(result, "Script"))
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
