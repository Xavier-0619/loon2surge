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
// 每天跑一次，标题短才扫得快；细节放正文，翻历史时不用点进去看 diff。
// 时间一律用可莉 list.json 里的 date 字段，而不是本机同步时刻——用户关心的是上游什么时候更新。

const commitLineLimit = 60

// beijing 固定用东八区，runner 本身是 UTC，不能靠本地时区。
var beijing = time.FixedZone("CST", 8*60*60)

// commitMessage 生成完整提交信息（标题 + 空行 + 正文）。
func commitMessage(entries []Entry, added, updated, removed, failed []string, now time.Time) string {
	dates := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.Date != "" {
			dates[entry.Stem] = entry.Date
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔄 自动更新 %s\n\n", now.In(beijing).Format("2006-01-02"))
	fmt.Fprintf(&b, "新增 %d · 更新 %d · 删除 %d · 失败 %d · 合计 %d\n",
		len(added), len(updated), len(removed), len(failed), len(entries))

	for _, group := range []struct {
		label string
		stems []string
	}{
		{"新增模块", sortedStems(added)},
		{"更新模块", sortedStems(updated)},
		{"删除模块", sortedStems(removed)},
	} {
		writeGroup(&b, group.label, group.stems, dates)
	}

	fmt.Fprintf(&b, "\n同步时间：%s（北京时间）\n", now.In(beijing).Format("2006-01-02 15:04:05"))
	return b.String()
}

// writeGroup 日期提到小标题上，只给少数派逐条标注。
//
// 可莉的 date 是站点重建时间：275 条里 273 条都是同一个值。照实逐条标注，
// 正文就会变成几十行重复的"可莉更新 2026-10-02 13:57:49"，所以取占绝对多数的那个。
func writeGroup(b *strings.Builder, label string, stems []string, dates map[string]string) {
	if len(stems) == 0 {
		return
	}

	header := dominantDate(stems, dates)
	b.WriteString("\n" + label)
	if header != "" {
		fmt.Fprintf(b, "（可莉更新 %s）：\n", header)
	} else {
		b.WriteString("：\n")
	}

	for _, stem := range head(stems, commitLineLimit) {
		date := dates[stem]
		switch {
		case date != "" && date != header:
			fmt.Fprintf(b, "- %s.sgmodule · 可莉更新 %s\n", stem, date)
		case date == "":
			fmt.Fprintf(b, "- %s.sgmodule · 上游已下架\n", stem)
		default:
			fmt.Fprintf(b, "- %s.sgmodule\n", stem)
		}
	}

	if len(stems) > commitLineLimit {
		fmt.Fprintf(b, "- 其余 %d 个略\n", len(stems)-commitLineLimit)
	}
}

// dominantDate 返回占绝对多数（≥ 3/4）的日期；日期太分散就返回空串，交给逐条标注。
func dominantDate(stems []string, dates map[string]string) string {
	counts := map[string]int{}
	for _, stem := range stems {
		if date := dates[stem]; date != "" {
			counts[date]++
		}
	}

	best, bestCount := "", 0
	for date, count := range counts {
		if count > bestCount || (count == bestCount && date > best) {
			best, bestCount = date, count
		}
	}
	if bestCount*4 < len(stems)*3 {
		return ""
	}
	return best
}

func sortedStems(stems []string) []string {
	out := append([]string(nil), stems...)
	sort.Strings(out)
	return out
}

// writeCommitMessage 落盘到 action 指定的路径，交给 git commit -F 使用。
func writeCommitMessage(path, message string) error {
	return os.WriteFile(path, []byte(message), 0o644)
}
