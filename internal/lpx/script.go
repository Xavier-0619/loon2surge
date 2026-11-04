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

// V2 触发器名与 Surge 的对应：Loon 写 request / response，Surge 要 http-request / http-response。
var scriptKindAlias = map[string]string{
	"request":  "http-request",
	"response": "http-response",
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
	// renamed 记录被归一过的属性名（`requires_body` → `requires-body`）。
	// 这不是美化：不归一就会被当成未知属性丢掉，而 requires-body 掉了
	// 脚本就读不到响应体——模块能导入，但去广告静默失效。
	renamed []string
	// conflicts 记录同一个属性被赋了不同值。Loon 里后者覆盖前者，但多半是
	// 上游写错了，猜哪个生效等于替用户做决定，所以留给上层拒绝。
	conflicts []string
}

func newAttrSet() *attrSet { return &attrSet{values: map[string]string{}} }

// normalizeAttrKey 把 `requires_body`、`Requires-Body` 这类写法统一成 Surge 的连字符小写。
//
// 可莉的插件两种写法都在用（财新去广告整份用下划线），Loon 自己两种都吃，
// Surge 只认连字符形式。
func normalizeAttrKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
}

func (a *attrSet) set(key, value string) {
	if normalized := normalizeAttrKey(key); normalized != key {
		a.renamed = appendUnique(a.renamed, key+" → "+normalized)
		key = normalized
	}
	if previous, seen := a.values[key]; seen {
		if previous != value {
			a.conflicts = appendUnique(a.conflicts, key)
		}
		a.values[key] = value
		return
	}
	a.order = append(a.order, key)
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
	if alias, ok := scriptKindAlias[kind]; ok {
		kind = alias
	}
	defaultTimeout, known := scriptDefaultTimeout[kind]
	if !known {
		return nil, unsupported("script-type", "不支持的脚本类型：%s", kind)
	}

	// V2 的路径写在动作里：`generic then script("https://…/a.js") with tag=…`。
	// 先把它摘出来，剩下的部分才按"匹配条件"解析。
	pathFromAction := ""
	var actionAttrs *attrSet
	if head, action, found := splitThen(rest); found {
		path, attrs, err := parseScriptAction(ctx, action)
		if err != nil {
			return nil, err
		}
		pathFromAction, actionAttrs, rest = path, attrs, head
	}

	condition, rest := splitCondition(rest)
	pattern, attrs, err := splitPattern(rest)
	if err != nil {
		return nil, err
	}
	if actionAttrs != nil {
		for _, key := range actionAttrs.order {
			if value, ok := actionAttrs.values[key]; ok {
				attrs.set(key, value)
			}
		}
	}
	// 属性名归一过就明说：这一条往往决定了脚本到底能不能跑。
	renamed := append([]string{}, attrs.renamed...)
	if actionAttrs != nil {
		renamed = append(renamed, actionAttrs.renamed...)
	}
	for _, change := range appendUnique(nil, renamed...) {
		ctx.Adapt("script-attribute-alias", "属性名归一 "+change, lineno)
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
		scriptPath = pathFromAction
	}
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
	name, renamedTo := uniqueName(ctx, name)
	if renamedTo != "" {
		ctx.Adapt("script-name-shared", "脚本重名，已改名为 "+renamedTo+
			"（Surge 里同名脚本会被后者覆盖，只留一条生效）", lineno)
	}

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
		if _, left := attrs.values[key]; !left || key == "enable" || isDisplayKey(key) {
			continue
		}
		// 丢掉一个不认识的属性可能是无声的功能缺失（当年 requires_body 就是这么丢的），
		// 所以归到"请人看一眼"，不当作普通提示。
		ctx.Review("script-attribute", "Surge 没有这个脚本属性，已丢弃："+key, lineno)
	}

	if kind == "generic" {
		ctx.Review("script-generic", "generic 脚本在 Surge 里没有 Loon 的节点上下文，请自行确认", lineno)
	}

	body := name + " = " + strings.Join(pieces, ", ")
	if kind == "cron" && !plausibleCron(pattern) {
		return nil, unsupported("script-cron", "cron 表达式不是 5/6 段数字写法：%s", pattern)
	}
	wrapped, err := applyEnable(ctx, lineno, body, enable)
	if err != nil {
		return nil, err
	}
	if len(attrs.conflicts) > 0 {
		return nil, unsupported("script-attribute",
			"同一属性被赋了不同值，无法判断哪个生效：%s", strings.Join(attrs.conflicts, "、"))
	}
	return []Emit{{"Script", wrapped}}, nil
}

