package kelee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"loon2surge/internal/cliargs"
	"loon2surge/internal/lpx"
)

// 可莉插件中心 → 本仓库的 loon/ + surge/。
//
// 上游的 date 字段是站点重建时间（几百条完全相同），不能当版本号用，
// 所以"对比更新"一律靠下载后的内容哈希：哈希没变就不碰文件，Git 里也就没有噪音。

const (
	// IndexURL 是插件列表地址。
	IndexURL = "https://hub.kelee.one/list.json"
	// Canonical 是插件文件所在的官方站点。
	Canonical = "https://kelee.one"
	// UserAgent 必须是 Loon 客户端自己的 UA。
	//
	// kelee.one 按 UA 放行：手机 Safari、curl、Go 默认 UA 一律 403，
	// 只有 Loon 的 UA 拿得到文件（实测同一 IP 下 403 与 200 只差这一个头）。
	// 所以这行不是"随便伪装一下"，改成浏览器 UA 会整个同步挂掉。
	UserAgent = "Loon/860 CFNetwork/3826.500.111.2.2 Darwin/24.4.0"
	// 请求上只带 UA / Accept / Accept-Language 三个头，细节见 http.go 顶部注释。

	// StatusOK 表示插件与模块都已就绪。
	StatusOK = "ok"
	// StatusFailed 表示下载或转换失败，没有可用模块。
	StatusFailed = "convert-failed"
)

var badStemChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Remote 是 list.json 里的一条。
type Remote struct {
	Stem    string
	Name    string
	Desc    string
	Tags    []string
	Authors []string
	Date    string
	URL     string
}

type rawItem struct {
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Tag    any    `json:"tag"`
	Date   string `json:"date"`
	URL    string `json:"url"`
	Author any    `json:"author"`
}

type rawList struct {
	Lists []rawItem `json:"lists"`
}

// ---------------------------------------------------------------------------
// list.json 解析
// ---------------------------------------------------------------------------

func loadRemotes(data []byte) ([]Remote, error) {
	var doc rawList
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("插件列表不是预期的 JSON：%w", err)
	}

	remotes := make([]Remote, 0, len(doc.Lists))
	used := map[string]bool{}
	for _, item := range doc.Lists {
		target := pluginURL(item.URL)
		name := strings.TrimSpace(item.Name)
		if target == "" || name == "" {
			continue
		}
		remotes = append(remotes, Remote{
			Stem:    uniqueStem(stemOf(target), used),
			Name:    name,
			Desc:    strings.TrimSpace(item.Desc),
			Tags:    stringList(item.Tag),
			Authors: authorNames(item.Author),
			Date:    strings.TrimSpace(item.Date),
			URL:     target,
		})
	}
	return remotes, nil
}

// pluginURL 把 `loon://import?plugin=https%3A%2F%2F…lpx` 解成真实地址。
func pluginURL(value string) string {
	if index := strings.Index(value, "plugin="); index >= 0 {
		value = value[index+len("plugin="):]
	}
	value = strings.TrimSpace(value)
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return ""
}

func stemOf(raw string) string {
	parsed, err := url.Parse(raw)
	path := raw
	if err == nil {
		path = parsed.Path
	}
	tail := path
	if index := strings.LastIndex(tail, "/"); index >= 0 {
		tail = tail[index+1:]
	}
	if index := strings.LastIndex(tail, "."); index > 0 {
		tail = tail[:index]
	}
	stem := strings.Trim(badStemChars.ReplaceAllString(tail, "-"), "-")
	if stem == "" {
		return "unknown"
	}
	return stem
}

func uniqueStem(stem string, used map[string]bool) string {
	candidate, index := stem, 2
	for used[strings.ToLower(candidate)] {
		candidate = fmt.Sprintf("%s-%d", stem, index)
		index++
	}
	used[strings.ToLower(candidate)] = true
	return candidate
}

