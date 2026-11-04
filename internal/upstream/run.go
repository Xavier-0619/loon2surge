package upstream

// `loon2surge sync` 的主流程：拿列表 → 过滤 → 同步 → 落索引 → 报告。

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// Config 是一次同步的全部可调项，对应命令行上的同名标志。
type Config struct {
	Root        string
	IndexURL    string
	IndexFile   string
	Mirror      string
	Fetcher     string
	CommitFile  string
	Only        string
	Limit       int
	Jobs        int
	Delay       float64
	MaxFailures int
	DryRun      bool
	Probe       bool

	// stale 表示本轮用的是仓库里的旧列表，不是刚拉到的。
	stale bool
}

// Run 执行一次同步，返回进程退出码。
func Run(cfg Config) int {
	vault := NewVault(cfg.Root)
	sources, err := NewSourcesWith(cfg.Mirror, cfg.Fetcher)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}
	fmt.Printf("下载通道：%s\n", sources.channel())

	if cfg.Probe {
		return probe(sources, cfg)
	}

	list, err := readList(sources, cfg)
	previous := vault.ReadState()

	var remotes []Remote
	if err == nil {
		remotes, err = loadRemotes(list)
	}

	// 在线列表拿不到时，退回仓库里上一轮的 index.json：可能旧一天，
	// 但插件文件还能对齐，总比整轮什么都不做强。这时一律不删文件。
	if err != nil {
		if len(previous) == 0 {
			fmt.Fprintf(os.Stderr, "✗ 拿不到插件列表：%v\n", err)
			writeSummary([]string{"## 同步失败", "", fmt.Sprintf("读不到 `%s`：`%v`", cfg.IndexURL, err)})
			return 3
		}
		remotes = remotesFromState(previous)
		cfg.stale = true
		fmt.Fprintf(os.Stderr, "! 拿不到在线列表（%v），改用仓库里上一轮的 index.json，本轮不删插件\n", err)
	}

	remotes = filterRemotes(remotes, cfg, previous)
	if len(remotes) == 0 {
		fmt.Fprintln(os.Stderr, "✗ 列表为空，什么都不做")
		return 3
	}

	partial := cfg.Only != "" || cfg.Limit > 0 || cfg.stale
	syncer := newSyncer(vault, sources, cfg.Jobs, cfg.Delay)
	entries := syncer.run(remotes, previous, !cfg.DryRun, !partial)

	if partial { // 调试模式不能顺手删掉没处理的部分
		touched := make(map[string]bool, len(entries))
		for _, entry := range entries {
			touched[entryKey(entry)] = true
		}
		for key, entry := range previous {
			if !touched[key] {
				entries = append(entries, entry)
			}
		}
		sortEntries(entries)
	}

	now := time.Now()
	syncedAt := now.UTC().Format("2006-01-02 15:04:05") + " UTC"
	result := syncer.stats()

	// 全站被拦且一无所获时，连索引都不该动，别让空结果覆盖掉旧索引；
	// 插件没变化也不写，否则 synced_at 一变，git 里每轮多一个空提交
	idle := result.blocked && len(result.added) == 0 && len(result.updated) == 0 && len(result.renamed) == 0
	if !cfg.DryRun && !idle && contentChanged(entries, previous) {
		if _, err := vault.WriteState(entries, syncedAt); err != nil {
			fmt.Fprintf(os.Stderr, "! index.json 写入失败：%v\n", err)
		}
		if _, err := vault.WriteTable(entries, syncedAt); err != nil {
			fmt.Fprintf(os.Stderr, "! INDEX.md 写入失败：%v\n", err)
		}
	}

	report(syncer, entries, cfg, syncedAt)

	// 提交信息由程序生成：模块清单和可莉的更新时间只有这里知道，
	// 让 shell 去 grep 日志只会得到一行统计。
	if cfg.CommitFile != "" && !cfg.DryRun {
		message := commitMessage(changeSet{
			entries:  entries,
			added:    result.added,
			updated:  result.updated,
			renamed:  result.renamed,
			removed:  result.removed,
			failed:   result.failed,
			excluded: result.excluded,
			now:      now,
		})
		if err := writeCommitMessage(cfg.CommitFile, message); err != nil {
			fmt.Fprintf(os.Stderr, "! 提交信息写入失败：%v\n", err)
		}
	}
	if result.blocked {
		return 3
	}
	if cfg.MaxFailures >= 0 && len(result.failed) > cfg.MaxFailures {
		return 2
	}
	return 0
}

