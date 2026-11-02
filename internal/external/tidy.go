package external

// 把上游模块整理成这个仓库的样子。
//
// 边界是 Surge 语法层面的机械修正：头部写法、段与段的空行、section 顺序、
// 完全重复的行、[Script] 行尾多出来的逗号、pattern 里没转义的主机名点、
// MITM 里写了端口。规则内容、正则分支、脚本参数的值一律不碰——拿不准的
// 宁可留原样，也不猜上游想干什么。
//
// 每一步改动都会记进 notes，最后写进清单：谁被改了什么，事后可查。

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"loon2surge/internal/lpx"
)

// sectionRank 给出 section 在 Surge 里的习惯位置；不认识的排最后。
func sectionRank(name string) int {
	for index, known := range lpx.SectionOrder {
		if strings.EqualFold(known, name) {
			return index
		}
	}
	// [Panel] 不在 SectionOrder 里（转换器不产出），Surge 的顺序放在最后。
	return len(lpx.SectionOrder) + 1
}

// scriptParams 是 [Script] 行里会出现的参数名。判断 pattern 的值到哪儿结束要靠它：
// 正则本身可能带逗号（比如 {2,3}），不能见到逗号就切。
var scriptParams = []string{
	"type", "pattern", "requires-body", "max-size", "script-path",
	"script-update-interval", "timeout", "argument", "binary-body-mode",
	"debug", "engine", "interval", "cronexp",
}

// tidy 返回整理后的正文和"动了什么"。
func tidy(text, repo string) (string, []string) {
	var notes []string
	add := func(note string) {
		if !slices.Contains(notes, note) {
			notes = append(notes, note)
		}
	}

	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	raw := strings.Split(text, "\n")
	lines := make([]string, len(raw))
	trimmedLines := 0
	for index, line := range raw {
		lines[index] = strings.TrimRight(line, " \t\u00a0")
		if lines[index] != line && strings.TrimSpace(line) != "" {
			trimmedLines++
		}
	}
	if trimmedLines > 0 {
		add(fmt.Sprintf("去掉 %d 处行尾空白", trimmedLines))
	}

	type section struct {
		name string
		body []string
	}
	var head []string
	var sections []section
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			canonical := canonicalSection(name)
			if canonical != name {
				add("section 名统一成 [" + canonical + "]")
			}
			sections = append(sections, section{name: canonical})
			continue
		}
		if len(sections) == 0 {
			head = append(head, line)
			continue
		}
		sections[len(sections)-1].body = append(sections[len(sections)-1].body, line)
	}

	sorted := true
	for index := 1; index < len(sections); index++ {
		if sectionRank(sections[index-1].name) > sectionRank(sections[index].name) {
			sorted = false
			break
		}
	}
	if !sorted {
		sort.SliceStable(sections, func(i, j int) bool {
			return sectionRank(sections[i].name) < sectionRank(sections[j].name)
		})
		add("section 按 Surge 惯例重排")
	}

	parts := make([]string, 0, len(sections))
	for _, item := range sections {
		body := trimBlank(item.body)
		body = dedupeLines(body, add)
		switch item.name {
		case "Script":
			body = tidyScript(body, add)
		case "MITM":
			body = tidyMITM(body, add)
		}
		body = trimBlank(body)
		if len(body) == 0 {
			add("删掉空的 [" + item.name + "] 段")
			continue
		}
		parts = append(parts, "["+item.name+"]\n"+strings.Join(body, "\n"))
	}

	var out strings.Builder
	if header := tidyHeader(head, repo, add); len(header) > 0 {
		out.WriteString(strings.Join(header, "\n"))
	}
	if len(parts) > 0 {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(strings.Join(parts, "\n\n"))
	}
	return strings.TrimRight(out.String(), "\n") + "\n", notes
}

