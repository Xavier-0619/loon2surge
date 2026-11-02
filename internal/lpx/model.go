// Package lpx 把 Loon 插件（.lpx）转换成 Surge 模块（.sgmodule）。
//
// 设计原则：宁可少转一行并明确报出来，也不生成能导入但行为不对的模块。
// 无法安全映射的语法一律抛 Unsupported，由 Converter 注释保留原文。
package lpx

import (
	"fmt"
	"regexp"
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

var sectionSpace = regexp.MustCompile(`[\s_]+`)

// NormalizeSection 把 [MitM]、[IP-Rewrite]、[ip rewrite] 统一成内部 key。
func NormalizeSection(title string) string {
	return strings.ToUpper(sectionSpace.ReplaceAllString(strings.TrimSpace(title), "-"))
}

// Options 控制输出形态。
type Options struct {
	Category string // #!category
	Banner   bool   // 是否写"由 loon2surge 转换"的说明
}

// DefaultOptions 返回命令行默认值。
func DefaultOptions() Options { return Options{Category: "插件转换", Banner: true} }

// Args 是 Loon 模块参数，同时负责把 {Name} 换成 Surge 的 {{{Name}}}。
type Args struct {
	Defaults map[string]string
	Order    []string // 参数声明顺序，决定 #!arguments 的顺序
	Used     map[string]bool
}

func newArgs() *Args {
	return &Args{Defaults: map[string]string{}, Used: map[string]bool{}}
}

// Sub 把正文里已声明的参数占位符改成 Surge 语法。
func (a *Args) Sub(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(text); {
		// ${Name}
		if text[i] == '$' && i+1 < len(text) && text[i+1] == '{' {
			if end, name, ok := scanName(text, i+2, false); ok {
				if _, declared := a.Defaults[name]; declared {
					a.Used[name] = true
					b.WriteString("{{{" + name + "}}}")
					i = end
					continue
				}
			}
		}
		// {Name}，但不能是 ${Name} 或 {{{Name}}}
		if text[i] == '{' && (i == 0 || (text[i-1] != '$' && text[i-1] != '{')) {
			if end, name, ok := scanName(text, i+1, true); ok {
				if _, declared := a.Defaults[name]; declared {
					a.Used[name] = true
					b.WriteString("{{{" + name + "}}}")
					i = end
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// scanName 读取 `Name}`，返回占位符之后的位置、参数名、是否匹配。
func scanName(text string, start int, rejectDouble bool) (int, string, bool) {
	i := start
	for i < len(text) && isNameChar(text[i]) {
		i++
	}
	if i == start || i >= len(text) || text[i] != '}' {
		return 0, "", false
	}
	if rejectDouble && i+1 < len(text) && text[i+1] == '}' {
		return 0, "", false // 已经是 {{{Name}}}，不要二次转换
	}
	return i + 1, text[start:i], true
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Unused 返回声明了但正文没引用的参数。
func (a *Args) Unused() []string {
	var out []string
	for _, name := range a.Order {
		if !a.Used[name] {
			out = append(out, name)
		}
	}
	return out
}

// Table 渲染 #!arguments 的值。
//
// 只有含逗号才加引号：逗号是参数之间的分隔符，`Cron:0 0,12 * * *` 不引起来会被拆成两个参数。
// 含空格的值（cron 表达式）不能加引号，引号会被当成值的一部分传给脚本。
func (a *Args) Table() string {
	pairs := make([]string, 0, len(a.Order))
	for _, name := range a.Order {
		value := a.Defaults[name]
		if strings.Contains(value, ",") || strings.HasPrefix(value, `"`) {
			value = quoteValue(value)
		}
		pairs = append(pairs, name+":"+value)
	}
	return strings.Join(pairs, ",")
}

// Ctx 是一次转换的共享上下文：各 section 处理器都往这里写产出、诊断和版本门槛。
type Ctx struct {
	args        *Args
	options     Options
	notes       []Note
	requirement int
	emitted     map[string][]string
	emitOrder   []string
	names       map[string]bool // 已占用的 Script 名称
	// platform 是插件 #!system 的原文。进程规则这类"只在某个平台成立"的语法
	// 要靠它判断能不能转，光看规则本身看不出平台。
	platform string
}

func newCtx(options Options) *Ctx {
	if options.Category == "" {
		options.Category = "插件转换"
	}
	return &Ctx{
		args:        newArgs(),
		options:     options,
		requirement: ReqBase,
		emitted:     map[string][]string{},
		names:       map[string]bool{},
	}
}

// Warn 记一条不致命的诊断。
func (c *Ctx) Warn(code, message string, line int) {
	c.notes = append(c.notes, Note{Code: code, Message: message, Line: line})
}

// Adapt 记一条"转换器替你改写过了"的诊断。
func (c *Ctx) Adapt(code, message string, line int) {
	c.notes = append(c.notes, Note{Code: code, Message: message, Line: line, Action: ActionAdapted})
}

// Review 记一条"这里需要人看一眼"的诊断。
func (c *Ctx) Review(code, message string, line int) {
	c.notes = append(c.notes, Note{Code: code, Message: message, Line: line, Action: ActionReview})
}

// ReviewSkipped 记一条"这一行没能转换"，条文会以注释保留在文件末尾。
func (c *Ctx) ReviewSkipped(code, message string, line int) {
	c.notes = append(c.notes, Note{
		Code: code, Message: message, Line: line, Action: ActionReview, Skipped: true,
	})
}

// Fail 记一条致命诊断：模块不会产出。
func (c *Ctx) Fail(code, message string, line int) {
	c.notes = append(c.notes, Note{Code: code, Message: message, Line: line, Fatal: true})
}

// Need 抬高版本门槛。
func (c *Ctx) Need(version int) {
	if version > c.requirement {
		c.requirement = version
	}
}

// Emit 追加一行产出，空行会被忽略，占位符在这里统一改写。
func (c *Ctx) Emit(section string, lines ...string) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, seen := c.emitted[section]; !seen {
			c.emitOrder = append(c.emitOrder, section)
		}
		c.emitted[section] = append(c.emitted[section], c.args.Sub(line))
	}
}

// sections 按固定顺序返回非空 section。
func (c *Ctx) sections() (map[string][]string, []string) {
	sections := make(map[string][]string, len(c.emitted))
	var order []string
	for _, name := range SectionOrder {
		if lines := c.emitted[name]; len(lines) > 0 {
			sections[name] = lines
			order = append(order, name)
		}
	}
	return sections, order
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
func (r Result) Fatal() []Note {
	var out []Note
	for _, note := range r.Notes {
		if note.Fatal {
			out = append(out, note)
		}
	}
	return out
}

// Warnings 返回非致命诊断。
func (r Result) Warnings() []Note {
	var out []Note
	for _, note := range r.Notes {
		if !note.Fatal {
			out = append(out, note)
		}
	}
	return out
}

// Adapted 返回"转换器做过等价改写"的诊断。
func (r Result) Adapted() []Note { return r.withAction(ActionAdapted) }

// NeedsReview 返回"需要人看一眼"的诊断。
func (r Result) NeedsReview() []Note { return r.withAction(ActionReview) }

func (r Result) withAction(action string) []Note {
	var out []Note
	for _, note := range r.Notes {
		if note.Action == action {
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
	const marker = "不允许 "
	at := strings.Index(message, marker)
	if at < 0 {
		return "策略"
	}
	rest := message[at+len(marker):]
	if end := strings.Index(rest, " "); end > 0 {
		return rest[:end]
	}
	return rest
}

// CountByCode 按诊断码计数，报告里用。
func CountByCode(notes []Note) map[string]int {
	counts := map[string]int{}
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
	counts := map[string]int{}
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
