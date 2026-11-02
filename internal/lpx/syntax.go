package lpx

import (
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

var blankRun = regexp.MustCompile(`[\r\n]+`)

// jqLiteral 生成 Surge 的 JQ 表达式：单引号包裹，内部单引号写成 \u0027。
func jqLiteral(expr string) string {
	oneLine := strings.TrimSpace(blankRun.ReplaceAllString(expr, " "))
	return "'" + strings.ReplaceAll(oneLine, "'", `\u0027`) + "'"
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

// PluginName 只取头部的 #!name，遇到第一个 [section] 就停。
//
// 同步时用它给落盘文件命名，此时还不该付整个解析的代价；调用方要自己
// 先去掉 BOM（见 kelee.decode）。取不到返回空串，由调用方决定兜底名字。
func PluginName(text string) string {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#!"):
			key, value, _ := strings.Cut(line[2:], "=")
			if strings.EqualFold(strings.TrimSpace(key), "name") {
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
