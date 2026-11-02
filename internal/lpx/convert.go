package lpx

import (
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// 把解析好的 Loon 插件组装成 Surge 模块。
//
// 这里只负责"调度 + 各 section 的落地"，具体语法细节交给：
//
//	rule.go     [Rule]
//	rewrite.go  [Rewrite]
//	script.go   [Script]
//	syntax.go   文本切分与引号
//
// 新增一种 section：写一个方法，在 run() 里排一次队即可。

// Surge 模块头部认识的 #! 字段，其余字段丢弃并写进报告。
var surgeMeta = map[string]bool{
	"name": true, "desc": true, "author": true, "icon": true, "category": true,
	"system": true, "openUrl": true, "open": true, "tag": true, "homepage": true,
	"date": true, "requirement": true, "arguments": true, "compatibility": true,
}

// Loon 用 %APPEND% / %INSERT% 标记 hostname，Surge 侧只需补一次，源里的要摘掉。
var mitmMark = regexp.MustCompile(`(?i)%(?:append|insert)%`)

// 有专门处理器的 section；其余 section 一律原样注释，避免静默丢内容。
var handledSections = map[string]bool{
	"ARGUMENT": true, "GENERAL": true, "IP-REWRITE": true,
	"RULE": true, "REWRITE": true, "SCRIPT": true, "MITM": true,
}

var nonArgName = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Converter 是一次转换的载体。
type Converter struct {
	plugin  *Plugin
	ctx     *Ctx
	skipped []string // 无法转换、以注释形式保留的原始行
}

// ConvertText 把插件正文转成模块。
func ConvertText(source string, options Options) Result {
	converter := &Converter{
		plugin: parsePlugin(strings.TrimPrefix(source, "\ufeff")),
		ctx:    newCtx(options),
	}
	return converter.run()
}

// ConvertFile 读取文件并转换。
func ConvertFile(path string, options Options) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	return ConvertText(string(data), options), nil
}

func (c *Converter) run() Result {
	c.arguments()
	c.general()
	c.ipRewrite()
	c.rule()
	c.rewrite()
	c.script()
	c.mitm()
	c.unhandled()
	c.pruneArgs()

	sections, order := c.ctx.sections()
	if len(sections) == 0 {
		c.ctx.Fail("module-excluded", "没有任何可转换的内容，不生成模块", 0)
	}
	c.warnMissingMITM(sections)

	text := c.render(sections, order) // 渲染时还会补 meta 诊断，之后再取 notes
	name := c.plugin.Meta["name"]
	if name == "" {
		name = "Untitled"
	}
	return Result{
		Text:     text,
		Name:     name,
		Notes:    c.ctx.notes,
		Sections: sections,
		Order:    order,
		Skipped:  len(c.skipped),
	}
}

// -- [Argument] -------------------------------------------------------------

// arguments 处理 `Cron=select, "0 1 * * *", "0 2 * * *"`，取第二个字段作默认值。
func (c *Converter) arguments() {
	for _, line := range c.plugin.Lines("Argument") {
		key, body, found := strings.Cut(line.Text, "=")
		if !found {
			c.ctx.Warn("argument-parse", "无法解析参数行："+line.Text, line.No)
			continue
		}
		fields := splitTop(body, ",")
		name := argName(strings.TrimSpace(key))
		if _, exists := c.ctx.args.Defaults[name]; exists {
			c.ctx.Fail("argument-name-collision", "参数重名："+name, line.No)
			continue
		}
		value := ""
		if len(fields) > 1 {
			value = unquote(fields[1])
		}
		c.ctx.args.Defaults[name] = value
		c.ctx.args.Order = append(c.ctx.args.Order, name)
	}
}

func (c *Converter) pruneArgs() {
	unused := c.ctx.args.Unused()
	if len(unused) == 0 {
		return
	}
	drop := map[string]bool{}
	for _, name := range unused {
		drop[name] = true
		c.ctx.Warn("argument-unused", "参数 "+name+" 未被任何行引用，已删除", 0)
	}
	var kept []string
	for _, name := range c.ctx.args.Order {
		if drop[name] {
			delete(c.ctx.args.Defaults, name)
			continue
		}
		kept = append(kept, name)
	}
	c.ctx.args.Order = kept
}

// -- [General] --------------------------------------------------------------

