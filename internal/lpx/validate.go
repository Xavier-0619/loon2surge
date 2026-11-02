package lpx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 产物自检：把已经生成好的 .sgmodule 当成"别人写的文件"再读一遍。
//
// 转换器里的诊断回答的是"我替你改了什么"，这里回答的是"产出的文件本身合不合
// Surge 的语法"。两件事分开查才有意义：转换器自己写错行时，它自己的诊断是看不见的。
//
// 这里只查"能静态判定、且一旦错了模块就是废的"项目，不猜运行时行为。

// Problem 是一条产物问题；Fatal 表示这份模块不能发布。
type Problem struct {
	Line    int    `json:"line"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	return fmt.Sprintf("L%d [%s] %s", p.Line, p.Code, p.Message)
}

// 产物里允许出现的 section。顺序由 SectionOrder 决定，不在这里重复。
var allowedSections = map[string]bool{
	"General": true, "IP Rewrite": true, "Rule": true,
	"URL Rewrite": true, "Header Rewrite": true, "Body Rewrite": true,
	"Map Local": true, "Panel": true, "Script": true, "MITM": true,
}

var (
	// 裸的 Loon 占位符：`{Name}` 或 `${Name}`，但 {{...}} 是 Surge 语法不算。
	loonPlaceholder  = regexp.MustCompile(`(^|[^{$])\{[A-Za-z_][A-Za-z0-9_]*\}`)
	legacyPercentArg = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
	// 参数表里每一项是 名字:默认值，默认值可用双引号包住。
	argumentItem = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*):`)
	statusCode   = regexp.MustCompile(`\bstatus-code=(\d+)`)
	dataType     = regexp.MustCompile(`\bdata-type=([a-z-]+)`)
	bodyHead     = regexp.MustCompile(`^http-(request|response)(-jq)?\s`)
	scriptType   = regexp.MustCompile(`\btype=(http-request|http-response|cron|generic|event|dns)\b`)
	requirement  = regexp.MustCompile(`^#!requirement=CORE_VERSION>=(\d+)$`)
)

// hasLegacyPercentArg 找 `%Arg%` 形式的老占位符。
//
// `%APPEND%` / `%INSERT%` 是 Surge 自己的列表追加标记，长得一样但不是占位符。
func hasLegacyPercentArg(line string) bool {
	for _, match := range legacyPercentArg.FindAllString(line, -1) {
		if strings.EqualFold(match, "%APPEND%") || strings.EqualFold(match, "%INSERT%") {
			continue
		}
		return true
	}
	return false
}