func readList(sources *Sources, cfg Config) ([]byte, error) {
	if cfg.IndexFile != "" {
		return os.ReadFile(cfg.IndexFile)
	}
	return sources.Fetch(cfg.IndexURL)
}

// remotesFromState 把上一轮的 index.json 还原成待办列表，供索引拉取失败时兜底。
func remotesFromState(state map[string]Entry) []Remote {
	remotes := make([]Remote, 0, len(state))
	for _, entry := range state {
		if entry.URL == "" || entry.Name == "" {
			continue
		}
		remotes = append(remotes, Remote{
			Stem: entry.Stem, Name: entry.Name, Desc: entry.Desc,
			Tags: entry.Tags, Authors: entry.Authors, Date: entry.Date, URL: entry.URL,
		})
	}
	slices.SortFunc(remotes, func(a, b Remote) int { return strings.Compare(a.Stem, b.Stem) })
	return remotes
}

// filterRemotes 处理 --only：三种写法都认——地址、地址里的文件名、上一轮落盘的名字。
func filterRemotes(remotes []Remote, cfg Config, previous map[string]Entry) []Remote {
	out := remotes
	if cfg.Only != "" {
		wanted := make(map[string]bool)
		for name := range strings.SplitSeq(cfg.Only, ",") {
			if name = strings.TrimSpace(name); name != "" {
				wanted[name] = true
			}
		}
		filtered := make([]Remote, 0, len(wanted))
		for _, remote := range remotes {
			if wanted[remote.Stem] || wanted[remote.URL] || wanted[previous[remote.key()].Stem] {
				filtered = append(filtered, remote)
			}
		}
		out = filtered
	}
	if cfg.Limit > 0 && len(out) > cfg.Limit {
		out = out[:cfg.Limit]
	}
	return out
}

func probe(sources *Sources, cfg Config) int {
	fmt.Println("连通性自检")
	ok := true

	list, err := readList(sources, cfg)
	if err != nil {
		ok = false
		fmt.Printf("  ✗ 列表 %s → %v\n", cfg.IndexURL, err)
	} else if remotes, err := loadRemotes(list); err != nil {
		ok = false
		fmt.Printf("  ✗ 列表 %s → %v\n", cfg.IndexURL, err)
	} else {
		fmt.Printf("  ✓ 列表 %s → %d 个插件\n", cfg.IndexURL, len(remotes))
		if len(remotes) > 0 {
			sample := remotes[0]
			if data, err := sources.Fetch(sample.URL); err != nil {
				ok = false
				fmt.Printf("  ✗ 文件 %s → %v\n", sample.URL, err)
			} else {
				fmt.Printf("  ✓ 文件 %s → %d 字节\n", sample.Stem, len(data))
			}
		}
	}

	if !ok {
		fmt.Fprintln(os.Stderr, "\n若报「托管质询」（cf-mitigated=challenge）：Cloudflare 认的是 TLS 指纹，"+
			"\n用 --fetcher python 走 Python 的 OpenSSL 栈，与上游同款项目一致。"+
			"\n若只是 403 没有 challenge：那才是出口 IP 被拒，换一台 runner 重试即可。"+
			"\n想彻底不靠运气，用自建镜像（--mirror 或仓库变量 KLEE_MIRROR）。")
	}
	verdict := "通过"
	if !ok {
		verdict = "失败"
	}
	writeSummary([]string{
		"## 连通性自检", "",
		"- 结果：" + verdict,
		"- 下载通道：" + sources.channel(),
	})
	if ok {
		return 0
	}
	return 3
}