// tidyHeader 规范头部：`#!key = value` 收成 `#!key=value`、按 MetaOrder 排序、
// 去掉重复与空值；缺 author / category 的补上来源，免得不认识这个模块的人
// 找不到出处。
func tidyHeader(lines []string, repo string, add func(string)) []string {
	type meta struct {
		key   string
		value string
	}
	var metas []meta
	var comments []string
	index := make(map[string]int, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "#!"):
			body := trimmed[2:]
			equal := strings.IndexByte(body, '=')
			if equal < 0 {
				add("头部有写坏的 #! 行")
				comments = append(comments, trimmed)
				continue
			}
			rawKey, rawValue := body[:equal], body[equal+1:]
			key, value := strings.TrimSpace(rawKey), strings.TrimSpace(rawValue)
			if key != rawKey || value != rawValue {
				add("头部 #!key = value 统一成 #!key=value")
			}
			if canonical := canonicalMetaKey(key); canonical != key {
				add("头部字段名统一成 #!" + canonical)
				key = canonical
			}
			lower := strings.ToLower(key)
			if at, ok := index[lower]; ok {
				if metas[at].value == "" && value != "" {
					metas[at].value = value
				}
				add("去掉重复的 #!" + key)
				continue
			}
			index[lower] = len(metas)
			metas = append(metas, meta{key: key, value: value})
		default:
			// 上游在头部写的说明性注释，位置保留在元数据之后。
			comments = append(comments, trimmed)
		}
	}

	fill := func(key, value, why string) {
		lower := strings.ToLower(key)
		if at, ok := index[lower]; ok {
			if metas[at].value != "" {
				return
			}
			metas[at].value = value
		} else {
			index[lower] = len(metas)
			metas = append(metas, meta{key: key, value: value})
		}
		add(why)
	}
	if repo != "" {
		owner, _, _ := strings.Cut(repo, "/")
		fill("author", owner+"[https://github.com/"+repo+"]", "补齐 #!author（标明来源）")
		fill("category", owner, "补齐 #!category（Surge 里好归类）")
		if note := provenance(repo); !slices.Contains(comments, note) {
			comments = append(comments, note)
			add("加一行来源说明")
		}
	}

	sort.SliceStable(metas, func(i, j int) bool {
		return metaRank(metas[i].key) < metaRank(metas[j].key)
	})

	out := make([]string, 0, len(metas)+len(comments))
	for _, item := range metas {
		if item.value == "" {
			add("删掉没有值的 #!" + item.key)
			continue
		}
		out = append(out, "#!"+item.key+"="+item.value)
	}
	return append(out, comments...)
}

func provenance(repo string) string {
	return "# 由 loon2surge 从 " + repo + " 同步整理"
}

// canonicalSection 把 [script] 这种写法收回标准大小写。
func canonicalSection(name string) string {
	for _, known := range lpx.SectionOrder {
		if strings.EqualFold(known, name) {
			return known
		}
	}
	if strings.EqualFold(name, "Panel") {
		return "Panel"
	}
	return name
}

func canonicalMetaKey(key string) string {
	for _, known := range lpx.MetaOrder {
		if strings.EqualFold(known, key) {
			return known
		}
	}
	return key
}

func metaRank(key string) int {
	for index, known := range lpx.MetaOrder {
		if strings.EqualFold(known, key) {
			return index
		}
	}
	return len(lpx.MetaOrder) + 1
}

// tidyScript 只动"写错了但一眼能看出本意"的地方。
func tidyScript(body []string, add func(string)) []string {
	out := make([]string, 0, len(body))
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		if strings.HasSuffix(line, ",") {
			line = strings.TrimSuffix(line, ",")
			add("[Script] 去掉行尾多余的逗号")
		}
		if strings.Contains(line, "requires-body=") {
			before := line
			line = strings.ReplaceAll(line, "requires-body=true", "requires-body=1")
			line = strings.ReplaceAll(line, "requires-body=false", "requires-body=0")
			if line != before {
				add("requires-body 的 true/false 统一写成 1/0")
			}
		}
		if fixed, ok := escapePatternHost(line); ok {
			line = fixed
			add("[Script] 转义 pattern 主机名里的点")
		}
		out = append(out, line)
	}
	return out
}

// escapePatternHost 把 pattern 里 scheme 与主机名之间那些没转义的点补上反斜杠。
//
// `pattern=^https://api.agcplayer.com/` 里的点是正则通配，能匹配
// `apiXagcplayerYcom`；补成 `\.` 才是"这个域名"。只处理主机名部分且只处理
// 形态干净的主机名，路径和带分组的正则一概不碰。
func escapePatternHost(line string) (string, bool) {
	const marker = "pattern="
	at := strings.Index(line, marker)
	if at < 0 {
		return line, false
	}
	start := at + len(marker)
	rest := line[start:]
	end := scriptValueEnd(rest)
	fixed, changed := escapeHostDots(rest[:end])
	if !changed {
		return line, false
	}
	return line[:start] + fixed + rest[end:], true
}

