package lpx

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Loon [Rewrite] → Surge 的 URL / Header / Body / Map Local 四类 section。
//
// Loon 目前有两种写法，都会被归一成 (phase, pattern, action) 再落地：
//
//	V2：  request if ${url} ~= /^https:\/\/a\.com\//i then response.json.jq("...")
//	旧式：^https://a\.com/api url reject-dict
//
// 无法等价表达的行不会硬转，而是返回 Unsupported，由上层注释掉并写进报告。

var prefixes = map[string]string{"request": "http-request", "response": "http-response"}

var rejectCodes = map[string]int{"reject": 404, "reject_dict": 200, "reject_img": 200, "reject_array": 200}
var rejectBodies = map[string]string{"reject_dict": "{}", "reject_array": "[]"}

var actionName = regexp.MustCompile(`^(request|response)\.(header|body|json)\.(\w+)$`)

var rewriteCond = regexp.MustCompile(`^\$\{?url\}?\s*(~=|=~|==)\s*(.+)$`)

// urlPattern 把 Loon 的 URL 条件转成 Surge 用的单行正则。
// 支持 ${url} ~= /re/i 和 ${url} == "常量"；其余组合一律不做猜测。
func urlPattern(condition string) (string, error) {
	text := strings.TrimSpace(condition)
	for strings.HasPrefix(text, "if ") {
		text = strings.TrimSpace(text[3:])
	}
	if cut := findTop(text, " then ", 0); cut >= 0 {
		text = strings.TrimSpace(text[:cut])
	}

	match := rewriteCond.FindStringSubmatch(text)
	if match == nil {
		return "", unsupported("rewrite-condition", "不支持的条件：%s", strings.TrimSpace(condition))
	}
	operator, rhs := match[1], strings.TrimSpace(match[2])

	if operator == "==" {
		return "^" + escapeRegex(unquote(rhs)) + "$", nil
	}

	body, flags, ok := splitRegexLiteral(rhs)
	if !ok {
		return "", unsupported("rewrite-condition", "URL 正则格式无法识别：%s", rhs)
	}
	switch flags {
	case "":
		return body, nil
	case "i":
		return "(?i:" + body + ")", nil
	}
	return "", unsupported("rewrite-regex-flag", "URL 正则 flag /%s 尚无等价写法", flags)
}

// escapeRegex 按 Python re.escape 的语义转义正则元字符。
func escapeRegex(text string) string {
	const special = `()[]{}?*+-|^$\.&~# `
	const blanks = "\t\n\r\v\f"
	var b strings.Builder
	b.Grow(len(text) + 8)
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if strings.IndexByte(special, ch) >= 0 || strings.IndexByte(blanks, ch) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// splitRegexLiteral 把 `/re/flags` 拆成 (/re/, flags)，只接受字母 flag。
func splitRegexLiteral(raw string) (string, string, bool) {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	for i := len(text) - 1; i > 0; i-- {
		if text[i] != '/' || !allAlpha(text[i+1:]) {
			continue
		}
		return text[1:i], text[i+1:], true
	}
	return "", "", false
}

// regexLiteral 把 `/re/i` 转成 `(?i:re)`；Surge 的正则参数不带分隔符。
// 解析不了（或 flag 不是小写字母）时原样返回，交由 Surge 自行判断。
func regexLiteral(raw string) string {
	body, flags, ok := splitRegexLiteral(raw)
	if !ok || !allLowerAlpha(flags) {
		return raw
	}
	if flags == "" {
		return body
	}
	return "(?" + flags + ":" + body + ")"
}

func allLowerAlpha(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 'a' || text[i] > 'z' {
			return false
		}
	}
	return true
}

