package lpx

// Loon 模块参数 → Surge 模块参数。
//
// Loon 写 `{Name}` 或 `${Name}`，Surge 要 `{{{Name}}}`；`#!arguments` 的格式两边也不同。

import "strings"

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
		if strings.ContainsRune(value, ',') || strings.HasPrefix(value, `"`) {
			value = quoteValue(value)
		}
		pairs = append(pairs, name+":"+value)
	}
	return strings.Join(pairs, ",")
}
