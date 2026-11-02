package kelee

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// 提交信息：标题只放北京日期，正文写清"这次动过哪些 Surge 模块、可莉那边什么时候更新的"。
//
// 每条明细都是同一个形状：`- <可莉更新时间> · <模块>`。时间在前、模块在后，
// 竖着扫一眼就能对时间；改名那几行把"旧 → 新"放在模块位上。
// 时间一律用可莉 list.json 里的 date，而不是本机同步时刻——用户关心的是上游什么时候更新。

const commitLineLimit = 60

// beijing 固定用东八区，runner 本身是 UTC，不能靠本地时区。
var beijing = time.FixedZone("CST", 8*60*60)

// unknownDate 用于上游没给日期的情况（例如上一轮就没记到），保持每行都有时间位。
const unknownDate = "未知时间"

// changeSet 是一轮同步的结果汇总，专门给提交信息用。
type changeSet struct {
	entries []Entry
	added   []string
	updated []string
	renamed []Rename
	removed []Entry
	failed  []string
	now     time.Time
}

// commitMessage 生成完整提交信息（标题 + 空行 + 正文）。
func commitMessage(c changeSet) string {
	dates := make(map[string]string, len(c.entries))
	for _, entry := range c.entries {
		dates[entry.Stem] = entry.Date
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔄 自动更新 %s\n\n", c.now.In(beijing).Format("2006-01-02"))
	fmt.Fprintf(&b, "新增 %d · 更新 %d · 重命名 %d · 删除 %d · 失败 %d · 合计 %d\n",
		len(c.added), len(c.updated), len(c.renamed), len(c.removed), len(c.failed), len(c.entries))

	writeGroup(&b, "新增模块", "", moduleLines(c.added, dates))
	writeGroup(&b, "更新模块", "", moduleLines(c.updated, dates))
	writeGroup(&b, "重命名", "（文件名跟随 #!name）", renameLines(c.renamed))
	writeGroup(&b, "删除模块", "（上游已下架）", removedLines(c.removed))

	fmt.Fprintf(&b, "\n同步时间：%s（北京时间）\n", c.now.In(beijing).Format("2006-01-02 15:04:05"))
	return b.String()
}

// writeGroup 渲染一节；明细超过上限就折叠，免得首次全量时提交信息比代码还长。
func writeGroup(b *strings.Builder, label, note string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s %d 个%s：\n", label, len(lines), note)
	for _, line := range head(lines, commitLineLimit) {
		b.WriteString("- " + line + "\n")
	}
	if len(lines) > commitLineLimit {
		fmt.Fprintf(b, "… 其余 %d 个略\n", len(lines)-commitLineLimit)
	}
}

// moduleLines 每条：`2026-10-02 13:57:49 · 前程无忧去广告.sgmodule`
func moduleLines(stems []string, dates map[string]string) []string {
	sorted := sortedStems(stems)
	lines := make([]string, 0, len(sorted))
	for _, stem := range sorted {
		lines = append(lines, stamp(dates[stem])+" · "+stem+".sgmodule")
	}
	return lines
}

// renameLines 每条：`2026-10-02 13:57:49 · 51Job_remove_ads → 前程无忧去广告`
func renameLines(renamed []Rename) []string {
	sorted := make([]Rename, len(renamed))
	copy(sorted, renamed)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].From != sorted[j].From {
			return sorted[i].From < sorted[j].From
		}
		return sorted[i].To < sorted[j].To
	})

	lines := make([]string, 0, len(sorted))
	for _, change := range sorted {
		lines = append(lines, stamp(change.Date)+" · "+change.From+" → "+change.To)
	}
	return lines
}

// removedLines 每条：`2026-10-02 13:57:49 · Old_Plugin.sgmodule`
func removedLines(removed []Entry) []string {
	sorted := make([]Entry, len(removed))
	copy(sorted, removed)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Stem < sorted[j].Stem })

	lines := make([]string, 0, len(sorted))
	for _, entry := range sorted {
		lines = append(lines, stamp(entry.Date)+" · "+entry.Stem+".sgmodule")
	}
	return lines
}

func stamp(date string) string {
	if date = strings.TrimSpace(date); date == "" {
		return unknownDate
	}
	return date
}

// ToString 给改名记录一个稳定的排序键。
func (r Rename) ToString() string { return r.From + "\x00" + r.To }

func sortedStems(stems []string) []string {
	out := append([]string(nil), stems...)
	sort.Strings(out)
	return out
}

// writeCommitMessage 落盘到 action 指定的路径，交给 git commit -F 使用。
func writeCommitMessage(path, message string) error {
	return os.WriteFile(path, []byte(message), 0o644)
}