func allAlpha(text string) bool {
	for i := 0; i < len(text); i++ {
		if !isAlpha(text[i]) {
			return false
		}
	}
	return true
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

func convertRewrite(ctx *Ctx, lineno int, line string) ([]Emit, error) {
	if strings.HasPrefix(line, "request if ") || strings.HasPrefix(line, "response if ") {
		return fromV2(ctx, lineno, line)
	}
	return fromLegacy(ctx, lineno, line)
}

func fromV2(ctx *Ctx, lineno int, line string) ([]Emit, error) {
	headEnd := findTop(line, " ", 0)
	if headEnd < 0 {
		return nil, unsupported("rewrite-v2", "缺少触发阶段")
	}
	phase := strings.TrimSpace(line[:headEnd])
	rest := strings.TrimSpace(line[headEnd:])

	thenAt := findTop(rest, " then ", 0)
	if thenAt < 0 {
		return nil, unsupported("rewrite-v2", "缺少 then 子句")
	}
	// Loon 的 `as name` 给 URL 捕获组起名，后面写 `${name.1}` 引用；
	// Surge 侧只有 `$1`，所以把前缀名摘掉、把引用换成编号。
	condition, capture := splitCapture(rest[:thenAt])
	pattern, err := urlPattern(condition)
	if err != nil {
		return nil, err
	}
	action := applyCapture(rest[thenAt+len(" then "):], capture)
	return applyRewrite(ctx, lineno, phase, pattern, strings.TrimSpace(action))
}

var captureRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\.(\d+)\}`)

// splitCapture 把 `/re/i as name` 拆成 (/re/i, name)。
func splitCapture(condition string) (string, string) {
	text := strings.TrimSpace(condition)
	at := findTop(text, " as ", 0)
	if at < 0 {
		return text, ""
	}
	name := strings.TrimSpace(text[at+len(" as "):])
	if name == "" || !isIdentifier(name) {
		return text, ""
	}
	return strings.TrimSpace(text[:at]), name
}

// applyCapture 把 `${name.2}` 换成 `$2`；别的参数占位符原样保留。
func applyCapture(text, name string) string {
	if name == "" {
		return text
	}
	return captureRef.ReplaceAllStringFunc(text, func(match string) string {
		parts := captureRef.FindStringSubmatch(match)
		if parts[1] != name {
			return match
		}
		return "$" + parts[2]
	})
}

func isIdentifier(text string) bool {
	if text == "" || (text[0] >= '0' && text[0] <= '9') {
		return false
	}
	for i := 0; i < len(text); i++ {
		if !isNameChar(text[i]) {
			return false
		}
	}
	return true
}

func applyRewrite(ctx *Ctx, lineno int, phase, pattern, action string) ([]Emit, error) {
	name, rawArgs, err := splitCall(action)
	if err != nil {
		return nil, err
	}
	var args []string
	if rawArgs != "" {
		// 这里不解码转义：`json.delete([...])` 的数组是裸写的 JSON 文本，
		// 把它的 \" 当 Loon 转义还原会把 JSON 弄坏。需要还原的动作各自处理。
		for _, part := range splitTop(rawArgs, ",") {
			args = append(args, unquote(part))
		}
	}

	lowered := strings.ToLower(name)

	if code, ok := rejectCodes[lowered]; ok {
		return rejectAction(ctx, pattern, lowered, code, args)
	}
	if lowered == "redirect" {
		return redirectAction(pattern, args)
	}
	if lowered == "url.replace" {
		return urlReplaceAction(phase, pattern, args)
	}
	if match := actionName.FindStringSubmatch(lowered); match != nil {
		switch match[2] {
		case "header":
			return headerAction(match[1], pattern, match[3], args)
		case "body":
			if match[3] == "mock" || match[3] == "mock_file" {
				return mockAction(ctx, phase, pattern, match[3], args)
			}
			return bodyAction(ctx, match[1], pattern, match[3], args)
		default:
			return jsonAction(ctx, match[1], pattern, match[3], args)
		}
	}
	return nil, unsupported("rewrite-action", "不支持的改写动作：%s", action)
}

// splitCall 把 `response.json.jq("x")` 拆成 ("response.json.jq", `"x"`)。
func splitCall(action string) (string, string, error) {
	text := strings.TrimSpace(action)
	if text == "" || (!isAlpha(text[0]) && text[0] != '_') {
		return "", "", unsupported("rewrite-action", "无法解析的动作：%s", action)
	}
	end := 1
	for end < len(text) && (isNameChar(text[end]) || text[end] == '.') {
		end++
	}
	name, rest := text[:end], strings.TrimSpace(text[end:])
	if rest == "" {
		return name, "", nil
	}
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		return name, rest[1 : len(rest)-1], nil
	}
	return "", "", unsupported("rewrite-action", "无法解析的动作：%s", action)
}

// ---------------------------------------------------------------------------
// 各动作
// ---------------------------------------------------------------------------

func rejectAction(ctx *Ctx, pattern, name string, defaultCode int, args []string) ([]Emit, error) {
	code := defaultCode
	if len(args) > 0 && isDigits(args[0]) {
		parsed, err := strconv.Atoi(args[0])
		if err != nil {
			return nil, unsupported("map-local-status", "无法解析状态码：%s", args[0])
		}
		code = parsed
	}
	if code < 200 || code > 999 {
		return nil, unsupported("map-local-status", "Surge Map Local 只接受 200-999，收到 %d", code)
	}
	// 状态码与 data-type 都是 iOS 5.10.0 / Mac 5.6.0 才有的写法，
	// 低版本上整行不生效，门槛必须跟上。
	ctx.Need(ReqMapLocal)

	if name == "reject_img" {
		return []Emit{{"Map Local", pattern + " data-type=tiny-gif status-code=" + strconv.Itoa(code)}}, nil
	}
	body, ok := rejectBodies[name]
	if !ok {
		return []Emit{{"Map Local", pattern + ` data-type=text data="" status-code=` + strconv.Itoa(code)}}, nil
	}
	line := pattern + ` data-type=text data="` + body + `" status-code=` + strconv.Itoa(code) +
		` header="Content-Type:application/json"`
	return []Emit{{"Map Local", line}}, nil
}

// urlReplaceAction 处理 Loon V2 的 url.replace(目标)。
//
// Surge 的 [URL Rewrite] 是「整条 URL 换成目标」，第四列 header 表示
// 只改 URL、不动请求体里的同一地址——和 Loon 只改 URL 的行为一致。
func urlReplaceAction(phase, pattern string, args []string) ([]Emit, error) {
	if phase != "request" {
		return nil, unsupported("rewrite-url", "url.replace 只能用在 request 阶段")
	}
	if len(args) < 1 {
		return nil, unsupported("rewrite-url", "url.replace 缺少目标地址")
	}
	target, err := rewritToken(loonUnescape(args[0]), "url.replace 目标")
	if err != nil {
		return nil, err
	}
	return []Emit{{"URL Rewrite", pattern + " " + target + " header"}}, nil
}

// mockAction 把 V2 的 response.body.mock / mock_file 转成静态 [Map Local]。
func mockAction(ctx *Ctx, phase, pattern, op string, args []string) ([]Emit, error) {
	if phase != "response" {
		return nil, unsupported("rewrite-mock", "%s 只能用在 response 阶段", op)
	}
	if len(args) < 1 {
		return nil, unsupported("rewrite-mock", "%s 缺少内容", op)
	}
	body := args[0]

	if op == "mock_file" {
		// Loon 的 mock_file 有两种签名：mock_file(url) 和
		// mock_file("text", url, 200)——后者把类型和状态码也写在参数里。
		target, status := body, 200
		if len(args) >= 2 {
			target = args[1]
			if len(args) >= 3 && isDigits(args[2]) {
				if parsed, err := strconv.Atoi(args[2]); err == nil {
					status = parsed
				}
			}
		}
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			return nil, unsupported("rewrite-mock", "mock_file 只支持 HTTP(S) 地址：%s", target)
		}
		if status < 200 || status > 999 {
			return nil, unsupported("rewrite-mock", "Surge Map Local 只接受 200-999，收到 %d", status)
		}
		ctx.Need(ReqMapLocal)
		line := pattern + ` data-type=file data="` + target + `" status-code=` + strconv.Itoa(status)
		return []Emit{{"Map Local", line}}, nil
	}

	// 内联正文里出现引号或换行就没法直接放进 data="…"。
	// 引号改走 base64（Surge 支持 data-type=base64），换行则直接拒绝。
	body = loonUnescape(args[0])
	if strings.ContainsAny(body, "\r\n") {
		return nil, unsupported("rewrite-mock", "mock 正文含换行，无法写成单行 Map Local")
	}
	ctx.Need(ReqMapLocal)
	contentType := ""
	if json.Valid([]byte(body)) {
		contentType = ` header="Content-Type:application/json"`
	}
	if strings.ContainsAny(body, `"\`) {
		encoded := base64.StdEncoding.EncodeToString([]byte(body))
		line := pattern + ` data="` + encoded + `" data-type=base64 status-code=200` + contentType
		return []Emit{{"Map Local", line}}, nil
	}
	line := pattern + ` data-type=text data="` + body + `" status-code=200` + contentType
	return []Emit{{"Map Local", line}}, nil
}