func scriptValueEnd(rest string) int {
	for index := range len(rest) {
		if rest[index] != ',' {
			continue
		}
		tail := strings.TrimLeft(rest[index+1:], " \t")
		for _, name := range scriptParams {
			if strings.HasPrefix(tail, name+"=") {
				return index
			}
		}
	}
	return len(rest)
}

func escapeHostDots(pattern string) (string, bool) {
	prefix, host, rest, ok := splitScheme(pattern)
	if !ok || host == "" {
		return pattern, false
	}
	for _, char := range host {
		switch {
		case char == '.' || char == '-' || char == '_' || char == '*':
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		default:
			return pattern, false // 带分组、字符类的正则，改了会变味
		}
	}
	var out strings.Builder
	changed := false
	for index := range len(host) {
		if host[index] == '.' && (index == 0 || host[index-1] != '\\') {
			out.WriteString(`\.`)
			changed = true
			continue
		}
		out.WriteByte(host[index])
	}
	if !changed {
		return pattern, false
	}
	return prefix + out.String() + rest, true
}

func splitScheme(pattern string) (prefix, host, rest string, ok bool) {
	for _, candidate := range []string{
		"^https?://", "https?://", "^https://", "^http://", "https://", "http://",
	} {
		body, found := strings.CutPrefix(pattern, candidate)
		if !found {
			continue
		}
		if slash := strings.IndexByte(body, '/'); slash >= 0 {
			return candidate, body[:slash], body[slash:], true
		}
		return candidate, body, "", true
	}
	return "", "", "", false
}

// tidyMITM 合并多行 hostname、去掉重复项、去掉端口——Surge 的 hostname 只认
// 主机名，写成 `host:port` 整个条目会被忽略，MITM 就静默失效了。
func tidyMITM(body []string, add func(string)) []string {
	var hosts []string
	appendMode := false
	seen := make(map[string]bool, len(body))
	out := make([]string, 0, len(body))
	hostAt := -1

	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "hostname") {
			out = append(out, line)
			continue
		}
		if hostAt < 0 {
			hostAt = len(out)
			out = append(out, "") // 占位，等所有 hostname 收齐再写
		} else {
			add("[MITM] 合并多行 hostname")
		}
		rest := strings.TrimSpace(value)
		if trimmed, found := strings.CutPrefix(rest, "%APPEND%"); found {
			appendMode = true
			rest = strings.TrimSpace(trimmed)
		}
		for _, host := range strings.Split(rest, ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			if bare, changed := stripPort(host); changed {
				add("[MITM] 去掉 hostname 里的端口")
				host = bare
			}
			if seen[host] {
				add("[MITM] 去掉重复的 hostname")
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	if hostAt < 0 {
		return out
	}
	if len(hosts) == 0 {
		add("[MITM] 删掉空的 hostname")
		return append(out[:hostAt], out[hostAt+1:]...)
	}
	line := "hostname = "
	if appendMode {
		line += "%APPEND% "
	}
	out[hostAt] = line + strings.Join(hosts, ", ")
	return out
}

func stripPort(host string) (string, bool) {
	colon := strings.LastIndexByte(host, ':')
	if colon <= 0 || colon == len(host)-1 {
		return host, false
	}
	for _, char := range host[colon+1:] {
		if char < '0' || char > '9' {
			return host, false
		}
	}
	if strings.ContainsRune(host[:colon], ':') {
		return host, false // IPv6 原样留着
	}
	return host[:colon], true
}

func trimBlank(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

func dedupeLines(lines []string, add func(string)) []string {
	seen := make(map[string]bool, len(lines))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, ";") {
			out = append(out, line)
			continue
		}
		if seen[trimmed] {
			add("去掉段内完全重复的行")
			continue
		}
		seen[trimmed] = true
		out = append(out, line)
	}
	return out
}