func (c *Converter) general() {
	var realIPs, routes []string

	for _, line := range c.plugin.Lines("General") {
		key, value, _ := strings.Cut(line.Text, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = unquote(value)

		switch key {
		case "real-ip", "always-real-ip":
			realIPs = appendUnique(realIPs, strings.Split(value, ",")...)
		case "include-tun":
			for _, item := range strings.Split(value, ",") {
				if route := cidrOf(item, line.No, c.ctx); route != "" {
					routes = appendUnique(routes, route)
				}
			}
		case "ipv6-vif":
			switch strings.ToLower(value) {
			case "off":
				c.ctx.Emit("General", "ipv6-vif = disabled")
				c.ctx.Need(ReqLatest)
			case "auto", "always":
				// 与 Surge 默认一致，不必输出
			default:
				c.ctx.Warn("general-value", "ipv6-vif 取值无法映射："+value, line.No)
			}
		default:
			c.ctx.Review("general-unsupported", "未核验的 General 项，已忽略："+key, line.No)
		}
	}

	if len(realIPs) > 0 {
		c.ctx.Emit("General", "always-real-ip = %APPEND% "+strings.Join(realIPs, ", "))
		c.ctx.Need(ReqBase)
	}
	if len(routes) > 0 {
		c.ctx.Emit("General", "tun-included-routes = %APPEND% "+strings.Join(routes, ", "))
		c.ctx.Need(ReqLatest)
	}
}

// -- [IP Rewrite] -----------------------------------------------------------

func (c *Converter) ipRewrite() {
	seen := map[string]bool{}
	for _, line := range c.plugin.Lines("IP-Rewrite") {
		address, action, _ := strings.Cut(line.Text, "=")
		address = strings.TrimSpace(address)
		action = strings.ToLower(strings.TrimSpace(action))

		switch action {
		case "reflect", "reject", "drop":
		default:
			c.ctx.Fail("ip-rewrite", "无法映射的 IP Rewrite："+line.Text, line.No)
			continue
		}
		if strings.ContainsAny(address, "/:") {
			c.ctx.Fail("ip-rewrite", "无法映射的 IP Rewrite："+line.Text, line.No)
			continue
		}
		if seen[address] {
			c.ctx.Warn("ip-rewrite-duplicate", "重复地址已跳过："+address, line.No)
			continue
		}
		seen[address] = true
		c.ctx.Emit("IP Rewrite", address+" = "+action)
		c.ctx.Need(ReqLatest)
	}
}

// -- [Rule] / [Rewrite] / [Script] ------------------------------------------

func (c *Converter) rule() {
	seen := map[string]bool{}
	for _, line := range c.plugin.Lines("Rule") {
		text := stripComment(line.Text)
		if text == "" {
			continue
		}
		produced, err := convertRule(c.ctx, line.No, text)
		if err != nil {
			c.skip(line.No, text, err)
			continue
		}
		for _, emit := range produced {
			if seen[emit.Text] {
				c.ctx.Warn("rule-duplicate", "重复规则已合并："+shorten(emit.Text), line.No)
				continue
			}
			seen[emit.Text] = true
			c.ctx.Emit(emit.Section, emit.Text)
		}
	}
}

func (c *Converter) rewrite() {
	for _, line := range c.plugin.Lines("Rewrite") {
		text := stripComment(line.Text)
		if text == "" {
			continue
		}
		produced, err := convertRewrite(c.ctx, line.No, text)
		if err != nil {
			c.skip(line.No, text, err)
			continue
		}
		for _, emit := range produced {
			c.ctx.Emit(emit.Section, emit.Text)
		}
	}
}

func (c *Converter) script() {
	for _, line := range c.plugin.Lines("Script") {
		text := stripComment(line.Text)
		if text == "" {
			continue
		}
		produced, err := convertScript(c.ctx, line.No, text)
		if err != nil {
			c.skip(line.No, text, err)
			continue
		}
		for _, emit := range produced {
			c.ctx.Emit(emit.Section, emit.Text)
		}
	}
}

// -- [MITM] -----------------------------------------------------------------

func (c *Converter) mitm() {
	var hosts []string
	for _, line := range c.plugin.Lines("MITM") {
		key, value, _ := strings.Cut(line.Text, "=")
		if strings.ToLower(strings.TrimSpace(key)) != "hostname" {
			c.ctx.Warn("mitm-unsupported", "不支持的 MITM 项："+strings.TrimSpace(key), line.No)
			continue
		}
		// Loon 允许 `hostname = %APPEND% a.com`，这个标记由输出统一补一次
		items := strings.Split(value, ",")
		for i, item := range items {
			items[i] = mitmMark.ReplaceAllString(item, " ")
		}
		hosts = appendUnique(hosts, items...)
	}
	if len(hosts) > 0 {
		c.ctx.Emit("MITM", "hostname = %APPEND% "+strings.Join(hosts, ", "))
	}
}

func (c *Converter) warnMissingMITM(sections map[string][]string) {
	if _, has := sections["MITM"]; has {
		return
	}
	for _, name := range []string{"URL Rewrite", "Header Rewrite", "Body Rewrite", "Map Local"} {
		if _, ok := sections[name]; ok {
			c.ctx.Review("mitm-missing", "该模块改写了 HTTPS 流量，但源插件没有 hostname，需手动补 [MITM]", 0)
			return
		}
	}
}

// unhandled 对没有处理器的 section 不装作没看见：原样注释并报一声。
func (c *Converter) unhandled() {
	for _, name := range c.plugin.Order {
		lines := c.plugin.Sections[name]
		if handledSections[name] || len(lines) == 0 {
			continue
		}
		c.ctx.Review("section-unsupported",
			"["+name+"] 没有对应转换规则，"+strconv.Itoa(len(lines))+" 行已原样注释", 0)
		for _, line := range lines {
			c.skipped = append(c.skipped, line.Text)
		}
	}
}

// skip 处理无法安全转换的行：注释保留原文，其余部分照常产出。
func (c *Converter) skip(lineno int, line string, err error) {
	code, message := "unsupported", err.Error()
	if typed, ok := err.(*Unsupported); ok {
		code, message = typed.Code, typed.Message
	}
	c.ctx.ReviewSkipped(code, message+" → 已注释："+shorten(line), lineno)
	c.skipped = append(c.skipped, line)
}

// -- 输出 -------------------------------------------------------------------

type metaPair struct{ key, value string }

func (c *Converter) meta() []metaPair {
	var pairs []metaPair
	seen := map[string]bool{}
	add := func(key, value string) {
		if value == "" || seen[key] {
			return
		}
		seen[key] = true
		pairs = append(pairs, metaPair{key, value})
	}

	raw := c.plugin.Meta
	for _, key := range []string{"name", "desc", "author", "icon"} {
		add(key, raw[key])
	}
	add("category", c.ctx.options.Category)
	add("system", surgeSystem(raw["system"]))
	for _, key := range []string{"openUrl", "open", "tag", "homepage", "date"} {
		add(key, raw[key])
	}
	if len(c.ctx.args.Order) > 0 {
		add("arguments", c.ctx.args.Table())
		c.ctx.Need(ReqBase)
	}
	// requirement 依赖前面所有 need()，必须最后算
	add("requirement", "CORE_VERSION>="+strconv.Itoa(c.ctx.requirement))

	for _, key := range c.plugin.MetaKeys {
		if !surgeMeta[key] {
			c.ctx.Warn("meta-dropped", "头部字段 `"+key+"` 在 Surge 中无对应，已删除", 0)
		}
	}
	return pairs
}

func (c *Converter) render(sections map[string][]string, order []string) string {
	pairs := c.meta()
	index := map[string]string{}
	for _, pair := range pairs {
		index[pair.key] = pair.value
	}

	var blocks []string
	used := map[string]bool{}
	for _, key := range MetaOrder {
		if value := index[key]; value != "" {
			blocks = append(blocks, "#!"+key+"="+value)
			used[key] = true
		}
	}
	for _, pair := range pairs {
		if !used[pair.key] {
			blocks = append(blocks, "#!"+pair.key+"="+pair.value)
		}
	}

	if c.ctx.options.Banner {
		blocks = append(blocks, "", "# 由 loon2surge 自动转换")
		if len(c.skipped) > 0 {
			blocks = append(blocks, "# 有 "+strconv.Itoa(len(c.skipped))+" 行无法自动转换，见文件末尾注释")
		}
	}

	for _, name := range order {
		blocks = append(blocks, "", "["+name+"]")
		blocks = append(blocks, sections[name]...)
	}

	if len(c.skipped) > 0 {
		blocks = append(blocks, "", "# ---- 以下 Loon 原始行无法自动转换，请人工确认 ----")
		for _, line := range c.skipped {
			blocks = append(blocks, "# "+line)
		}
	}

	return strings.TrimRight(strings.Join(blocks, "\n"), "\n") + "\n"
}

// -- 小工具 -----------------------------------------------------------------

func shorten(text string) string {
	const limit = 72
	if len([]rune(text)) <= limit {
		return text
	}
	return string([]rune(text)[:limit-1]) + "…"
}

func appendUnique(bucket []string, items ...string) []string {
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		duplicate := false
		for _, existing := range bucket {
			if existing == item {
				duplicate = true
				break
			}
		}
		if !duplicate {
			bucket = append(bucket, item)
		}
	}
	return bucket
}

