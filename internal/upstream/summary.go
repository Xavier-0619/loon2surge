package upstream

// 给人看的同步结果：控制台摘要 + GitHub Job Summary。
//
// 两者共用同一份分类，避免"控制台说失败、页面上说跳过"这种对不上的情况。

import (
	"fmt"
	"os"
	"strings"
)

// report 打印控制台摘要，并追加到 Job Summary（在 Actions 里跑才有）。
func report(syncer *Syncer, entries []Entry, cfg Config, syncedAt string) {
	result := syncer.stats()

	// 排除（Surge 表达不了）与失败（真出问题）分开列，否则每轮都有一堆"失败"。
	// 另外两类单独数：转换器替我们改写过的，和还没能自动处理、要人看一眼的。
	var bad, loonOnly, reviewed, adapted []Entry
	for _, entry := range entries {
		switch {
		case entry.Status == StatusExcluded:
			loonOnly = append(loonOnly, entry)
		case entry.Status != StatusOK && !syncer.failedKeys[entryKey(entry)]:
			bad = append(bad, entry)
		}
		if entry.Status == StatusOK && len(entry.Review) > 0 {
			reviewed = append(reviewed, entry)
		}
		if entry.Status == StatusOK && len(entry.Adapted) > 0 {
			adapted = append(adapted, entry)
		}
	}

	prefix := ""
	if cfg.DryRun {
		prefix = "（dry-run，未写入）"
	}
	fmt.Printf("%s共 %d 个插件：新增 %d，更新 %d，重命名 %d，删除 %d，失败 %d，Loon 专用 %d，需人工确认 %d，已适配 %d\n",
		prefix, len(entries), len(result.added), len(result.updated), len(result.renamed),
		len(result.removed), len(result.failed), len(loonOnly), len(reviewed), len(adapted))
	fmt.Printf("统计：新增 %d · 更新 %d · 重命名 %d · 删除 %d · 失败 %d · Loon 专用 %d · 需人工确认 %d · 已适配 %d · 合计 %d\n",
		len(result.added), len(result.updated), len(result.renamed), len(result.removed),
		len(result.failed), len(loonOnly), len(reviewed), len(adapted), len(entries))

	for _, group := range []struct {
		label string
		names []string
	}{
		{"+", result.added}, {"~", result.updated},
		{"→", renameList(result.renamed)}, {"-", stemsOf(result.removed)},
	} {
		if len(group.names) > 0 {
			fmt.Printf("  %s %s%s\n", group.label, strings.Join(head(group.names, 8), ", "), tail(group.names, 8))
		}
	}
	for _, name := range head(result.failed, 10) {
		fmt.Printf("  ! %s\n", name)
	}
	for _, entry := range headEntries(bad, 10) {
		fmt.Printf("  ! 转换失败 %s：%s\n", entry.Stem, firstNote(entry.Notes))
	}

	summary := []string{
		"## 可莉插件同步",
		"",
		fmt.Sprintf("- 插件总数：**%d**", len(entries)),
		fmt.Sprintf("- 新增 %d · 更新 %d · 重命名 %d · 删除 %d · 转换失败 %d · 未生成 %d",
			len(result.added), len(result.updated), len(result.renamed), len(result.removed), len(bad), len(loonOnly)),
		fmt.Sprintf("- 需人工确认 %d · 已自动适配 %d", len(reviewed), len(adapted)),
		"- 同步时间：" + syncedAt,
	}
	if len(result.renamed) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 文件改名 %d（文件名跟随插件里的 #!name）", len(result.renamed)), "")
		for _, change := range head(result.renamed, 20) {
			summary = append(summary, fmt.Sprintf("- `%s` → `%s`", change.From, change.To))
		}
	}
	if cfg.stale {
		summary = append(summary, "", "> ℹ️ 本轮没拉到在线列表，用的是仓库里上一轮的 `index.json`，因此没有删除任何插件。")
	}
	if result.blocked {
		summary = append(summary, "", "> ⚠️ 上游拒绝访问（403），本轮没有下到新内容。见 README「被 Cloudflare 拦截怎么办」。")
	}
	if len(result.failed) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 下载失败 %d", len(result.failed)), "")
		for _, name := range head(result.failed, 20) {
			summary = append(summary, "- `"+name+"`")
		}
	}
	if len(bad) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 转换失败 %d（需要看一眼）", len(bad)), "")
		for _, entry := range headEntries(bad, 20) {
			summary = append(summary, fmt.Sprintf("- `%s`：%s", entry.Stem, firstNote(entry.Notes)))
		}
	}
	if len(reviewed) > 0 {
		summary = append(summary, "", fmt.Sprintf("### ⚠️ 需人工确认 %d", len(reviewed)), "")
		summary = append(summary, "> 这些模块**已经生成并能导入**，但里面有转换器没能自动处理的原始行（注释在文件末尾），",
			"> 或者运行时行为和 Loon 不完全一样。改完源插件后下一轮会重新判定。", "")
		for _, entry := range headEntries(reviewed, 30) {
			summary = append(summary, fmt.Sprintf("- `%s`：%s", entry.Stem, strings.Join(entry.Review, "；")))
		}
	}
	if len(adapted) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 已自动适配 %d（不需要处理）", len(adapted)), "")
		summary = append(summary, "> 转换器做了等价改写，功能不受影响，只是生成的模块和源文件长得不完全一样。", "")
		for _, entry := range headEntries(adapted, 20) {
			summary = append(summary, fmt.Sprintf("- `%s`：%s", entry.Stem, strings.Join(entry.Adapted, "；")))
		}
		if len(adapted) > 20 {
			summary = append(summary, fmt.Sprintf("- 其余 %d 个略", len(adapted)-20))
		}
	}
	if len(loonOnly) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 未生成模块 %d（Loon 专用，不是故障）", len(loonOnly)), "")
		summary = append(summary, "> 这些插件的内容 Surge 模块表达不出来（通常是规则里写了 `PROXY`：模块规则只允许 `DIRECT` / `REJECT` / `REJECT-TINYGIF`）。",
			"> 原始 `.lpx` 已照常保存，可以在 Loon 里使用；这里不算失败，也不会让流水线变红。", "")
		for _, entry := range headEntries(loonOnly, 20) {
			summary = append(summary, fmt.Sprintf("- `%s`：%s", entry.Name, firstNote(entry.Notes)))
		}
	}
	writeSummary(summary)
}

func renameList(renamed []Rename) []string {
	out := make([]string, 0, len(renamed))
	for _, change := range renamed {
		out = append(out, change.From+"→"+change.To)
	}
	return out
}

func stemsOf(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Stem)
	}
	return out
}

// writeSummary 追加到 Job Summary；本地跑没有这个环境变量，直接跳过。
func writeSummary(lines []string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer handle.Close()
	_, _ = handle.WriteString(strings.Join(lines, "\n") + "\n")
}

func head[T any](items []T, limit int) []T {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func tail(items []string, limit int) string {
	if len(items) > limit {
		return " …"
	}
	return ""
}

func headEntries(items []Entry, limit int) []Entry { return head(items, limit) }

func firstNote(notes []string) string {
	if len(notes) == 0 {
		return "未知原因"
	}
	return notes[0]
}
