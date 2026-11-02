// Package lpx 把 Loon 插件（.lpx）转换成 Surge 模块（.sgmodule）。
//
// 设计原则：宁可少转一行并明确报出来，也不生成能导入但行为不对的模块。
// 无法安全映射的语法一律抛 Unsupported，由 Converter 注释保留原文。
//
// 文件分工：
//
//	parse.go     .lpx 顶层结构解析
//	syntax.go    只在"顶层"动手的切分与引号处理
//	convert.go   调度 + 头部渲染 + 组装
//	rule.go      [Rule]（含 AND/OR/NOT 逻辑规则）
//	rewrite.go   [Rewrite] → URL/Header/Body Rewrite 与 Map Local
//	script.go    [Script]
//	jq.go        内联 JQ 表达式的压行与转义
//	validate.go  产物自检
package lpx

import (
	"fmt"
	"strconv"
	"strings"
)

// Surge 模块要求的版本门槛，取所有命中项的最大值。
const (
	ReqBase   = 20      // 模块参数 / Map Local / Body Rewrite
	ReqModern = 6008000 // JQ / pre-matching / 引号参数
	ReqLatest = 6010000 // IP Rewrite / tun 路由 / DOMAIN-WILDCARD / 进程规则
)

// SectionOrder 是模块里各 section 的固定输出顺序。
var SectionOrder = []string{
	"General",
	"IP Rewrite",
	"Rule",
	"URL Rewrite",
	"Header Rewrite",
	"Body Rewrite",
	"Map Local",
	"Script",
	"MITM",
}

// MetaOrder 是模块头部 #! 字段的顺序，其余字段排在后面。
var MetaOrder = []string{
	"name", "desc", "author", "icon", "category", "system",
	"openUrl", "open", "tag", "homepage", "date", "requirement", "arguments",
}

// Unsupported 表示当前行无法安全映射到 Surge。
type Unsupported struct {
	Code    string
	Message string
}

func (e *Unsupported) Error() string { return e.Message }

