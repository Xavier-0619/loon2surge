package lpx

// 一次转换的共享上下文：各 section 处理器都往这里写产出、诊断和版本门槛。

import "strings"

// Ctx 是一次转换的共享上下文。
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