func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text := toString(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func authorNames(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := toString(record["name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// 同步
// ---------------------------------------------------------------------------

// Syncer 一轮同步的执行者。
type Syncer struct {
	vault *Vault
	src   *Sources
	opts  lpx.Options
	jobs  int
	delay float64

	mu        sync.Mutex
	added     []string
	updated   []string
	removed   []string
	failed    []string
	failedSet map[string]bool
	blocked   bool
}

func newSyncer(vault *Vault, src *Sources, jobs int, delay float64) *Syncer {
	if jobs < 1 {
		jobs = 1
	}
	if delay < 0 {
		delay = 0
	}
	return &Syncer{
		vault:     vault,
		src:       src,
		opts:      lpx.Options{Category: "可莉插件", Banner: true},
		jobs:      jobs,
		delay:     delay,
		failedSet: map[string]bool{},
	}
}

func (s *Syncer) run(remotes []Remote, previous map[string]Entry, write, prune bool) []Entry {
	entries := make([]Entry, len(remotes))
	sem := make(chan struct{}, s.jobs)
	var wg sync.WaitGroup

	for index, remote := range remotes {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int, remote Remote) {
			defer wg.Done()
			defer func() { <-sem }()

			var old *Entry
			if previousEntry, ok := previous[remote.Stem]; ok {
				old = &previousEntry
			}
			entries[index] = s.process(remote, old, write)
		}(index, remote)
	}
	wg.Wait()

	if write && prune {
		keep := make(map[string]bool, len(entries))
		for _, entry := range entries {
			keep[entry.Stem] = true
		}
		s.prune(keep, previous)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Stem < entries[j].Stem })
	return entries
}

// process 下载 + 落盘 + 转换，一个插件一条龙走完，省掉第二遍遍历。
func (s *Syncer) process(remote Remote, old *Entry, write bool) Entry {
	entry := Entry{
		Stem: remote.Stem, Name: remote.Name, Desc: remote.Desc,
		Tags: remote.Tags, Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
	}

	if !s.src.alive() {
		s.markBlocked()
		return s.keep(old, remote)
	}
	if s.delay > 0 {
		time.Sleep(time.Duration(rand.Float64() * s.delay * float64(time.Second)))
	}

	data, err := s.src.Fetch(remote.URL)
	if err != nil {
		var denied *Blocked
		if errors.As(err, &denied) {
			s.markBlocked()
		}
		fmt.Fprintf(os.Stderr, "  下载失败 %s：%v\n", remote.Stem, err)
		return s.keep(old, remote)
	}
	if !looksLikePlugin(data) {
		fmt.Fprintf(os.Stderr, "  下载失败 %s：返回的不是插件文本（多半是拦截页）\n", remote.Stem)
		return s.keep(old, remote)
	}

	entry.LpxSHA = hashOf(data)
	changed := old == nil || old.LpxSHA != entry.LpxSHA
	if changed {
		if write {
			if err := writeFile(s.vault.lpxPath(remote.Stem), data); err != nil {
				fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", remote.Stem, err)
				return s.keep(old, remote)
			}
		}
		s.mu.Lock()
		if old == nil {
			s.added = append(s.added, remote.Stem)
		} else {
			s.updated = append(s.updated, remote.Stem)
		}
		s.mu.Unlock()
	}

	return s.convert(entry, data, old, write)
}

func (s *Syncer) convert(entry Entry, data []byte, old *Entry, write bool) Entry {
	entry.Status = StatusOK

	reusable := old != nil && old.LpxSHA == entry.LpxSHA && old.Status == StatusOK
	if reusable && (!write || fileExists(s.vault.modulePath(entry.Stem))) {
		entry.ModuleSHA = old.ModuleSHA
		entry.Notes = old.Notes
		return entry
	}

	result := lpx.ConvertText(decode(data), s.opts)
	if !result.OK() {
		if write {
			_ = os.Remove(s.vault.modulePath(entry.Stem))
		}
		entry.Status = StatusFailed
		entry.Notes = renderNotes(result.Notes, 8)
		if len(entry.Notes) == 0 {
			entry.Notes = []string{"没有产出任何 section"}
		}
		return entry
	}

	entry.ModuleSHA = hashOf([]byte(result.Text))
	entry.Notes = renderNotes(result.Warnings(), 5)
	if write {
		if err := writeFile(s.vault.modulePath(entry.Stem), []byte(result.Text)); err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
			entry.Status = StatusFailed
			return entry
		}
	}
	return entry
}

// keep 下载失败时不删除既有产物，留一条记录说明这轮没跟上。
func (s *Syncer) keep(old *Entry, remote Remote) Entry {
	s.mu.Lock()
	s.failedSet[remote.Stem] = true
	if old == nil {
		s.failed = append(s.failed, remote.Stem+"（从未下载成功）")
		s.mu.Unlock()
		return Entry{
			Stem: remote.Stem, Name: remote.Name, Desc: remote.Desc,
			Tags: remote.Tags, Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
			Status: StatusFailed, Notes: []string{"下载失败，尚未入库"},
		}
	}
	s.failed = append(s.failed, remote.Stem+"（沿用上一版）")
	s.mu.Unlock()
	return *old
}

// prune 删掉上游下架的插件；一次少太多说明列表不完整，本轮不动文件。
func (s *Syncer) prune(keep map[string]bool, previous map[string]Entry) {
	var gone []string
	for stem := range previous {
		if !keep[stem] {
			gone = append(gone, stem)
		}
	}
	if len(gone) == 0 {
		return
	}
	sort.Strings(gone)

	threshold := len(previous) * 3 / 10
	if threshold < 3 {
		threshold = 3
	}
	if len(gone) > threshold {
		fmt.Fprintf(os.Stderr, "! 上游一次少了 %d/%d 个插件，疑似列表不完整，本轮不删文件\n",
			len(gone), len(previous))
		return
	}

	for _, stem := range gone {
		_ = os.Remove(s.vault.lpxPath(stem))
		_ = os.Remove(s.vault.modulePath(stem))
	}
	s.removed = gone
}

func (s *Syncer) markBlocked() {
	s.mu.Lock()
	s.blocked = true
	s.mu.Unlock()
}

func (s *Syncer) stats() (added, updated, removed, failed []string, blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.added, s.updated, s.removed, s.failed, s.blocked
}

// ---------------------------------------------------------------------------
// 命令行
// ---------------------------------------------------------------------------

type config struct {
	root        string
	indexURL    string
	indexFile   string
	mirror      string
	fetcher     string
	commitFile  string
	only        string
	limit       int
	jobs        int
	delay       float64
	maxFailures int
	dryRun      bool
	probe       bool
	stale       bool // 本轮用的是仓库里的旧列表，不是刚拉到的
}

// Main 是 `loon2surge sync` 的入口。
func Main(argv []string) int {
	var cfg config
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.StringVar(&cfg.root, "root", ".", "仓库根目录")
	fs.StringVar(&cfg.indexURL, "index-url", IndexURL, "插件列表地址")
	fs.StringVar(&cfg.indexFile, "index-file", "", "用本地 list.json 代替在线列表（离线调试）")
	fs.StringVar(&cfg.mirror, "mirror", os.Getenv("KLEE_MIRROR"), "备用站点前缀，例如 https://mirror.example.com")
	fs.StringVar(&cfg.fetcher, "fetcher", "auto", "下载通道：auto（有 python3 就用）/ python / go")
	fs.StringVar(&cfg.commitFile, "commit-file", "", "把提交信息写到这个文件（action 里交给 git commit -F）")
	fs.StringVar(&cfg.only, "only", "", "只处理这些文件名（逗号分隔）")
	fs.IntVar(&cfg.limit, "limit", 0, "只处理前 N 个（调试用）")
	fs.IntVar(&cfg.jobs, "jobs", 8, "并发下载数")
	fs.Float64Var(&cfg.delay, "delay", 0.15, "每个请求前随机等待上限（秒）")
	fs.IntVar(&cfg.maxFailures, "max-failures", 25, "失败超过这个数就返回非零")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "只报告差异，不写文件")
	fs.BoolVar(&cfg.probe, "probe", false, "只做连通性自检")
	flags, _ := cliargs.Split(argv, map[string]bool{
		"root": true, "index-url": true, "index-file": true, "mirror": true,
		"fetcher": true, "commit-file": true, "only": true,
		"limit": true, "jobs": true, "delay": true, "max-failures": true,
	})
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	return runSync(cfg)
}