func redirectAction(pattern string, args []string) ([]Emit, error) {
	if len(args) < 2 {
		return nil, unsupported("rewrite-redirect", "redirect 需要目标地址和状态码")
	}
	target, code := loonUnescape(args[0]), args[1]
	if isDigits(args[0]) {
		target, code = loonUnescape(args[1]), args[0]
	}
	if code != "302" && code != "307" {
		return nil, unsupported("rewrite-redirect", "Surge 只支持 302/307，收到 %s", code)
	}
	return []Emit{{"URL Rewrite", pattern + " " + target + " " + code}}, nil
}

func headerAction(phase, pattern, op string, args []string) ([]Emit, error) {
	prefix := prefixes[phase]
	args = decodeAll(args)
	// 改消息边界会让请求直接失败（Surge 会拒绝这次改写），不是"效果差一点"。
	if len(args) > 0 && isFramingHeader(args[0]) {
		return nil, unsupported("rewrite-header",
			"不能改 %s：Surge 会拒绝这种改变消息边界的 Header 改写", args[0])
	}
	switch op {
	case "add", "set":
		if len(args) < 2 {
			return nil, unsupported("rewrite-header", "%s 需要字段名和值", op)
		}
		var out []Emit
		if op == "set" { // Surge 没有 set，先删再加，避免出现重复头
			out = append(out, Emit{"Header Rewrite", prefix + " " + pattern + " header-del " + quoteToken(args[0])})
		}
		out = append(out, Emit{"Header Rewrite", prefix + " " + pattern + " header-add " +
			quoteToken(args[0]) + " " + quoteValue(args[1])})
		return out, nil

	case "del", "delete":
		if len(args) == 0 {
			return nil, unsupported("rewrite-header", "%s 需要字段名", op)
		}
		return []Emit{{"Header Rewrite", prefix + " " + pattern + " header-del " + quoteToken(args[0])}}, nil

	case "replace", "replace_regex":
		if len(args) < 3 {
			return nil, unsupported("rewrite-header", "%s 需要字段名、匹配与替换值", op)
		}
		key, needle := "header-replace", args[1]
		if op == "replace_regex" {
			key, needle = "header-replace-regex", regexLiteral(args[1])
		}
		line := prefix + " " + pattern + " " + key + " " + quoteToken(args[0]) +
			" " + quoteValue(needle) + " " + quoteValue(args[2])
		return []Emit{{"Header Rewrite", line}}, nil
	}
	return nil, unsupported("rewrite-header", "不支持的 header 动作：%s", op)
}

