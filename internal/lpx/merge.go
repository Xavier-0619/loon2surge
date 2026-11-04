package lpx

// 同名插件融合。
//
// 上游会把同一个 App 的去广告规则拆成两三个插件，但它们都写同一个 #!name
// （可莉那边「挖财记账去广告」就有两份，各自补一半接口）。若按名字落盘，
// 后转的那份会把前一份覆盖掉，规则白丢一半；若按 `-2` 分开落盘，用户要装
// 两次才完整，还会在两个模块里各自重复一遍同样的规则。
//
// 所以转换前先合并：section 按行并集、完全相同的行只留一次，头部取主源
// 再把其余源独有的字段补齐。合并后行号是重排的，诊断靠 origin 表映射回
// 真正的来源，见 Converter.locate。

import (
	"slices"
	"strings"
)

// Source 是一份待转换的插件全文。
type Source struct {
	// Name 只在诊断里说明"这条来自哪份"，可以留空（标准输入、单文件）。
	Name string
	Text string
}

// ConvertSources 转换一组同名插件；只有一份时等价于 ConvertText。
func ConvertSources(sources []Source, options Options) Result {
	if len(sources) == 0 {
		return Result{
			Name:  "Untitled",
			Notes: []Note{{Code: "empty-input", Message: "没有可转换的输入", Fatal: true}},
		}
	}
	plugin := mergePlugins(sources)
	ctx := newCtx(options)
	ctx.platform = plugin.Meta["system"]
	return (&Converter{plugin: plugin, ctx: ctx}).run()
}

func mergePlugins(sources []Source) *Plugin {
	if len(sources) == 1 {
		return parsePlugin(strings.TrimPrefix(sources[0].Text, "\ufeff"))
	}

	parsed := make([]*Plugin, len(sources))
	names := make([]string, len(sources))
	for index, source := range sources {
		parsed[index] = parsePlugin(strings.TrimPrefix(source.Text, "\ufeff"))
		names[index] = source.Name
	}

	plugin := &Plugin{
		Meta:     map[string]string{},
		Sections: map[string][]Line{},
		origin:   map[int]string{},
		sources:  names,
	}
	plugin.Meta, plugin.MetaKeys = mergeMeta(parsed)

	// 行号在这里重新编排：合并后的文本并不存在，诊断要能指回来源，
	// 靠的是 origin 表而不是行号本身。
	line := 0
	for _, section := range mergeOrder(parsed) {
		var lines []Line
		seen := map[string]bool{}
		for index, item := range parsed {
			for _, row := range item.Sections[section] {
				// 同一个接口常在两份里各写一遍，动作也一模一样；
				// 留着只会让产物里出现两行等价的 Map Local。
				key := strings.Join(strings.Fields(row.Text), " ")
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				line++
				lines = append(lines, Line{No: line, Text: row.Text})
				plugin.origin[line] = names[index]
			}
		}
		if len(lines) == 0 {
			continue
		}
		plugin.Order = append(plugin.Order, section)
		plugin.Sections[section] = lines
	}
	return plugin
}

// mergeOrder 按"主源先、其余源补新节"排出 section 顺序。
func mergeOrder(parsed []*Plugin) []string {
	var order []string
	seen := map[string]bool{}
	for _, item := range parsed {
		for _, section := range item.Order {
			if !seen[section] {
				seen[section] = true
				order = append(order, section)
			}
		}
	}
	return order
}

// mergeMeta 合并头部：主源定调，其余源补空缺。
//
// 只有这几项真的需要"合"而不是"取第一个"：desc / author / tag 是并列信息，
// date 要最新的。icon 跟着主源走——两份插件常给同一 App 配了不同尺寸的图标，
// 混用没有必要。
func mergeMeta(parsed []*Plugin) (map[string]string, []string) {
	meta := map[string]string{}
	var keys []string

	for _, item := range parsed {
		for _, key := range item.MetaKeys {
			if _, seen := meta[key]; !seen {
				keys = append(keys, key)
			}
			if meta[key] == "" {
				meta[key] = item.Meta[key]
			}
		}
	}

	if merged := joinMeta(parsed, "desc", "；", trimStop); merged != "" {
		meta["desc"] = merged
	}
	if merged := joinMeta(parsed, "author", ", ", nil); merged != "" {
		meta["author"] = merged
	}
	if merged := joinMeta(parsed, "tag", ",", nil); merged != "" {
		meta["tag"] = merged
	}
	if latest := latestMeta(parsed, "date"); latest != "" {
		meta["date"] = latest
	}
	return meta, keys
}

// MergeDesc 把多份同名插件的 #!desc 拼成一条完整描述。
//
// 模块头部的 desc 走 mergeMeta 拼接，index.json 走的是上游列表的 desc；
// 不给同步层用同一个拼接规则，网站就会只显示主源那半句。导出的原因是
// upstream 包也要用，避免两处各自实现、规则慢慢漂移。
func MergeDesc(sources []Source) string {
	values := make([]string, len(sources))
	for index, source := range sources {
		values[index] = PluginMeta(strings.TrimPrefix(source.Text, "\ufeff"), "desc")
	}
	return joinValues(values, "；", trimStop)
}

// joinMeta 把各源同名字段去重后拼起来，供 desc / author / tag 这种并列信息用。
func joinMeta(parsed []*Plugin, key, sep string, trim func(string) string) string {
	values := make([]string, len(parsed))
	for index, item := range parsed {
		values[index] = item.Meta[key]
	}
	return joinValues(values, sep, trim)
}

// joinValues 是拼接的唯一实现：去空白、去句末标点、去重、按源顺序连接。
func joinValues(values []string, sep string, trim func(string) string) string {
	var parts []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if trim != nil {
			value = trim(value)
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		parts = append(parts, value)
	}
	return strings.Join(parts, sep)
}

// trimStop 去掉句末标点，拼接时不会出现"……。；移除……"。
func trimStop(value string) string { return strings.TrimRight(value, "。．.") }

// latestMeta 取最新的日期。上游日期是定长时间戳，字符串比较即时间顺序。
func latestMeta(parsed []*Plugin, key string) string {
	best := ""
	for _, item := range parsed {
		if value := strings.TrimSpace(item.Meta[key]); value > best {
			best = value
		}
	}
	return best
}

// merged 记一条"这是多份插件合出来的"诊断。
func (c *Converter) merged() {
	names := slices.DeleteFunc(slices.Clone(c.plugin.sources), func(name string) bool { return name == "" })
	if len(names) < 2 {
		return
	}
	c.ctx.Adapt("merged-source", "同名插件已融合成一版："+strings.Join(names, " + "), 0)
}

// locate 把合并后的行号映射回真正的来源文件。
//
// 合并后行号是重排的，只说"第 5 行"会指错文件——这在排查上游写法时最要命，
// 所以每条带行号的诊断都补上来源名。
func (c *Converter) locate() {
	if len(c.plugin.origin) == 0 {
		return
	}
	for index := range c.ctx.notes {
		if name, ok := c.plugin.origin[c.ctx.notes[index].Line]; ok && name != "" {
			c.ctx.notes[index].Source = name
		}
	}
}
