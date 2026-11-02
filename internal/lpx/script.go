package lpx

import (
	"regexp"
	"strconv"
	"strings"
)

// Loon [Script] → Surge [Script]。
//
// Loon 的脚本行有两种形态，这里都归一成"触发器 + 匹配条件 + 属性表"：
//
//	旧式：http-response ^https://a\.com/api script-path=https://x/a.js, tag=去广告
//	V2：  http-request script-path=https://x/a.js, tag=去广告 if ${url} ~= /re/i
//
// enable= 在 Surge 里没有对应字段，用 Script-Hub 的行前缀方案：
// 参数默认值取 `#` 或空，脚本名前面挂上 {{{参数}}}。

// 输出时的属性顺序，未列出的属性一律丢弃并写进报告。
var scriptKeyOrder = []string{
	"requires-body",
	"binary-body-mode",
	"max-size",
	"engine",
	"timeout",
	"ability",
	"script-update-interval",
	"debug",
	"wake-system",
	"argument",
}

// 旧式 http 脚本没写超时时 Loon 取 10 秒，Surge 默认 5 秒，必须显式补上。
var scriptDefaultTimeout = map[string]int{
	"http-request":  10,
	"http-response": 10,
	"cron":          300,
	"generic":       300,
}

var falsyValues = map[string]bool{"false": true, "0": true, "off": true, "no": true, "#": true, "": true}

var (
	jsExt       = regexp.MustCompile(`(?i)\.(js|ts|jsdelivr)$`)
	illegalRune = regexp.MustCompile(`[^A-Za-z0-9_.\-]`)
)

// attrSet 保留属性在源文件里的顺序，输出与告警才稳定。
type attrSet struct {
	values map[string]string
	order  []string
}

func newAttrSet() *attrSet { return &attrSet{values: map[string]string{}} }

func (a *attrSet) set(key, value string) {
	if _, seen := a.values[key]; !seen {
		a.order = append(a.order, key)
	}
	a.values[key] = value
}

func (a *attrSet) get(key string) (string, bool) {
	value, ok := a.values[key]
	return value, ok
}

func (a *attrSet) pop(key string) (string, bool) {
	value, ok := a.values[key]
	if ok {
		delete(a.values, key)
	}
	return value, ok
}

