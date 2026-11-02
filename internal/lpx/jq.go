package lpx

// 内联 JQ 表达式的压行与转义。
//
// Loon 允许把多行 JQ 直接写在插件里，Surge 的 Body Rewrite 只吃单行。压平不能
// 简单地把换行换成空格：JQ 的 `#` 注释靠换行结束，压平之后注释会把后面的代码
// 一起吃掉，模块能导入但改写整段失效。所以这里按词法走一遍。

import (
	"fmt"
	"strings"
)

// jqLiteral 生成 Surge 的 JQ 表达式：先压成一行，再用单引号包起来。
func jqLiteral(expr string) (string, error) {
	oneLine, err := compactJQ(expr)
	if err != nil {
		return "", unsupported("rewrite-jq", "%s", err.Error())
	}
	return "'" + oneLine + "'", nil
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
			// C2 开头的两字节字符里只有 U+0085 要转义；0x85 本身是续字节，
			// 汉字（比如「关」= E5 85 B3）的中间字节就是它，按字节比会把字切断。
			case ch < 0x20 || ch == 0xc2 || ch == 0xe2:
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
		// 代码态的 CR 一律折成 LF，和下面按行压平的口径保持一致。
		if ch == '\r' {
			out.WriteByte('\n')
		} else {
			out.WriteByte(ch)
		}
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
//
// U+0085 在 UTF-8 里是 C2 85，必须整字比较：只比 0x85 会命中汉字的中间字节，
// 把「关」写成 `\xe5\u0085\xb3` 这种非法 UTF-8，jq 里也匹配不到原字符串。
func jsonEscapeRune(text string) (string, int) {
	if len(text) == 0 {
		return "", 0
	}
	if strings.HasPrefix(text, "\u0085") {
		return `\u0085`, len("\u0085")
	}
	if text[0] == 0xe2 {
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