// decodeAll 把 Loon 参数层的转义还原成真实字符。
func decodeAll(items []string) []string {
	for i := range items {
		items[i] = loonUnescape(items[i])
	}
	return items
}

// isFramingHeader 判断字段名是否影响 HTTP 消息边界。
func isFramingHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content-length", "transfer-encoding":
		return true
	}
	return false
}

func bodyAction(ctx *Ctx, phase, pattern, op string, args []string) ([]Emit, error) {
	if op != "replace" && op != "replace_regex" {
		return nil, unsupported("rewrite-body", "不支持的 body 动作：%s", op)
	}
	if len(args) < 2 {
		return nil, unsupported("rewrite-body", "body.replace 需要正则和替换值")
	}
	// Surge 的 Body Rewrite 是 `http-request <url> <正则> <替换>`，
	// 不是 `http-response-replace-regex`——后者是 Header Rewrite 那套命名，
	// 写出来能导入但整行不生效。
	regex, err := bodyRegexLiteral(loonUnescape(args[0]))
	if err != nil {
		return nil, err
	}
	regexToken, err := rewritToken(regex, "正则")
	if err != nil {
		return nil, err
	}
	replacement, err := rewritToken(loonUnescape(args[1]), "替换文本")
	if err != nil {
		return nil, err
	}
	// Body Rewrite 是 iOS 5.10.0 / Mac 5.6.0 才有的 section。
	ctx.Need(ReqMapLocal)
	line := prefixes[phase] + " " + pattern + " " + regexToken + " " + replacement
	return []Emit{{"Body Rewrite", line}}, nil
}