// plausibleCron 只做形状检查：5 或 6 段、每段是数字和 * , - / 的组合。
// 含模块参数占位符的表达式留给 Surge 在运行期判断，不在这里假装能验。
func plausibleCron(expr string) bool {
	text := strings.TrimSpace(expr)
	if text == "" {
		return false
	}
	if strings.Contains(text, "{") {
		return true
	}
	fields := strings.Fields(text)
	if len(fields) != 5 && len(fields) != 6 {
		return false
	}
	for _, field := range fields {
		for i := 0; i < len(field); i++ {
			ch := field[i]
			if (ch >= '0' && ch <= '9') || ch == '*' || ch == ',' || ch == '-' || ch == '/' {
				continue
			}
			return false
		}
	}
	return true
}

// isDisplayKey 是只在客户端列表里显示用的元数据，Surge 没有对应字段，静默丢弃。
func isDisplayKey(key string) bool {
	switch strings.ToLower(key) {
	case "img-url", "img_url", "icon":
		return true
	}
	return false
}

// splitThen 拆出 V2 的 ` then <动作>`；只认顶层，正则里的 then 不算。
//
// `generic then script(…)` 的 then 紧跟在触发器后面，切词后没有前导空格，也要认。
func splitThen(text string) (head, action string, found bool) {
	at := findTop(text, " then ", 0)
	if at < 0 {
		if len(text) >= 5 && strings.EqualFold(text[:5], "then ") {
			return "", strings.TrimSpace(text[5:]), true
		}
		return text, "", false
	}
	return strings.TrimSpace(text[:at]), strings.TrimSpace(text[at+len(" then "):]), true
}

// parseScriptAction 解析 `script("url") with k=v, k=v`，取出路径与属性表。
//
// 还有一种带参数清单的写法：`script("url", {${A}, ${B}})`。清单里的名字在 Loon 里
// 表示"把这两个参数传给脚本"，脚本用 $argument.A 取值；Surge 没有位置参数，
// 只能传一条 argument=，所以拼成一个 JSON 对象塞进去——脚本里 $argument.A 照样取得到。
func parseScriptAction(ctx *Ctx, action string) (string, *attrSet, error) {
	const prefix = "script("
	if len(action) < len(prefix) || !strings.EqualFold(action[:len(prefix)], prefix) {
		return "", nil, unsupported("script-action", "无法识别的脚本动作：%s", action)
	}

	end := matchParen(action, len(prefix))
	if end < 0 {
		return "", nil, unsupported("script-action", "script( 没有闭合：%s", action)
	}
	path, names, err := splitScriptArgs(action[len(prefix):end])
	if err != nil {
		return "", nil, err
	}

	attrs := newAttrSet()
	if len(names) > 0 {
		attrs.set("argument", argumentJSON(ctx, names))
	}

	rest := strings.TrimSpace(action[end+1:])
	if rest == "" {
		return path, attrs, nil
	}
	if len(rest) < 4 || !strings.EqualFold(rest[:4], "with") {
		return "", nil, unsupported("script-action", "script(…) 之后只支持 with 属性：%s", rest)
	}
	for _, chunk := range splitTop(strings.TrimSpace(rest[4:]), ",") {
		if chunk == "" {
			continue
		}
		key, value, found := strings.Cut(chunk, "=")
		if !found {
			return "", nil, unsupported("script-attribute", "属性缺少取值：%s", chunk)
		}
		attrs.set(strings.TrimSpace(key), unquote(value))
	}
	return path, attrs, nil
}

// splitScriptArgs 拆 script( 括号里的内容：路径 + 可选的 `{${A}, ${B}}` 参数清单。
func splitScriptArgs(inside string) (string, []string, error) {
	head, tail := takeToken(inside)
	path := unquote(head)
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return path, nil, nil
	}
	if !strings.HasPrefix(tail, ",") {
		return "", nil, unsupported("script-action", "script(…) 里除了路径只认参数清单：%s", tail)
	}
	tail = strings.TrimSpace(strings.TrimPrefix(tail, ","))
	if !strings.HasPrefix(tail, "{") {
		return "", nil, unsupported("script-action", "脚本参数清单要用 {} 包起来：%s", tail)
	}
	closeAt := strings.LastIndex(tail, "}")
	if closeAt < 0 {
		return "", nil, unsupported("script-action", "脚本参数清单没有闭合：%s", tail)
	}

	var names []string
	for _, chunk := range splitTop(tail[1:closeAt], ",") {
		name := strings.TrimSpace(chunk)
		name = strings.TrimPrefix(name, "$")
		name = strings.Trim(strings.TrimSpace(name), "{}")
		if name = strings.TrimSpace(name); name != "" {
			names = appendUnique(names, name)
		}
	}
	return path, names, nil
}