func unsupported(code, format string, args ...any) *Unsupported {
	return &Unsupported{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Emit 是一条待写入的产出：目标 section + 行内容。
type Emit struct {
	Section string
	Text    string
}

// 诊断的处理等级。区分这两种，是为了让"我替你改了什么"和"你得自己看一眼"
// 分别可查：前者说明转换器做了等价改写，后者说明有东西没能自动处理。
const (
	// ActionAdapted 表示转换器做了等价改写（属性名归一、脚本重名加后缀…），
	// 模块可以直接用，只是和源文件长得不完全一样。
	ActionAdapted = "adapted"
	// ActionReview 表示这里留有需要人确认的地方（原文注释、运行时语义差异…）。
	ActionReview = "review"
)

// Note 是一条转换诊断；Fatal 表示该模块无法安全产出。
type Note struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Fatal   bool   `json:"fatal,omitempty"`
	// Action 取 ActionAdapted / ActionReview，空表示只是记录，不需要人管。
	Action string `json:"action,omitempty"`
	// Skipped 表示这一行没能转换、以注释形式留在了文件末尾。
	Skipped bool `json:"skipped,omitempty"`
}

func (n Note) String() string {
	if n.Line > 0 {
		return fmt.Sprintf("[%s] L%d: %s", n.Code, n.Line, n.Message)
	}
	return fmt.Sprintf("[%s] %s", n.Code, n.Message)
}

// Line 是源文件里的一行，带行号便于定位诊断。
type Line struct {
	No   int
	Text string
}

// Plugin 是一个 .lpx 的原始结构。
type Plugin struct {
	Meta     map[string]string
	MetaKeys []string // 保持文件里的出现顺序，输出才稳定
	Sections map[string][]Line
	Order    []string // section 的出现顺序
}

// Lines 返回某个 section 的所有行（section 名不区分大小写与写法）。
func (p *Plugin) Lines(section string) []Line { return p.Sections[NormalizeSection(section)] }

// Options 控制输出形态。
type Options struct {
	Category string // #!category
	Banner   bool   // 是否写"由 loon2surge 转换"的说明
}

// Result 是一次转换的完整结果。
type Result struct {
	Text     string
	Name     string
	Notes    []Note
	Sections map[string][]string
	Order    []string
	// Skipped 是无法转换、以注释形式留在文件末尾的行数。
	Skipped int
}

// Fatal 返回致命诊断。
func (r Result) Fatal() []Note { return r.filter(func(note Note) bool { return note.Fatal }) }

// Warnings 返回非致命诊断。
func (r Result) Warnings() []Note { return r.filter(func(note Note) bool { return !note.Fatal }) }

// Adapted 返回"转换器做过等价改写"的诊断。
func (r Result) Adapted() []Note {
	return r.filter(func(note Note) bool { return note.Action == ActionAdapted })
}

// NeedsReview 返回"需要人看一眼"的诊断。
func (r Result) NeedsReview() []Note {
	return r.filter(func(note Note) bool { return note.Action == ActionReview })
}

func (r Result) filter(keep func(Note) bool) []Note {
	var out []Note
	for _, note := range r.Notes {
		if keep(note) {
			out = append(out, note)
		}
	}
	return out
}

// OK 表示模块可以安全落盘。
func (r Result) OK() bool { return len(r.Fatal()) == 0 && len(r.Sections) > 0 }

// Excluded 表示"这个插件在 Surge 里根本表达不出来"，而不是源文件坏了。
//
// 典型是 [Rule] 里写 PROXY：Surge 模块的规则只允许 DIRECT / REJECT / REJECT-TINYGIF，
// 这类模块本质是"给代理用的规则集"，硬转出来会少掉关键规则、看着能装其实不对。
// 调用方要把它和真正的转换失败分开统计——前者是正常结论，后者才要人去看。
func (r Result) Excluded() bool {
	fatal := r.Fatal()
	if len(fatal) == 0 {
		return false
	}
	for _, note := range fatal {
		if note.Code != "module-excluded" {
			return false
		}
	}
	return true
}

// ExcludeReason 给出一句话的排除理由，优先解释"哪一行没被 Surge 接受"。
func (r Result) ExcludeReason() string {
	for _, note := range r.Warnings() {
		if note.Code == "rule-policy" {
			return "模块规则只能用 DIRECT/REJECT，原文用的是 " + policyOf(note.Message)
		}
	}
	if warnings := r.Warnings(); len(warnings) > 0 {
		return warnings[0].Message
	}
	return "没有任何可转换的内容"
}

// policyOf 从 "Surge 模块 [Rule] 不允许 PROXY 策略" 里取出策略名。
func policyOf(message string) string {
	_, rest, found := strings.Cut(message, "不允许 ")
	if !found {
		return "策略"
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}

// CountByCode 按诊断码计数，报告里用。
func CountByCode(notes []Note) map[string]int {
	counts := make(map[string]int, len(notes))
	for _, note := range notes {
		counts[note.Code]++
	}
	return counts
}

// 诊断码 → 一句话说明。提交信息和 Job Summary 都用这套词，
// 免得同一件事在两处叫两个名字。
var noteLabels = map[string]string{
	"script-attribute-alias": "属性名归一（下划线转连字符）",
	"script-name-shared":     "重名脚本自动加后缀",
	"script-object-adapted":  "脚本参数清单转成 argument JSON",
	"script-argument":        "参数未声明，按 null 传",
	"script-enable":          "enable 转为模块参数开关",
	"script-generic":         "generic 脚本没有节点上下文",
	"script-attribute":       "有脚本属性被丢弃",
	"script-disabled":        "脚本默认关闭，已注释",
	"mitm-missing":           "缺 [MITM] hostname",
	"section-unsupported":    "有 section 整段注释",
	"general-unsupported":    "General 项未核验",
}

// Summarize 把同一 code 的诊断压成一行，按首次出现的顺序排列。
//
// 只给"需要人管"的诊断用：同一份插件里 `requires_body` 可能出现六次，
// 逐条列出来只会把提交信息淹掉。
func Summarize(notes []Note) []string {
	var order []string
	counts := make(map[string]int, len(notes))
	for _, note := range notes {
		if note.Skipped {
			continue // 逐行的注释诊断会压成一句"N 行无法自动转换"，不逐条列
		}
		if _, seen := counts[note.Code]; !seen {
			order = append(order, note.Code)
		}
		counts[note.Code]++
	}

	lines := make([]string, 0, len(order))
	for _, code := range order {
		label, ok := noteLabels[code]
		if !ok {
			label = code
		}
		if counts[code] > 1 {
			label += " ×" + strconv.Itoa(counts[code])
		}
		lines = append(lines, label)
	}
	return lines
}