func cidrOf(item string, lineno int, ctx *Ctx) string {
	item = strings.TrimSpace(item)
	if item == "" {
		return ""
	}
	text := item
	if !strings.Contains(text, "/") {
		text += "/32"
	}
	_, network, err := net.ParseCIDR(text)
	if err != nil || network.IP.To4() == nil {
		ctx.Review("general-unsupported", "include-tun 只接受 IPv4 网段，已忽略："+item, lineno)
		return ""
	}
	ones, bits := network.Mask.Size()
	if bits != 32 {
		ctx.Review("general-unsupported", "include-tun 只接受 IPv4 网段，已忽略："+item, lineno)
		return ""
	}
	return network.IP.String() + "/" + strconv.Itoa(ones)
}

func argName(raw string) string {
	name := nonArgName.ReplaceAllString(raw, "_")
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return "ARG_" + name
	}
	return name
}

func surgeSystem(raw string) string {
	text := strings.ToLower(raw)
	if text == "" {
		return ""
	}
	ios := strings.Contains(text, "ios") || strings.Contains(text, "ipados")
	mac := strings.Contains(text, "macos")
	switch {
	case ios && mac:
		return "" // 双平台就用 Surge 默认
	case ios:
		return "ios"
	case mac:
		return "mac"
	}
	return ""
}