func runSync(cfg config) int {
	vault := NewVault(cfg.root)
	sources, err := NewSourcesWith(cfg.mirror, cfg.fetcher)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}
	fmt.Printf("下载通道：%s\n", sources.channel())

	if cfg.probe {
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
	stale := false
	if err != nil {
		if len(previous) == 0 {
			fmt.Fprintf(os.Stderr, "✗ 拿不到插件列表：%v\n", err)
			writeSummary([]string{"## 同步失败", "", fmt.Sprintf("读不到 `%s`：`%v`", cfg.indexURL, err)})
			return 3
		}
		remotes = remotesFromState(previous)
		stale = true
		fmt.Fprintf(os.Stderr, "! 拿不到在线列表（%v），改用仓库里上一轮的 index.json，本轮不删插件\n", err)
	}

	remotes = filterRemotes(remotes, cfg)
	if len(remotes) == 0 {
		fmt.Fprintln(os.Stderr, "✗ 列表为空，什么都不做")
		return 3
	}

	partial := cfg.only != "" || cfg.limit > 0 || stale
	cfg.stale = stale
	syncer := newSyncer(vault, sources, cfg.jobs, cfg.delay)
	entries := syncer.run(remotes, previous, !cfg.dryRun, !partial)

	if partial { // 调试模式不能顺手删掉没处理的部分
		touched := make(map[string]bool, len(entries))
		for _, entry := range entries {
			touched[entry.Stem] = true
		}
		for stem, entry := range previous {
			if !touched[stem] {
				entries = append(entries, entry)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Stem < entries[j].Stem })
	}

	now := time.Now()
	syncedAt := now.UTC().Format("2006-01-02 15:04:05") + " UTC"
	added, updated, removed, failed, blocked := syncer.stats()

	// 全站被拦且一无所获时，连索引都不该动，别让空结果覆盖掉旧索引；
	// 插件没变化也不写，否则 synced_at 一变，git 里每天多一个空提交
	if !cfg.dryRun && contentChanged(entries, previous) && !(blocked && len(added) == 0 && len(updated) == 0) {
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
	if cfg.commitFile != "" && !cfg.dryRun {
		message := commitMessage(entries, added, updated, removed, failed, now)
		if err := writeCommitMessage(cfg.commitFile, message); err != nil {
			fmt.Fprintf(os.Stderr, "! 提交信息写入失败：%v\n", err)
		}
	}
	if blocked {
		return 3
	}
	if cfg.maxFailures >= 0 && len(failed) > cfg.maxFailures {
		return 2
	}
	return 0
}

func readList(sources *Sources, cfg config) ([]byte, error) {
	if cfg.indexFile != "" {
		return os.ReadFile(cfg.indexFile)
	}
	return sources.Fetch(cfg.indexURL)
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
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Stem < remotes[j].Stem })
	return remotes
}

