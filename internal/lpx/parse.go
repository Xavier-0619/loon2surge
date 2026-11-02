package lpx

// .lpx 的顶层结构解析：头部 #! 元数据 + 各 section 的行（带行号）。

import (
	"regexp"
	"strings"
)

var sectionSpace = regexp.MustCompile(`[\s_]+`)

// NormalizeSection 把 [MitM]、[IP-Rewrite]、[ip rewrite] 统一成内部 key。
func NormalizeSection(title string) string {
	return strings.ToUpper(sectionSpace.ReplaceAllString(strings.TrimSpace(title), "-"))
}

// PluginMeta 只取头部的某个 #! 字段，遇到第一个 [section] 就停。
//
// 同步时用它给落盘文件命名、提取图标，此时还不该付整个解析的代价；调用方
// 要自己先去掉 BOM（见 upstream.decode）。取不到返回空串。
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
		if rest, found := strings.CutPrefix(line, "#!"); found {
			key, value, _ := strings.Cut(rest, "=")
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
