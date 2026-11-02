package lpx

// Loon 的一行里同时混着正则、引号和嵌套括号，直接 split(",") 一定切坏。
// 这里实现一套"只在顶层动手"的扫描器，所有 section 处理器共用。

import (
	"regexp"
	"strings"
)

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

// loonUnescape 还原 Loon 双引号参数里的转义。
//
// Loon 把整段 JQ 写在双引号里，内部 JSON 的引号只能写成 `\"`，反斜杠写成 `\\`。
// 外壳去掉之后这些反斜杠不能留着：JQ 的词法分析会把 `\"` 当成字符串转义而不是
// 字符串边界，括号配对和注释判断全都会错位。
//
// 只认 `\"` 和 `\\` 两个。`\d` 这类正则转义在 JQ 里本来就是"反斜杠 + d"两个字符，
// 一旦解码就变成非法转义，所以原样保留。
func loonUnescape(text string) string {
	if !strings.ContainsRune(text, '\\') {
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
