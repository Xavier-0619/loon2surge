package lpx

import (
	"sort"
	"strings"
)

// Loon [Rule] → Surge [Rule]（少量落到 [Map Local]）。
//
// Surge 模块的 [Rule] 只接受 DIRECT / REJECT / REJECT-TINYGIF，
// 因此带 PROXY 之类的规则会被注释掉，而不是整个模块报废。
//
// 选项补齐规则：
//   - 域名类 → extended-matching
//   - IP 类   → no-resolve
//   - 拒绝策略再加 pre-matching，非拒绝策略一律不加

var logicOps = map[string]bool{"AND": true, "OR": true, "NOT": true}

var domainTypes = map[string]bool{
	"DOMAIN": true, "DOMAIN-SUFFIX": true, "DOMAIN-KEYWORD": true,
	"DOMAIN-WILDCARD": true, "URL-REGEX": true,
}

var ipTypes = map[string]bool{"IP-CIDR": true, "IP-CIDR6": true, "GEOIP": true, "IP-ASN": true}

var plainTypes = map[string]bool{
	"DEST-PORT": true, "PROTOCOL": true, "SRC-PORT": true,
	"USER-AGENT": true, "PROCESS-NAME": true,
}

var modulePolicies = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-TINYGIF": true}
var rejectPolicies = map[string]bool{"REJECT": true, "REJECT-TINYGIF": true}

// Loon 里出现、但在 Surge 里应落到 [Map Local] 的策略。
var mapLocalByPolicy = map[string]struct {
	body        string
	contentType string
	tinyGif     bool
}{
	"REJECT-DICT":  {body: "{}", contentType: "application/json"},
	"REJECT-ARRAY": {body: "[]", contentType: "application/json"},
	"REJECT-IMG":   {tinyGif: true},
}

var ruleAliases = map[string]string{"IPASN": "IP-ASN"}

func allowedType(kind string) bool {
	return domainTypes[kind] || ipTypes[kind] || plainTypes[kind]
}

func convertRule(ctx *Ctx, lineno int, line string) ([]Emit, error) {
	parts := splitTop(line, ",")

	if len(parts) == 1 { // 裸域名 → 广告拦截
		domain := parts[0]
		if domain == "" {
			return nil, nil
		}
		ctx.Need(ReqBase)
		return []Emit{{"Rule", "DOMAIN," + domain + ",REJECT,extended-matching,pre-matching"}}, nil
	}

	kind := strings.ToUpper(parts[0])
	if alias, ok := ruleAliases[kind]; ok {
		kind = alias
	}
	if logicOps[kind] {
		return logicRule(ctx, parts)
	}
	if !allowedType(kind) {
		return nil, unsupported("unsupported-rule", "未核验的规则类型 %s", parts[0])
	}
	if len(parts) < 2 {
		return nil, unsupported("unsupported-rule", "规则缺少取值")
	}

	value := parts[1]
	policy := "REJECT"
	if len(parts) > 2 {
		policy = strings.ToUpper(parts[2])
	}
	extra := make([]string, 0, len(parts))
	for _, item := range parts[3:] {
		extra = append(extra, strings.ToUpper(item))
	}

	if kind == "URL-REGEX" && isMapLocalPolicy(policy) {
		return ruleAsMapLocal(ctx, value, policy), nil
	}
	text, err := formatRule(ctx, kind, value, policy, extra)
	if err != nil {
		return nil, err
	}
	return []Emit{{"Rule", text}}, nil
}

func isMapLocalPolicy(policy string) bool {
	_, ok := mapLocalByPolicy[policy]
	return ok
}

func formatRule(ctx *Ctx, kind, value, policy string, extra []string) (string, error) {
	if !modulePolicies[policy] {
		return "", unsupported("rule-policy", "Surge 模块 [Rule] 不允许 %s 策略", policy)
	}

	var options []string
	switch {
	case domainTypes[kind]:
		options = append(options, "extended-matching")
	case ipTypes[kind]:
		options = append(options, "no-resolve")
	}
	// 源文件里的 extended-matching / no-resolve / pre-matching 不照搬，按类型重建
	for _, item := range extra {
		switch strings.ToLower(item) {
		case "extended-matching", "no-resolve", "pre-matching":
			continue
		}
		options = append(options, item)
	}

	if kind == "DOMAIN-WILDCARD" || kind == "PROCESS-NAME" {
		ctx.Need(ReqLatest)
	}
	if kind == "URL-REGEX" {
		ctx.Need(ReqModern)
	} else {
		ctx.Need(ReqBase)
	}

	if rejectPolicies[policy] {
		options = append(options, "pre-matching")
		ctx.Need(ReqModern)
	}

	return strings.Join(append([]string{kind, value, policy}, options...), ","), nil
}

func ruleAsMapLocal(ctx *Ctx, pattern, policy string) []Emit {
	local := mapLocalByPolicy[policy]
	ctx.Need(ReqBase)
	if local.tinyGif {
		return []Emit{{"Map Local", pattern + " data-type=tiny-gif status-code=200"}}
	}
	line := pattern + ` data-type=text data="` + local.body + `" status-code=200 ` +
		`header="Content-Type:` + local.contentType + `"`
	return []Emit{{"Map Local", line}}
}