func convertScript(ctx *Ctx, lineno int, line string) ([]Emit, error) {
	keyword, rest, _ := strings.Cut(line, " ")
	kind := strings.ToLower(strings.TrimSpace(keyword))
	rest = strings.TrimSpace(rest)

	if kind == "network-changed" {
		return nil, unsupported("script-network-changed", "Surge 没有 network-changed 触发器")
	}
	defaultTimeout, known := scriptDefaultTimeout[kind]
	if !known {
		return nil, unsupported("script-type", "不支持的脚本类型：%s", kind)
	}

	condition, rest := splitCondition(rest)
	pattern, attrs, err := splitPattern(rest)
	if err != nil {
		return nil, err
	}
	if condition != "" {
		pattern, err = urlPattern(condition)
		if err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(kind, "http") && pattern == "" {
		return nil, unsupported("script-pattern", "缺少匹配条件")
	}

	scriptPath, _ := attrs.pop("script-path")
	if scriptPath == "" {
		return nil, unsupported("script-path", "缺少 script-path")
	}
	if timeout, ok := attrs.get("timeout"); !ok || timeout == "" {
		attrs.set("timeout", strconv.Itoa(defaultTimeout))
	}

	name, _ := attrs.pop("tag")
	if name == "" {
		name = nameFromPath(scriptPath)
	}
	name = uniqueName(ctx, name)

	pieces := []string{"type=" + kind}
	switch {
	case strings.HasPrefix(kind, "http"):
		pieces = append(pieces, "pattern="+pattern)
	case kind == "cron":
		pieces = append(pieces, "cronexp="+quoteValue(pattern))
	}
	pieces = append(pieces, "script-path="+scriptPath)

	for _, key := range scriptKeyOrder {
		value, ok := attrs.pop(key)
		if !ok {
			continue
		}
		switch key {
		case "argument":
			pieces = append(pieces, "argument="+quoteValue(value))
		case "requires-body", "binary-body-mode":
			if strings.EqualFold(value, "false") {
				continue
			}
			pieces = append(pieces, key+"="+value)
		default:
			pieces = append(pieces, key+"="+value)
		}
	}

	enable, _ := attrs.get("enable")
	for _, key := range attrs.order {
		if _, left := attrs.values[key]; !left || key == "enable" || key == "img-url" {
			continue
		}
		ctx.Warn("script-attribute", "忽略未知脚本属性 "+key, lineno)
	}

	if kind == "generic" {
		ctx.Warn("script-generic", "generic 脚本在 Surge 里没有 Loon 的节点上下文，请自行确认", lineno)
	}

	body := name + " = " + strings.Join(pieces, ", ")
	wrapped, err := applyEnable(ctx, lineno, body, enable)
	if err != nil {
		return nil, err
	}
	return []Emit{{"Script", wrapped}}, nil
}

// splitCondition 把 V2 的 ` if ...` 尾巴拆出来。
func splitCondition(text string) (string, string) {
	at := findTop(text, " if ", 0)
	if at < 0 {
		return "", text
	}
	head := strings.TrimSpace(text[:at])
	condition := strings.TrimSpace(text[at+len(" if "):])
	if thenAt := findTop(condition, " then ", 0); thenAt >= 0 {
		// `if ... then ...` 的动作部分在脚本里没有意义，丢掉
		condition = strings.TrimSpace(condition[:thenAt])
	}
	return condition, head
}

// splitPattern 首个 token 若是 k=v 则没有内联匹配式（V2 走 if 条件）。
func splitPattern(text string) (string, *attrSet, error) {
	head, tail := takeToken(text)
	pattern, attrText := "", text
	if !strings.Contains(head, "=") {
		pattern, attrText = unquote(head), tail
	}

	attrs := newAttrSet()
	for _, chunk := range splitTop(attrText, ",") {
		if chunk == "" {
			continue
		}
		key, value, found := strings.Cut(chunk, "=")
		if !found {
			return "", nil, unsupported("script-attribute", "属性缺少取值：%s", chunk)
		}
		attrs.set(strings.TrimSpace(key), unquote(value))
	}
	return pattern, attrs, nil
}

// takeToken 取首个 token；带引号时整体取出（cron 表达式里有空格）。
func takeToken(text string) (string, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if text[0] == '"' || text[0] == '\'' {
		end := skipQuoted(text, 0)
		return text[:end], strings.TrimSpace(text[end:])
	}
	head, tail, _ := strings.Cut(text, " ")
	return head, strings.TrimSpace(tail)
}

// applyEnable 把 Loon 的 enable 开关翻成 Surge 的行前缀。
func applyEnable(ctx *Ctx, lineno int, body, raw string) (string, error) {
	value := unquote(strings.TrimSpace(raw))
	if value == "" {
		return body, nil
	}
	switch strings.ToLower(value) {
	case "true", "1", "on", "yes":
		return body, nil
	}
	if falsyValues[strings.ToLower(value)] {
		ctx.Warn("script-disabled", "脚本在源插件中默认关闭，已注释", lineno)
		return "# " + body, nil
	}

	param, ok := enableParam(value)
	if !ok {
		return "", unsupported("script-enable", "无法识别的 enable 取值：%s", raw)
	}
	defaultValue, declared := ctx.args.Defaults[param]
	if !declared {
		return "", unsupported("script-enable", "enable 引用了未声明参数：%s", param)
	}

	ctx.args.Used[param] = true
	if falsyValues[strings.ToLower(strings.TrimSpace(defaultValue))] {
		ctx.args.Defaults[param] = "#"
	}
	ctx.Warn("script-enable", "enable 已转为行前缀开关 {{{"+param+"}}}", lineno)
	return "{{{" + param + "}}}" + body, nil
}

// enableParam 从 `$?\{?([A-Za-z0-9_]+)\}?` 里取出参数名。
func enableParam(value string) (string, bool) {
	text := strings.TrimPrefix(value, "$")
	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		text = text[1 : len(text)-1]
	}
	if text == "" {
		return "", false
	}
	for i := 0; i < len(text); i++ {
		if !isNameChar(text[i]) {
			return "", false
		}
	}
	return text, true
}

func uniqueName(ctx *Ctx, name string) string {
	candidate, index := name, 2
	for ctx.names[candidate] {
		candidate = name + "-" + strconv.Itoa(index)
		index++
	}
	ctx.names[candidate] = true
	return candidate
}

func nameFromPath(path string) string {
	stem := path
	if idx := strings.LastIndex(stem, "/"); idx >= 0 {
		stem = stem[idx+1:]
	}
	if idx := strings.Index(stem, "?"); idx >= 0 {
		stem = stem[:idx]
	}
	stem = jsExt.ReplaceAllString(stem, "")
	stem = strings.Trim(illegalRune.ReplaceAllString(stem, "_"), "_")
	if stem == "" {
		return "Script"
	}
	return stem
}