// bodyRegexLiteral 把 Loon V2 的 `/re/flags` 转成带显式 flags 的 Surge 正则。
//
// Surge 的 Body Rewrite 默认按行锚定 `^`/`$`，Loon 的正则没有这层语义。
// 只写 `(?i)` 是"在默认值上再加"，所以必须把 ims 三个 flag 一起写死：
// 无 flags → `(?-ims)`，只有 /m → `(?m-is)`，以此类推。
func bodyRegexLiteral(raw string) (string, error) {
	body, flags, ok := splitRegexLiteral(raw)
	if !ok || !allLowerAlpha(flags) {
		return "", unsupported("rewrite-body", "body 正则要写成 /表达式/flags：%s", raw)
	}
	var on, off strings.Builder
	for _, flag := range "ims" {
		if strings.ContainsRune(flags, flag) {
			on.WriteRune(flag)
		} else {
			off.WriteRune(flag)
		}
	}
	for i := 0; i < len(flags); i++ {
		if !strings.ContainsRune("ims", rune(flags[i])) {
			return "", unsupported("rewrite-body", "body 正则不支持 flag /%c", flags[i])
		}
	}
	modifier := on.String()
	if off.Len() > 0 {
		modifier += "-" + off.String()
	}
	return "(?" + modifier + ")" + body, nil
}

// rewritToken 给可能含空格的 token 加单引号。
//
// Surge 按空格切分 body 正则和替换文本，含空格不加引号会被切成两段；
// 而单引号本身没法在 token 里安全转义，遇到就直接拒绝转换。
func rewritToken(value, what string) (string, error) {
	if !strings.ContainsAny(value, " \t") {
		return value, nil
	}
	if strings.ContainsRune(value, '\'') {
		return "", unsupported("rewrite-body", "%s同时含空格和单引号，无法安全加引号：%s", what, value)
	}
	return "'" + value + "'", nil
}

func jsonAction(ctx *Ctx, phase, pattern, op string, args []string) ([]Emit, error) {
	var expr string
	switch op {
	case "jq":
		if len(args) == 0 {
			return nil, unsupported("rewrite-jq", "json.jq 缺少表达式")
		}
		literal, err := jqLiteral(loonUnescape(args[0]))
		if err != nil {
			return nil, err
		}
		ctx.Need(ReqModern)
		return []Emit{{"Body Rewrite", prefixes[phase] + "-jq " + pattern + " " + literal}}, nil

	case "delete":
		paths, err := collectPaths(args)
		if err != nil {
			return nil, err
		}
		parts := make([]string, 0, len(paths))
		for _, path := range paths {
			parts = append(parts, jqPath(path))
		}
		expr = "delpaths([" + strings.Join(parts, ",") + "])"

	case "add", "replace":
		if len(args) < 2 {
			return nil, unsupported("rewrite-json", "json.%s 需要路径和值", op)
		}
		path, err := parsePath(args[0])
		if err != nil {
			return nil, err
		}
		// 父路径不存在时 setpath 会建出多余结构，用 try/catch 保持原样返回
		expr = "try setpath(" + jqPath(path) + "; " + args[1] + ") catch ."

	default:
		return nil, unsupported("rewrite-json", "不支持的 json 动作：%s", op)
	}

	literal, err := jqLiteral(expr)
	if err != nil {
		return nil, err
	}
	ctx.Need(ReqModern)
	return []Emit{{"Body Rewrite", prefixes[phase] + "-jq " + pattern + " " + literal}}, nil
}

