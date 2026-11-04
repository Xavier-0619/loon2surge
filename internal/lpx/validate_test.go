package lpx

import (
	"strings"
	"testing"
)

// 自检是产物的最后一道闸：它不看转换器的意图，只看文件本身。
// 这里用"故意写坏"的模块确认它真的会拦。

func TestValidateClean(t *testing.T) {
	text := `#!name=示例
#!category=插件转换
#!requirement=CORE_VERSION>=6008000
#!arguments=Capture:#,Cron:0 8 * * *

[General]
always-real-ip = %APPEND% a.com

[Rule]
DOMAIN-SUFFIX,ads.example.com,REJECT,extended-matching,pre-matching

[Body Rewrite]
http-response-jq ^https:\/\/a\.com\/ '.a = 1'

[Map Local]
^https:\/\/a\.com\/x data-type=text data="{}" status-code=200

[Script]
Capture = type=cron, cronexp="{{{Cron}}}", script-path=https://e.com/a.js, timeout=300

[MITM]
hostname = %APPEND% a.com
`
	if problems := Validate(text); len(problems) != 0 {
		t.Fatalf("干净的模块不该有问题：%v", problems)
	}
}

func TestValidateCatches(t *testing.T) {
	cases := []struct {
		name string
		text string
		code string
	}{
		{
			"残留 enable",
			"#!name=t\n[Script]\nA = type=cron, cronexp=\"0 8 * * *\", script-path=https://e.com/a.js, enable={A}\n",
			"residual-enable",
		},
		{
			"裸占位符",
			"#!name=t\n[Script]\nA = type=cron, cronexp=\"{Cron}\", script-path=https://e.com/a.js\n",
			"residual-placeholder",
		},
		{
			"%Arg% 残留",
			"#!name=t\n[Script]\nA = type=cron, cronexp=\"%Cron%\", script-path=https://e.com/a.js\n",
			"residual-placeholder",
		},
		{
			"模块规则用了 PROXY",
			"#!name=t\n[Rule]\nDOMAIN,a.com,PROXY\n",
			"rule-policy",
		},
		{
			"Body Rewrite 用了不存在的四种写法",
			"#!name=t\n[Body Rewrite]\nhttp-response-replace-regex ^https://a\\.com foo bar\n",
			"body-rewrite-head",
		},
		{
			"Map Local 状态码越界",
			"#!name=t\n[Map Local]\n^https://a\\.com data-type=text data=\"\" status-code=100\n",
			"map-local-status",
		},
		{
			"Script 缺 type",
			"#!name=t\n[Script]\nA = cronexp=\"0 8 * * *\", script-path=https://e.com/a.js\n",
			"script-type",
		},
		{
			"Section 顺序颠倒",
			"#!name=t\n[Script]\nA = type=cron, cronexp=\"0 8 * * *\", script-path=https://e.com/a.js\n[MITM]\nhostname = a.com\n[Rule]\nDOMAIN,a.com,REJECT\n",
			"section-order",
		},
		{
			"参数重名",
			"#!name=t\n#!arguments=A:1,A:2\n[Script]\nX = type=cron, cronexp=\"0 8 * * *\", script-path=https://e.com/a.js\n",
			"argument-duplicate",
		},
		{
			"缺 name",
			"#!category=x\n[Rule]\nDOMAIN,a.com,REJECT\n",
			"meta-name",
		},
		{
			"改 Content-Length",
			"#!name=t\n[Header Rewrite]\nhttp-request ^https://a\\.com header-add Content-Length 1\n",
			"header-framing",
		},
		{
			"requirement 声明低于内容",
			"#!name=t\n#!requirement=CORE_VERSION>=20\n[Map Local]\n^https://a\\.com data-type=text data=\"\" status-code=200\n",
			"requirement-low",
		},
		{
			"缺 requirement 但内容需要",
			"#!name=t\n[Body Rewrite]\nhttp-response ^https://a\\.com (?-ims)x y\n",
			"requirement-low",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			problems := Validate(test.text)
			for _, problem := range problems {
				if problem.Code == test.code {
					return
				}
			}
			t.Fatalf("没有发现 %s：%v", test.code, problems)
		})
	}
}

// 生成出来的模块必须自己能过自检；这条是"转换器有 bug 时把 bug 挡在发布前"的底线。
func TestValidateOwnOutput(t *testing.T) {
	for _, path := range []string{"testdata/zhihu.lpx"} {
		result, err := ConvertFile(path, defaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range Validate(result.Text) {
			t.Errorf("%s 产物自检未过：%s", path, problem)
		}
		if strings.Contains(result.Text, "http-response-replace-regex") {
			t.Errorf("%s 还在用不存在的 Body Rewrite 写法", path)
		}
	}
}