// Validate 检查一份模块文本，返回所有问题（按行号排序）。
func Validate(text string) []Problem {
	var problems []Problem
	section := ""
	seenSections := map[string]bool{}
	argsSeen := map[string]int{}
	lastRank := -1
	sawName := false

	for index, raw := range strings.Split(text, "\n") {
		lineNo := index + 1
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#!") {
			key, value, _ := strings.Cut(line[2:], "=")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			switch key {
			case "name":
				sawName = value != ""
			case "arguments":
				problems = append(problems, checkArguments(lineNo, value, argsSeen)...)
			case "requirement":
				match := requirement.FindStringSubmatch(line)
				if match == nil {
					problems = append(problems, Problem{lineNo, "meta-requirement",
						"requirement 不是 CORE_VERSION>=数字：" + line})
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if !allowedSections[section] {
				problems = append(problems, Problem{lineNo, "section-unknown",
					"产物里出现了不认识的 section：[" + section + "]"})
			}
			if seenSections[section] {
				problems = append(problems, Problem{lineNo, "section-repeat",
					"section 重复出现：[" + section + "]"})
			}
			seenSections[section] = true
			if rank := sectionRank(section); rank >= 0 {
				if rank < lastRank {
					problems = append(problems, Problem{lineNo, "section-order",
						"section 顺序不对：[" + section + "] 应排在前面"})
				}
				lastRank = rank
			}
			continue
		}

		problems = append(problems, checkLine(lineNo, section, line)...)
	}

	if !sawName {
		problems = append(problems, Problem{0, "meta-name", "缺少 #!name，Surge 会拒绝这个模块"})
	}
	return problems
}

func sectionRank(name string) int {
	for index, want := range SectionOrder {
		if want == name {
			return index
		}
	}
	// Panel 不在 SectionOrder 里，和 Script 放一起。
	if name == "Panel" {
		return len(SectionOrder) - 1
	}
	return -1
}

func checkArguments(lineNo int, value string, seen map[string]int) []Problem {
	var problems []Problem
	for _, item := range splitTop(value, ",") {
		if item == "" {
			continue
		}
		match := argumentItem.FindStringSubmatch(item)
		if match == nil {
			problems = append(problems, Problem{lineNo, "argument-name",
				"参数表项不是 名字:默认值 的形式：" + item})
			continue
		}
		name := match[1]
		if first, ok := seen[name]; ok {
			problems = append(problems, Problem{lineNo, "argument-duplicate",
				fmt.Sprintf("参数 %s 重复声明（第一次在第 %d 行）", name, first)})
			continue
		}
		seen[name] = lineNo
	}
	return problems
}

func checkLine(lineNo int, section, line string) []Problem {
	var problems []Problem
	add := func(code, message string) {
		problems = append(problems, Problem{lineNo, code, message})
	}

	// 这几条与 section 无关，任何一行出现都算残留。
	switch {
	case strings.Contains(line, "enable="):
		add("residual-enable", "残留 Loon 的 enable= 字段："+shorten(line))
	case strings.Contains(line, "enabled?="):
		add("residual-enable", "残留 Loon 的 enabled?= 字段："+shorten(line))
	case hasLegacyPercentArg(line) && section != "Rule":
		// 规则行里的 `%E4%B8%AD` 是 URL 百分号编码，不是占位符
		add("residual-placeholder", "残留 %Arg% 形式的老占位符："+shorten(line))
	case loonPlaceholder.MatchString(line):
		add("residual-placeholder", "残留裸 {Arg} 占位符，Surge 要用 {{{Arg}}}："+shorten(line))
	case strings.Contains(line, "{{{"):
		if !strings.Contains(line, "}}}") {
			add("broken-placeholder", "{{{ 没有配对的 }}}："+shorten(line))
		}
	}

	switch section {
	case "Rule":
		problems = append(problems, checkRuleLine(lineNo, line)...)
	case "Map Local":
		if match := statusCode.FindStringSubmatch(line); match != nil {
			code, _ := strconv.Atoi(match[1])
			if code < 200 || code > 999 {
				add("map-local-status", fmt.Sprintf("Map Local 的 status-code=%d 超出 200-999", code))
			}
		}
		if match := dataType.FindStringSubmatch(line); match != nil {
			switch match[1] {
			case "file", "text", "tiny-gif", "base64":
			default:
				add("map-local-type", "Map Local 的 data-type 不认识："+match[1])
			}
		}
	case "Body Rewrite":
		if !bodyHead.MatchString(line) {
			add("body-rewrite-head", "Body Rewrite 行必须以 http-request/http-response 开头："+shorten(line))
		}
	case "Script":
		if !scriptType.MatchString(line) {
			add("script-type", "Script 行缺少合法的 type=："+shorten(line))
		}
	case "Header Rewrite":
		if !strings.HasPrefix(line, "http-request ") && !strings.HasPrefix(line, "http-response ") {
			add("header-rewrite-head", "Header Rewrite 行缺少 http-request/http-response："+shorten(line))
		}
		if strings.Contains(line, "header-add Content-Length") ||
			strings.Contains(line, "header-add Transfer-Encoding") {
			add("header-framing", "改了 Content-Length/Transfer-Encoding，Surge 会拒绝这次改写："+shorten(line))
		}
	case "MITM":
		if !strings.HasPrefix(strings.ToLower(line), "hostname") {
			add("mitm-entry", "MITM 只支持 hostname："+shorten(line))
		}
	}
	return problems
}

// checkRuleLine 只查"策略是否在模块允许范围内"和字段数。
//
// 规则类型本身由转换器把关；这里再查一遍是因为策略是成品废不废的关键：
// Surge 模块只接受 DIRECT / REJECT / REJECT-TINYGIF。
func checkRuleLine(lineNo int, line string) []Problem {
	var problems []Problem
	parts := splitTop(line, ",")
	if len(parts) < 3 {
		return []Problem{{lineNo, "rule-shape", "规则字段不足三个：" + shorten(line)}}
	}
	kind := strings.ToUpper(strings.TrimSpace(parts[0]))
	if kind == "AND" || kind == "OR" || kind == "NOT" {
		return nil // 逻辑规则的策略在末尾，转换器已校验
	}
	policy := strings.ToUpper(strings.TrimSpace(parts[2]))
	switch policy {
	case "DIRECT", "REJECT", "REJECT-TINYGIF":
	default:
		problems = append(problems, Problem{lineNo, "rule-policy",
			"模块 [Rule] 不允许 " + policy + " 策略：" + shorten(line)})
	}
	return problems
}

// FatalProblems 需要发布阻断的那部分（目前全部都是）。
func FatalProblems(problems []Problem) []Problem { return problems }