// collectPaths 接受 delete("a.b") 与 delete(["a.b","c"]) 两种写法。
func collectPaths(args []string) ([][]any, error) {
	var paths [][]any
	for _, arg := range args {
		text := strings.TrimSpace(arg)
		if !strings.HasPrefix(text, "[") {
			path, err := parsePath(text)
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
			continue
		}
		var items []any
		if err := json.Unmarshal([]byte(text), &items); err != nil {
			return nil, unsupported("json-path", "无法解析路径列表：%s", text)
		}
		for _, item := range items {
			path, err := parsePath(toString(item))
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, unsupported("json-path", "json.delete 需要至少一个路径")
	}
	return paths, nil
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// parsePath 把 `vip_info.entrance_v2` 拆成 ["vip_info","entrance_v2"]，
// `data[0].ad` 拆成 ["data",0,"ad"]。
func parsePath(raw string) ([]any, error) {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, ".")
	if text == "" {
		return nil, unsupported("json-path", "JSON 路径为空")
	}

	var parts []any
	index, needKey := 0, true
	for index < len(text) {
		switch char := text[index]; {
		case char == '.':
			if needKey {
				return nil, unsupported("json-path", "路径不合法：%s", raw)
			}
			needKey, index = true, index+1
		case char == '[':
			closeAt := strings.IndexByte(text[index:], ']')
			if closeAt < 0 {
				return nil, unsupported("json-path", "下标未闭合：%s", raw)
			}
			closeAt += index
			inner := strings.TrimSpace(text[index+1 : closeAt])
			switch {
			case isDigits(inner):
				value, _ := strconv.Atoi(inner)
				parts = append(parts, value)
			case len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"':
				parts = append(parts, inner[1:len(inner)-1])
			default:
				return nil, unsupported("json-path", "不支持的下标：%s", inner)
			}
			index, needKey = closeAt+1, false
		case !needKey:
			return nil, unsupported("json-path", "缺少分隔符：%s", raw)
		default:
			stop := index
			for stop < len(text) && text[stop] != '.' && text[stop] != '[' {
				stop++
			}
			parts = append(parts, text[index:stop])
			index, needKey = stop, false
		}
	}
	if needKey {
		return nil, unsupported("json-path", "路径以 . 结尾：%s", raw)
	}
	return parts, nil
}

func jqPath(parts []any) string {
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if number, ok := part.(int); ok {
			items = append(items, strconv.Itoa(number))
			continue
		}
		items = append(items, jsonQuote(toString(part)))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// jsonQuote 与 json.dumps(..., ensure_ascii=False) 的行为对齐，且不转义 HTML 字符。
func jsonQuote(text string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(text); err != nil {
		return `"` + text + `"`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ---------------------------------------------------------------------------
// 旧语法：`<pattern> url reject` / `<pattern> response-header-add "A: b"`
// ---------------------------------------------------------------------------

func fromLegacy(ctx *Ctx, lineno int, line string) ([]Emit, error) {
	pattern, tail, found := strings.Cut(line, " ")
	if !found || strings.TrimSpace(pattern) == "" || strings.TrimSpace(tail) == "" {
		return nil, unsupported("rewrite-legacy", "缺少改写动作")
	}
	pattern = strings.TrimSpace(pattern)
	keyword, rest, _ := strings.Cut(strings.TrimSpace(tail), " ")
	rest = strings.TrimSpace(rest)

	switch {
	case keyword == "url":
		return legacyURL(ctx, pattern, rest)
	case strings.HasPrefix(keyword, "request-header"),
		strings.HasPrefix(keyword, "response-header"),
		strings.HasPrefix(keyword, "header"):
		return legacyHeader(pattern, keyword, rest)
	case strings.Contains(keyword, "body"):
		return legacyBody(ctx, pattern, keyword, rest)
	}
	return nil, unsupported("rewrite-legacy", "不支持的旧语法动作：%s", keyword)
}

func legacyURL(ctx *Ctx, pattern, rest string) ([]Emit, error) {
	args := decodeAll(words(rest))
	if len(args) == 0 {
		return nil, unsupported("rewrite-legacy", "url 后缺少动作")
	}

	switch action := strings.ToLower(args[0]); action {
	case "reject":
		return rejectAction(ctx, pattern, "reject", 404, nil)
	case "reject-200":
		return rejectAction(ctx, pattern, "reject", 200, nil)
	case "reject-dict":
		return rejectAction(ctx, pattern, "reject_dict", 200, nil)
	case "reject-array":
		return rejectAction(ctx, pattern, "reject_array", 200, nil)
	case "reject-img":
		return rejectAction(ctx, pattern, "reject_img", 200, nil)
	case "302", "307":
		if len(args) < 2 {
			return nil, unsupported("rewrite-legacy", "重定向缺少目标地址")
		}
		return []Emit{{"URL Rewrite", pattern + " " + args[1] + " " + action}}, nil
	}
	return nil, unsupported("rewrite-legacy", "不支持的 url 动作：%s", strings.ToLower(args[0]))
}

func legacyHeader(pattern, keyword, rest string) ([]Emit, error) {
	phase := "request"
	if strings.HasPrefix(keyword, "response") {
		phase = "response"
	}
	prefix := prefixes[phase]
	op := keyword[strings.LastIndex(keyword, "-")+1:]
	args := decodeAll(words(rest))

	switch op {
	case "del":
		if len(args) == 0 {
			return nil, unsupported("rewrite-header", "header-del 缺少字段名")
		}
		return []Emit{{"Header Rewrite", prefix + " " + pattern + " header-del " + quoteToken(args[0])}}, nil

	case "add":
		header := ""
		if len(args) > 0 {
			header = args[0]
		}
		name, value, found := strings.Cut(header, ":")
		if !found || strings.TrimSpace(value) == "" {
			return nil, unsupported("rewrite-header", "header-add 需要 `名称: 值`：%s", header)
		}
		line := prefix + " " + pattern + " header-add " + quoteToken(strings.TrimSpace(name)) +
			" " + quoteValue(strings.TrimSpace(value))
		return []Emit{{"Header Rewrite", line}}, nil

	case "replace":
		if len(args) < 3 {
			return nil, unsupported("rewrite-header", "header-replace 需要三个参数")
		}
		line := prefix + " " + pattern + " header-replace " + quoteToken(args[0]) +
			" " + quoteValue(args[1]) + " " + quoteValue(args[2])
		return []Emit{{"Header Rewrite", line}}, nil
	}
	return nil, unsupported("rewrite-header", "不支持的 header 动作：%s", keyword)
}

func legacyBody(ctx *Ctx, pattern, keyword, rest string) ([]Emit, error) {
	phase := "request"
	if strings.HasPrefix(keyword, "response") {
		phase = "response"
	}
	prefix := prefixes[phase]
	args := decodeAll(words(rest))

	switch {
	case strings.HasSuffix(keyword, "json-jq"):
		if len(args) == 0 {
			return nil, unsupported("rewrite-jq", "缺少 JQ 表达式")
		}
		literal, err := jqLiteral(args[0])
		if err != nil {
			return nil, err
		}
		ctx.Need(ReqModern)
		return []Emit{{"Body Rewrite", prefix + "-jq " + pattern + " " + literal}}, nil

	case strings.HasSuffix(keyword, "json-del"):
		var parts []string
		for _, arg := range args {
			path, err := parsePath(arg)
			if err != nil {
				return nil, err
			}
			parts = append(parts, jqPath(path))
		}
		if len(parts) == 0 {
			return nil, unsupported("rewrite-json", "json-del 缺少路径")
		}
		literal, err := jqLiteral("delpaths([" + strings.Join(parts, ",") + "])")
		if err != nil {
			return nil, err
		}
		ctx.Need(ReqModern)
		return []Emit{{"Body Rewrite", prefix + "-jq " + pattern + " " + literal}}, nil

	case strings.HasSuffix(keyword, "json-replace"):
		if len(args)%2 != 0 {
			return nil, unsupported("rewrite-json", "json-replace 的路径与值必须成对出现")
		}
		var exprs []string
		for i := 0; i < len(args); i += 2 {
			path, err := parsePath(args[i])
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, "try setpath("+jqPath(path)+"; "+args[i+1]+") catch .")
		}
		literal, err := jqLiteral(strings.Join(exprs, " | "))
		if err != nil {
			return nil, err
		}
		ctx.Need(ReqModern)
		return []Emit{{"Body Rewrite", prefix + "-jq " + pattern + " " + literal}}, nil

	case strings.HasSuffix(keyword, "replace-regex"):
		if len(args) < 2 {
			return nil, unsupported("rewrite-body", "body-replace-regex 需要正则和替换值")
		}
		// 旧语法的正则已经是"Loon 无 flags"语义，不套 V2 的 (?-ims) 前缀；
		// 但 Surge 侧默认按行锚定，这里补 (?-m) 把 multiline 关掉才对得上。
		regex, err := rewritToken("(?-m)"+args[0], "正则")
		if err != nil {
			return nil, err
		}
		replacement, err := rewritToken(loonUnescape(args[1]), "替换文本")
		if err != nil {
			return nil, err
		}
		ctx.Need(ReqMapLocal)
		line := prefix + " " + pattern + " " + regex + " " + replacement
		return []Emit{{"Body Rewrite", line}}, nil
	}
	return nil, unsupported("rewrite-legacy", "不支持的旧语法动作：%s", keyword)
}