// ---------------------------------------------------------------------------
// 逻辑规则：AND / OR / NOT 的括号结构原样重建
// ---------------------------------------------------------------------------

type matcher struct {
	kind     string
	value    string
	children []*matcher
}

func (m *matcher) types() map[string]bool {
	if len(m.children) > 0 {
		out := map[string]bool{}
		for _, child := range m.children {
			for kind := range child.types() {
				out[kind] = true
			}
		}
		return out
	}
	return map[string]bool{strings.ToUpper(m.kind): true}
}

// logicParser 解析 ((DOMAIN-KEYWORD,x),(OR,((DOMAIN-SUFFIX,a),(DOMAIN,b))))。
type logicParser struct {
	text string
	pos  int
}

func (p *logicParser) parse() ([]*matcher, error) {
	items, err := p.parseList()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.text) {
		return nil, unsupported("rule-logic", "逻辑规则尾部有多余内容：%s", p.text[p.pos:])
	}
	return items, nil
}

func (p *logicParser) parseList() ([]*matcher, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	first, err := p.parseItem()
	if err != nil {
		return nil, err
	}
	items := []*matcher{first}
	p.skipSpace()
	for p.pos < len(p.text) && p.text[p.pos] == ',' {
		p.pos++
		item, err := p.parseItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		p.skipSpace()
	}
	return items, p.expect(')')
}

func (p *logicParser) parseItem() (*matcher, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	head := p.readUntil(",)")
	if logicOps[strings.ToUpper(head)] {
		if err := p.expect(','); err != nil {
			return nil, err
		}
		children, err := p.parseList()
		if err != nil {
			return nil, err
		}
		return &matcher{kind: strings.ToUpper(head), children: children}, p.expect(')')
	}
	if err := p.expect(','); err != nil {
		return nil, err
	}
	value := p.readUntil(")")
	return &matcher{kind: head, value: value}, p.expect(')')
}

func (p *logicParser) readUntil(stops string) string {
	start, depth := p.pos, 0
	for p.pos < len(p.text) {
		ch := p.text[p.pos]
		switch {
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			if depth == 0 {
				return strings.TrimSpace(p.text[start:p.pos])
			}
			depth--
		case depth == 0 && strings.IndexByte(stops, ch) >= 0:
			return strings.TrimSpace(p.text[start:p.pos])
		}
		p.pos++
	}
	return strings.TrimSpace(p.text[start:p.pos])
}

func (p *logicParser) skipSpace() {
	for p.pos < len(p.text) && isSpace(p.text[p.pos]) {
		p.pos++
	}
}

func (p *logicParser) expect(char byte) error {
	p.skipSpace()
	if p.pos >= len(p.text) || p.text[p.pos] != char {
		return unsupported("rule-logic", "逻辑规则缺少 %q", string(char))
	}
	p.pos++
	return nil
}

func formatMatcher(node *matcher) string {
	if len(node.children) > 0 {
		parts := make([]string, 0, len(node.children))
		for _, child := range node.children {
			parts = append(parts, formatMatcher(child))
		}
		return "(" + node.kind + ",(" + strings.Join(parts, ",") + "))"
	}

	kind := node.kind
	if alias, ok := ruleAliases[strings.ToUpper(kind)]; ok {
		kind = alias
	}
	var options []string
	switch {
	case domainTypes[strings.ToUpper(kind)]:
		options = append(options, "extended-matching")
	case ipTypes[strings.ToUpper(kind)]:
		options = append(options, "no-resolve")
	}
	suffix := ""
	if len(options) > 0 {
		suffix = "," + strings.Join(options, ",")
	}
	return "(" + kind + "," + node.value + suffix + ")"
}

func logicRule(ctx *Ctx, parts []string) ([]Emit, error) {
	if len(parts) < 3 {
		return nil, unsupported("rule-logic", "逻辑规则不完整")
	}

	engine := strings.ToUpper(parts[0])
	policy := strings.ToUpper(parts[len(parts)-1])
	body := strings.TrimSpace(strings.Join(parts[1:len(parts)-1], ","))

	nodes, err := (&logicParser{text: body}).parse()
	if err != nil {
		return nil, err
	}
	if !modulePolicies[policy] {
		return nil, unsupported("rule-policy", "Surge 模块 [Rule] 不允许 %s 策略", policy)
	}

	kinds := map[string]bool{}
	for _, node := range nodes {
		for kind := range node.types() {
			kinds[kind] = true
		}
	}
	var unknown []string
	for kind := range kinds {
		if !allowedType(kind) && !logicOps[kind] {
			unknown = append(unknown, kind)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, unsupported("unsupported-rule", "逻辑规则里有未核验类型 %v", unknown)
	}

	parts2 := make([]string, 0, len(nodes))
	for _, node := range nodes {
		parts2 = append(parts2, formatMatcher(node))
	}
	text := engine + ",(" + strings.Join(parts2, ",") + ")," + policy
	if rejectPolicies[policy] && !kinds["PROCESS-NAME"] {
		text += ",pre-matching"
		ctx.Need(ReqModern)
	}
	return []Emit{{"Rule", text}}, nil
}