// argumentJSON 把参数名清单拼成 Surge 的 argument= 取值。
//
// 键必须保持插件里的原样（`Weather.Provider`），脚本就是按这个名字取值的；
// 值才是 `{{{声明名}}}`——[Argument] 里的点号会被换成下划线（Surge 参数名只认
// 字母数字下划线），两边用 argName 对齐才不会一处有一处没有。
//
// 没声明过的参数留在 JSON 里给 null：Loon 那边它们本来就是 undefined，
// 脚本自己会回落默认值。
func argumentJSON(ctx *Ctx, names []string) string {
	pieces := make([]string, 0, len(names))
	var undeclared []string
	for _, name := range names {
		key := argName(name)
		if _, declared := ctx.args.Defaults[key]; declared {
			ctx.args.Used[key] = true
			pieces = append(pieces, quoteValue(name)+":{{{"+key+"}}}")
			continue
		}
		undeclared = append(undeclared, name)
		pieces = append(pieces, quoteValue(name)+":null")
	}
	if len(undeclared) > 0 {
		ctx.Review("script-argument", "参数没有在 [Argument] 里声明，按 null 传："+strings.Join(undeclared, ", "), 0)
	}
	ctx.Adapt("script-object-adapted", "脚本参数清单已转成 Surge 的 argument JSON", 0)
	return "{" + strings.Join(pieces, ",") + "}"
}

// matchParen 从 start 开始找配对的右括号，跳过引号内的内容。
func matchParen(text string, start int) int {
	depth := 1
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '"', '\'':
			i = skipQuoted(text, i) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitCondition 把 V2 的 ` if ...` 尾巴拆出来。
//
// 两种写法都要认：`http-request script-path=… if …`（if 在属性之后），
// 以及 `request if … then …`（if 紧跟触发器，前面没有空格可切）。
func splitCondition(text string) (string, string) {
	at := findTop(text, " if ", 0)
	if at < 0 {
		if len(text) >= 3 && strings.EqualFold(text[:3], "if ") {
			return strings.TrimSpace(text[3:]), ""
		}
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
		ctx.Review("script-disabled", "脚本在源插件中默认关闭，已注释；需要它生效请去掉行首的 #", lineno)
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

	// 同一个参数既当开关、又出现在 cron 或 argument 里时，不能把它的默认值
	// 改成 `#`：脚本拿到的会变成开关值，而不是原本的入参。
	// 这种情况 Surge 没有等价写法，只能按默认值把整行静态注释或保留。
	if referencesArg(body, param) {
		ctx.args.Used[param] = true
		if falsyValues[strings.ToLower(strings.TrimSpace(defaultValue))] {
			ctx.Review("script-enable-shared",
				"参数 "+param+" 同时是脚本入参，按默认值保持关闭（未用行前缀）", lineno)
			return "# " + body, nil
		}
		ctx.Review("script-enable-shared",
			"参数 "+param+" 同时是脚本入参，按默认值保持开启（未用行前缀）", lineno)
		return body, nil
	}

	ctx.args.Used[param] = true
	if falsyValues[strings.ToLower(strings.TrimSpace(defaultValue))] {
		ctx.args.Defaults[param] = "#"
	}
	ctx.Adapt("script-enable", "enable 已转为行前缀开关 {{{"+param+"}}}", lineno)
	return "{{{" + param + "}}}" + body, nil
}

// referencesArg 判断一行产出里是否还引用了某个参数（三种占位符写法都算）。
func referencesArg(text, name string) bool {
	return strings.Contains(text, "{"+name+"}") ||
		strings.Contains(text, "${"+name+"}") ||
		strings.Contains(text, "{{{"+name+"}}}")
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

func uniqueName(ctx *Ctx, name string) (string, string) {
	candidate, index := name, 2
	for ctx.names[candidate] {
		candidate = name + "-" + strconv.Itoa(index)
		index++
	}
	ctx.names[candidate] = true
	if candidate != name {
		return candidate, candidate
	}
	return candidate, ""
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
