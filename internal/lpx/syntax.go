package lpx

import (
	"fmt"
	"regexp"
	"strings"
)

// Loon 的一行里同时混着正则、引号和嵌套括号，直接 split(",") 一定切坏。
// 这里实现一套"只在顶层动手"的扫描器，所有 section 处理器共用。

// 出现在这些字符之后、且位于顶层的 `/`，视为正则字面量的开始。
// 不能包含 `:`，否则 https:// 会被误判成正则。
const regexPrefix = "(=~,"

func skipQuoted(text string, start int) int {
	quote := text[start]
	for i := start + 1; i < len(text); {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] == quote {
			return i + 1
		}
		i++
	}
	return len(text)
}

func skipRegex(text string, start int) int {
	i := start + 1
	for i < len(text) {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] == '/' {
			i++
			break
		}
		i++
	}
	for i < len(text) && isAlpha(text[i]) { // 跳过 /re/mis 的 flag
		i++
	}
	return i
}

func looksLikeRegex(text string, i int) bool {
	if text[i] != '/' {
		return false
	}
	j := i - 1
	for j >= 0 && text[j] == ' ' {
		j--
	}
	return j < 0 || strings.IndexByte(regexPrefix, text[j]) >= 0
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// topPositions 返回所有"顶层"字符的下标，跳过引号、正则和括号内部。
func topPositions(text string) []int {
	var out []int
	depth := 0
	for i := 0; i < len(text); {
		switch ch := text[i]; {
		case ch == '"' || ch == '\'':
			i = skipQuoted(text, i)
			continue
		case ch == '/' && looksLikeRegex(text, i):
			i = skipRegex(text, i)
			continue
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
		default:
			if depth == 0 {
				out = append(out, i)
			}
		}
		i++
	}
	return out
}

// findTop 返回 token 在顶层首次出现的位置，找不到返回 -1。
func findTop(text, token string, start int) int {
	for _, pos := range topPositions(text) {
		if pos < start {
			continue
		}
		if strings.HasPrefix(text[pos:], token) {
			return pos
		}
	}
	return -1
}

// splitTop 按 sep 切割，忽略引号/正则/括号内部的 sep，并去掉每段首尾空白。
func splitTop(text, sep string) []string {
	var parts []string
	previous := 0
	for _, pos := range topPositions(text) {
		if strings.HasPrefix(text[pos:], sep) {
			parts = append(parts, strings.TrimSpace(text[previous:pos]))
			previous = pos + len(sep)
		}
	}
	return append(parts, strings.TrimSpace(text[previous:]))
}

// unquote 去掉成对的首尾引号。
func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '"' || value[0] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

// quoteValue 生成 Surge 的双引号值；反斜杠与双引号需要转义。
func quoteValue(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

var bareToken = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// quoteToken 尽量不加引号；只有含空格/特殊字符时才加双引号。
func quoteToken(value string) string {
	if bareToken.MatchString(value) {
		return value
	}
	return quoteValue(value)
}

// jqLiteral 生成 Surge 的 JQ 表达式：先压成一行，再用单引号包起来。
//
// 不能只把换行换成空格：JQ 的 `#` 注释靠换行结束，压平之后注释会把后面的代码
// 一起吃掉，模块能导入但改写整段失效。所以这里按词法走一遍，见 compactJQ。
func jqLiteral(expr string) (string, error) {
	oneLine, err := compactJQ(expr)
	if err != nil {
		return "", unsupported("rewrite-jq", "%s", err.Error())
	}
	return "'" + oneLine + "'", nil
}

// loonUnescape 还原 Loon 双引号参数里的转义。
//
// Loon 把整段 JQ 写在双引号里，内部 JSON 的引号只能写成 `\"`，反斜杠写成 `\\`。
// 外壳去掉之后这些反斜杠不能留着：JQ 的词法分析会把 `\"` 当成字符串转义而不是
// 字符串边界，括号配对和注释判断全都会错位。
//
// 只认 `\"` 和 `\\` 两个。`\d` 这类正则转义在 JQ 里本来就是"反斜杠 + d"两个字符，
// 一旦解码就变成非法转义，所以原样保留。
func loonUnescape(text string) string {
	if !strings.Contains(text, `\`) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && (text[i+1] == '"' || text[i+1] == '\\') {
			b.WriteByte(text[i+1])
			i++
			continue
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

// compactJQ 把多行 JQ 程序压成单行，同时保证语义不变。
//
// 只处理与"压行"有关的三件事：
//   - `#` 注释整段删掉（字符串和插值里的 # 不算注释）；
//   - 字符串里的换行、控制字符转成 JSON 转义，单引号转成 \u0027
//     （Surge 用单引号包表达式，JQ 又没有 \' 这种写法）；
//   - `$__loc__` 直接拒绝：它取的是源文件名和行号，内联之后必然是错的。
//
// 语法本身仍交给真实的 jq/Surge 判断，这里不假装自己是解析器。
func compactJQ(program string) (string, error) {
	var out strings.Builder
	stack := []byte{0} // 0 = 代码态；'"' = 字符串；')' ']' '}' = 等配对的闭合符
	comment := false

	for i := 0; i < len(program); {
		ch := program[i]

		if comment {
			if ch == '\\' && i+1 < len(program) && (program[i+1] == '\n' || program[i+1] == '\r') {
				// JQ 1.7 与 1.8 对"注释行尾反斜杠续行"的处理不同，不猜。
				return "", unsupported("rewrite-jq", "JQ 注释使用反斜杠续行，不同 jq 版本语义不一致")
			}
			if ch == '\n' {
				comment = false
				out.WriteByte('\n')
			}
			i++
			continue
		}

		if stack[len(stack)-1] == '"' { // 字符串内部
			switch {
			case ch == '\\':
				if i+1 >= len(program) {
					return "", unsupported("rewrite-jq", "JQ 字符串以反斜杠结尾")
				}
				next := program[i+1]
				if next == '\n' || next == '\r' {
					return "", unsupported("rewrite-jq", "JQ 字符串里有非法续行")
				}
				out.WriteString(program[i : i+2])
				if next == '(' { // \( ... ) 重新进入代码，里面还能再嵌字符串
					stack = append(stack, ')')
				}
				i += 2
				continue
			case ch == '"':
				stack = stack[:len(stack)-1]
			case ch == '\'':
				out.WriteString(`\u0027`) // JQ 没有 \' 转义，只能在 JQ 层编码
				i++
				continue
			case ch < 0x20 || ch == 0x85 || ch == 0xe2:
				escaped, size := jsonEscapeRune(program[i:])
				if escaped == "" {
					out.WriteByte(ch)
					i++
					continue
				}
				out.WriteString(escaped)
				i += size
				continue
			}
			out.WriteByte(ch)
			i++
			continue
		}

		// 代码态
		switch {
		case ch == '#':
			comment = true
			i++
			continue
		case ch == '\'':
			return "", unsupported("rewrite-jq", "JQ 程序在字符串之外出现单引号")
		case ch == '"':
			stack = append(stack, '"')
		case ch == '(':
			stack = append(stack, ')')
		case ch == '[':
			stack = append(stack, ']')
		case ch == '{':
			stack = append(stack, '}')
		case ch == ')' || ch == ']' || ch == '}':
			if stack[len(stack)-1] != ch {
				return "", unsupported("rewrite-jq", "JQ 程序的括号不配对")
			}
			stack = stack[:len(stack)-1]
		case ch == '$' && strings.HasPrefix(program[i:], "$__loc__"):
			after := program[i+len("$__loc__"):]
			if after == "" || !(isNameChar(after[0]) || after[0] == '_') {
				return "", unsupported("rewrite-jq", "JQ 的 $__loc__ 依赖源文件行号，内联后无法保持")
			}
		}
		out.WriteByte(ifByte(ch == '\r', '\n', ch))
		i++
	}
	if len(stack) != 1 {
		return "", unsupported("rewrite-jq", "JQ 程序里的字符串或括号没有闭合")
	}

	lines := strings.Split(out.String(), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, " "), nil
}

// jsonEscapeRune 处理字符串里的控制字符与 Unicode 行分隔符。
// 返回空串表示这个字节不需要转义（非 ASCII 的正常字符）。
func jsonEscapeRune(text string) (string, int) {
	if len(text) == 0 {
		return "", 0
	}
	switch text[0] {
	case 0x85:
		return `\u0085`, 1
	case 0xe2:
		if strings.HasPrefix(text, "\u2028") {
			return `\u2028`, 3
		}
		if strings.HasPrefix(text, "\u2029") {
			return `\u2029`, 3
		}
		return "", 0
	}
	if text[0] < 0x20 {
		return `\u` + fmt.Sprintf("%04x", text[0]), 1
	}
	return "", 0
}

func ifByte(condition bool, yes, no byte) byte {
	if condition {
		return yes
	}
	return no
}

// stripComment 去掉行尾独立的 `// 注释`，不碰字符串和 URL 里的 `//`。
func stripComment(line string) string {
	for _, pos := range topPositions(line) {
		if pos > 0 && strings.HasPrefix(line[pos:], "//") && isSpace(line[pos-1]) {
			return strings.TrimRight(line[:pos], " \t")
		}
	}
	return strings.TrimRight(line, " \t")
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// words 按空白切词，但保留被引号包裹的整段。
func words(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		switch {
		case isSpace(text[i]):
			i++
		case text[i] == '"' || text[i] == '\'':
			end := skipQuoted(text, i)
			out = append(out, unquote(text[i:end]))
			i = end
		default:
			start := i
			for i < len(text) && !isSpace(text[i]) {
				i++
			}
			out = append(out, unquote(text[start:i]))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// .lpx 解析
// ---------------------------------------------------------------------------

// PluginMeta 只取头部的某个 #! 字段，遇到第一个 [section] 就停。
//
// 同步时用它给落盘文件命名、提取图标，此时还不该付整个解析的代价；调用方
// 要自己先去掉 BOM（见 kelee.decode）。取不到返回空串。
func PluginMeta(text, wanted string) string {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#!"):
			key, value, _ := strings.Cut(line[2:], "=")
			if strings.EqualFold(strings.TrimSpace(key), wanted) {
				return strings.TrimSpace(value)
			}
		case strings.HasPrefix(line, "#"), strings.HasPrefix(line, "//"), strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "["):
			return "" // 头部结束，后面不会再有元数据
		}
	}
	return ""
}

// PluginName 取头部的 #!name，取不到返回空串，由调用方决定兜底名字。
func PluginName(text string) string { return PluginMeta(text, "name") }

// PluginIcon 取头部的 #!icon，供索引和模块网站展示。
func PluginIcon(text string) string { return PluginMeta(text, "icon") }

// parsePlugin 把 .lpx 拆成头部元数据 + 各 section 的行（带行号）。
func parsePlugin(text string) *Plugin {
	plugin := &Plugin{Meta: map[string]string{}, Sections: map[string][]Line{}}
	current := ""

	for index, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#!") {
			key, value, _ := strings.Cut(line[2:], "=")
			key = strings.TrimSpace(key)
			if _, seen := plugin.Meta[key]; !seen {
				plugin.MetaKeys = append(plugin.MetaKeys, key)
			}
			plugin.Meta[key] = strings.TrimSpace(value)
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = NormalizeSection(line[1 : len(line)-1])
			if _, seen := plugin.Sections[current]; !seen {
				plugin.Order = append(plugin.Order, current)
				plugin.Sections[current] = nil
			}
			continue
		}
		if current == "" {
			continue
		}
		plugin.Sections[current] = append(plugin.Sections[current], Line{No: index + 1, Text: line})
	}
	return plugin
}