func filterRemotes(remotes []Remote, cfg config) []Remote {
	out := remotes
	if cfg.only != "" {
		wanted := map[string]bool{}
		for _, name := range strings.Split(cfg.only, ",") {
			if name = strings.TrimSpace(name); name != "" {
				wanted[name] = true
			}
		}
		filtered := make([]Remote, 0, len(wanted))
		for _, remote := range remotes {
			if wanted[remote.Stem] {
				filtered = append(filtered, remote)
			}
		}
		out = filtered
	}
	if cfg.limit > 0 && len(out) > cfg.limit {
		out = out[:cfg.limit]
	}
	return out
}

func probe(sources *Sources, cfg config) int {
	fmt.Println("连通性自检")
	ok := true

	list, err := readList(sources, cfg)
	if err != nil {
		ok = false
		fmt.Printf("  ✗ 列表 %s → %v\n", cfg.indexURL, err)
	} else if remotes, err := loadRemotes(list); err != nil {
		ok = false
		fmt.Printf("  ✗ 列表 %s → %v\n", cfg.indexURL, err)
	} else {
		fmt.Printf("  ✓ 列表 %s → %d 个插件\n", cfg.indexURL, len(remotes))
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

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

func report(syncer *Syncer, entries []Entry, cfg config, syncedAt string) {
	added, updated, removed, failed, blocked := syncer.stats()

	var bad []Entry
	for _, entry := range entries {
		if entry.Status != StatusOK && !syncer.failedSet[entry.Stem] {
			bad = append(bad, entry)
		}
	}

	prefix := ""
	if cfg.dryRun {
		prefix = "（dry-run，未写入）"
	}
	line := fmt.Sprintf("%s共 %d 个插件：新增 %d，更新 %d，删除 %d，失败 %d",
		prefix, len(entries), len(added), len(updated), len(removed), len(failed))
	fmt.Println(line)
	fmt.Printf("统计：新增 %d · 更新 %d · 删除 %d · 失败 %d · 合计 %d\n",
		len(added), len(updated), len(removed), len(failed), len(entries))

	for _, group := range []struct {
		label string
		names []string
	}{{"+", added}, {"~", updated}, {"-", removed}} {
		if len(group.names) > 0 {
			fmt.Printf("  %s %s%s\n", group.label, strings.Join(head(group.names, 8), ", "), tail(group.names, 8))
		}
	}
	for _, name := range head(failed, 10) {
		fmt.Printf("  ! %s\n", name)
	}
	for _, entry := range headEntries(bad, 10) {
		fmt.Printf("  ! 转换失败 %s：%s\n", entry.Stem, firstNote(entry.Notes))
	}

	summary := []string{
		"## 可莉插件同步",
		"",
		fmt.Sprintf("- 插件总数：**%d**", len(entries)),
		fmt.Sprintf("- 新增 %d · 更新 %d · 删除 %d · 转换失败 %d", len(added), len(updated), len(removed), len(bad)),
		"- 同步时间：" + syncedAt,
	}
	if cfg.stale {
		summary = append(summary, "", "> ℹ️ 本轮没拉到在线列表，用的是仓库里上一轮的 `index.json`，因此没有删除任何插件。")
	}
	if blocked {
		summary = append(summary, "", "> ⚠️ 上游拒绝访问（403），本轮没有下到新内容。见 README「被 Cloudflare 拦截怎么办」。")
	}
	if len(failed) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 下载失败 %d", len(failed)), "")
		for _, name := range head(failed, 20) {
			summary = append(summary, "- `"+name+"`")
		}
	}
	if len(bad) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 未生成模块 %d", len(bad)), "")
		for _, entry := range headEntries(bad, 20) {
			summary = append(summary, fmt.Sprintf("- `%s`：%s", entry.Stem, firstNote(entry.Notes)))
		}
	}
	writeSummary(summary)
}

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

func head(items []string, limit int) []string {
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

func headEntries(items []Entry, limit int) []Entry {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func firstNote(notes []string) string {
	if len(notes) == 0 {
		return "未知原因"
	}
	return notes[0]
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func looksLikePlugin(data []byte) bool {
	head := strings.ToLower(strings.TrimSpace(string(data[:min(400, len(data))])))
	if strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html") {
		return false
	}
	window := data[:min(2000, len(data))]
	return bytes.Contains(window, []byte("#!")) || bytes.Contains(window, []byte("["))
}

// decode 优先按 UTF-8 读；脏字节按替换字符兜底，不让一个插件把整轮同步搞挂。
func decode(data []byte) string {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

func renderNotes(notes []lpx.Note, limit int) []string {
	out := make([]string, 0, limit)
	for _, note := range notes {
		if len(out) >= limit {
			break
		}
		out = append(out, note.String())
	}
	return out
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
