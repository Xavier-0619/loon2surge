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
	"slices"
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
	// StatusExcluded 表示插件本身没问题，只是 Surge 模块表达不了（例如规则用 PROXY）。
	// 这类不生成模块，但在报告里与"转换失败"分开写，免得天天报警。
	StatusExcluded = "loon-only"
	// StatusFailed 表示下载或转换失败，没有可用模块。
	StatusFailed = "convert-failed"
)

var badStemChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Remote 是 list.json 里的一条。
type Remote struct {
	// Stem 只是兜底名字（从地址里抠出来的），#!name 缺失时才会用到。
	Stem    string
	Name    string
	Desc    string
	Tags    []string
	Authors []string
	Date    string
	URL     string
}

// key 是插件的稳定身份：地址本身。
//
// 文件名取自 #!name，可莉改个名文件就得跟着改；拿文件名当身份的话，
// 每改一次名都会被当成"删一个 + 加一个"。地址不会变，所以用它。
func (r Remote) key() string {
	if r.URL != "" {
		return r.URL
	}
	return r.Stem
}

// entryKey 与 Remote.key 对齐，供 index.json 这一侧使用。
func entryKey(entry Entry) string {
	if entry.URL != "" {
		return entry.URL
	}
	return entry.Stem
}

// Rename 记录一次文件改名，提交信息里要写清"旧名 → 新名"。
type Rename struct {
	From string
	To   string
	Date string
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

	mu         sync.Mutex // 只保护 blocked：下载阶段是多协程
	added      []string
	updated    []string
	renamed    []Rename
	removed    []Entry
	failed     []string
	excluded   []string
	blocked    bool
	failedKeys map[string]bool
}

func newSyncer(vault *Vault, src *Sources, jobs int, delay float64) *Syncer {
	if jobs < 1 {
		jobs = 1
	}
	if delay < 0 {
		delay = 0
	}
	return &Syncer{
		vault:      vault,
		src:        src,
		opts:       lpx.Options{Category: "可莉插件", Banner: true},
		jobs:       jobs,
		delay:      delay,
		failedKeys: map[string]bool{},
	}
}

// fetched 是一个插件的下载结果。
type fetched struct {
	data []byte
	err  error
}

// usable 表示拿到了正文且确实是插件（不是拦截页）。
func (f fetched) usable() bool { return f.err == nil && looksLikePlugin(f.data) }

// run 分两步走：
//
//  1. 并发下载。文件名取自插件里的 #!name，不先拿到内容就定不了名；
//  2. 串行定名 → 落盘 → 转换。转换一个只要 0.2 毫秒，275 个串起来也就几十毫秒，
//     换来的是文件名可复现：同名冲突时按地址排序决定谁拿原名，不靠谁先跑完。
func (s *Syncer) run(remotes []Remote, previous map[string]Entry, write, prune bool) []Entry {
	downloads := s.fetchAll(remotes)

	old := make([]*Entry, len(remotes))
	for index, remote := range remotes {
		if previousEntry, ok := previous[remote.key()]; ok {
			old[index] = &previousEntry
		}
	}

	// 这一轮要沿用旧文件的插件先把名字占住，否则新插件可能先抢走同一个名字，
	// 两个条目就会指向同一个文件。
	used := make(map[string]bool, len(remotes))
	for index := range remotes {
		if !downloads[index].usable() && old[index] != nil {
			used[strings.ToLower(old[index].Stem)] = true
		}
	}

	order := make([]int, len(remotes))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(a, b int) bool { return remotes[order[a]].key() < remotes[order[b]].key() })

	entries := make([]Entry, len(remotes))
	for _, index := range order {
		entries[index] = s.finish(remotes[index], downloads[index], old[index], write, used)
	}

	if write && prune {
		s.sweep(entries, previous)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Stem < entries[j].Stem })
	return entries
}

// fetchAll 并发把每个插件下下来，顺序与 remotes 一致。
func (s *Syncer) fetchAll(remotes []Remote) []fetched {
	out := make([]fetched, len(remotes))
	sem := make(chan struct{}, s.jobs)
	var wg sync.WaitGroup

	for index, remote := range remotes {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int, remote Remote) {
			defer wg.Done()
			defer func() { <-sem }()
			out[index] = s.fetch(remote)
		}(index, remote)
	}
	wg.Wait()
	return out
}

func (s *Syncer) fetch(remote Remote) fetched {
	if !s.src.alive() {
		s.markBlocked()
		return fetched{err: errors.New("上游已拒绝访问")}
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
		return fetched{err: err}
	}
	if !looksLikePlugin(data) {
		fmt.Fprintf(os.Stderr, "  下载失败 %s：返回的不是插件文本（多半是拦截页）\n", remote.Stem)
		return fetched{err: errors.New("返回的不是插件文本")}
	}
	return fetched{data: data}
}

// finish 定名 + 落盘 + 转换，一个插件一条龙走完，省掉第二遍遍历。
func (s *Syncer) finish(remote Remote, got fetched, old *Entry, write bool, used map[string]bool) Entry {
	if !got.usable() {
		return s.keep(old, remote, got.err, used)
	}

	entry := Entry{
		Name: remote.Name, Desc: remote.Desc, Tags: remote.Tags,
		Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
		Stem:   uniqueStem(fileStem(lpx.PluginName(decode(got.data)), remote.Stem), used),
		LpxSHA: hashOf(got.data),
	}

	changed := old == nil || old.LpxSHA != entry.LpxSHA
	renamed := old != nil && old.Stem != entry.Stem
	if changed || renamed {
		if write {
			if err := writeFile(s.vault.lpxPath(entry.Stem), got.data); err != nil {
				fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
				return s.keep(old, remote, err, used)
			}
		}
	}

	entry = s.convert(entry, got.data, old, write)
	s.record(entry, old, changed, renamed)
	return entry
}

// record 归档这一条属于新增 / 更新 / 改名。
//
// 记账放在转换之后：只有真的产出了模块才算"新增/更新"。否则被排除的插件
// （Surge 表达不了的）会出现在提交信息的模块清单里，指向一个不存在的文件。
// 改名是文件层面的事实，与有没有模块无关，因此单独记。
func (s *Syncer) record(entry Entry, old *Entry, changed, renamed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if renamed {
		s.renamed = append(s.renamed, Rename{From: old.Stem, To: entry.Stem, Date: entry.Date})
	}
	if entry.Status != StatusOK || !changed {
		return
	}
	if old == nil {
		s.added = append(s.added, entry.Stem)
		return
	}
	s.updated = append(s.updated, entry.Stem)
}

func (s *Syncer) convert(entry Entry, data []byte, old *Entry, write bool) Entry {
	entry.Status = StatusOK

	// 排除的结论也可以复用：插件字节没变，结论就不会变，不必每天重跑一遍解析。
	if old != nil && old.LpxSHA == entry.LpxSHA && old.Status == StatusExcluded {
		entry.Status = StatusExcluded
		entry.Notes = old.Notes
		s.mu.Lock()
		s.excluded = append(s.excluded, entry.Stem)
		s.mu.Unlock()
		return entry
	}

	reusable := old != nil && old.LpxSHA == entry.LpxSHA && old.Status == StatusOK
	if reusable && (!write || fileExists(s.vault.modulePath(entry.Stem))) {
		entry.ModuleSHA = old.ModuleSHA
		entry.Notes = old.Notes
		entry.Adapted = old.Adapted
		entry.Review = old.Review
		return entry
	}

	result := lpx.ConvertText(decode(data), s.opts)
	if !result.OK() {
		if write {
			_ = os.Remove(s.vault.modulePath(entry.Stem))
		}
		// 排除与失败要分开：排除是"Surge 模块表达不了"，属于正常结论，
		// 不该每天在流水线里亮红灯；真失败才需要人来看。
		if result.Excluded() {
			entry.Status = StatusExcluded
			entry.Notes = []string{result.ExcludeReason()}
			s.mu.Lock()
			s.excluded = append(s.excluded, entry.Stem)
			s.mu.Unlock()
			return entry
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
	entry.Adapted = lpx.Summarize(result.Adapted())
	entry.Review = reviewSummary(result)
	if write {
		if err := writeFile(s.vault.modulePath(entry.Stem), []byte(result.Text)); err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
			entry.Status = StatusFailed
			return entry
		}
	}
	return entry
}

// reviewSummary 把"要人看一眼"的诊断压成几行，没法转换的行数排在最前面。
func reviewSummary(result lpx.Result) []string {
	lines := lpx.Summarize(result.NeedsReview())
	if result.Skipped > 0 {
		lines = append([]string{
			fmt.Sprintf("%d 行无法自动转换，原文注释在文件末尾", result.Skipped),
		}, lines...)
	}
	return lines
}

// keep 下载失败时不删除既有产物，留一条记录说明这轮没跟上。
func (s *Syncer) keep(old *Entry, remote Remote, cause error, used map[string]bool) Entry {
	s.failedKeys[remote.key()] = true
	note := "下载失败，尚未入库"
	if cause != nil {
		note = cause.Error()
	}

	if old != nil {
		s.failed = append(s.failed, old.Stem+"（沿用上一版）")
		return *old
	}

	s.failed = append(s.failed, remote.Stem+"（从未下载成功）")
	return Entry{
		Stem: uniqueStem(remote.Stem, used), Name: remote.Name, Desc: remote.Desc,
		Tags: remote.Tags, Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
		Status: StatusFailed, Notes: []string{note},
	}
}

// sweep 收拾旧文件：上游下架的、以及这一轮改了名的。
//
// 以前文件名 = 身份，删除只要比对文件名；现在文件名来自 #!name，会随上游改名，
// 所以要按地址（key）判断"这个插件还在不在"，再按文件名判断"旧文件还要不要"。
func (s *Syncer) sweep(entries []Entry, previous map[string]Entry) {
	live := make(map[string]string, len(entries)) // key → 这一轮的文件名
	liveStems := make(map[string]bool, len(entries))
	for _, entry := range entries {
		live[entryKey(entry)] = entry.Stem
		liveStems[entry.Stem] = true
	}

	var gone []Entry   // 上游下架
	var stale []string // 旧文件名，改名或下架留下的
	for key, old := range previous {
		stem, ok := live[key]
		if !ok {
			gone = append(gone, old)
			if old.Stem != "" && !liveStems[old.Stem] {
				stale = append(stale, old.Stem)
			}
			continue
		}
		// 改名：同一个插件换了文件名，旧文件必删——但要确认这个名字没被别的插件接手
		if old.Stem != "" && old.Stem != stem && !liveStems[old.Stem] {
			stale = append(stale, old.Stem)
		}
	}

	// 一次少太多说明列表不完整，本轮不删"下架"的；改名是逐个确认过的，照删。
	threshold := len(previous) * 3 / 10
	if threshold < 3 {
		threshold = 3
	}
	if len(gone) > threshold {
		fmt.Fprintf(os.Stderr, "! 上游一次少了 %d/%d 个插件，疑似列表不完整，本轮不删这些文件\n",
			len(gone), len(previous))
		keepStems := make(map[string]bool, len(gone))
		for _, entry := range gone {
			keepStems[entry.Stem] = true
		}
		stale = slices.DeleteFunc(stale, func(stem string) bool { return keepStems[stem] })
		gone = nil
	}

	for _, stem := range stale {
		_ = os.Remove(s.vault.lpxPath(stem))
		_ = os.Remove(s.vault.modulePath(stem))
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].Stem < gone[j].Stem })
	s.removed = gone
}

func (s *Syncer) markBlocked() {
	s.mu.Lock()
	s.blocked = true
	s.mu.Unlock()
}

// outcome 是一轮同步的结果汇总。
type outcome struct {
	added    []string
	updated  []string
	renamed  []Rename
	removed  []Entry
	failed   []string
	excluded []string
	blocked  bool
}

func (s *Syncer) stats() outcome {
	return outcome{
		added: s.added, updated: s.updated, renamed: s.renamed,
		removed: s.removed, failed: s.failed, excluded: s.excluded, blocked: s.blocked,
	}
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
	fs.StringVar(&cfg.only, "only", "", "只处理这些插件（文件名、地址或上一轮的模块名，逗号分隔）")
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

	remotes = filterRemotes(remotes, cfg, previous)
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
			touched[entryKey(entry)] = true
		}
		for key, entry := range previous {
			if !touched[key] {
				entries = append(entries, entry)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Stem < entries[j].Stem })
	}

	now := time.Now()
	syncedAt := now.UTC().Format("2006-01-02 15:04:05") + " UTC"
	result := syncer.stats()

	// 全站被拦且一无所获时，连索引都不该动，别让空结果覆盖掉旧索引；
	// 插件没变化也不写，否则 synced_at 一变，git 里每天多一个空提交
	if !cfg.dryRun && contentChanged(entries, previous) &&
		!(result.blocked && len(result.added) == 0 && len(result.updated) == 0 && len(result.renamed) == 0) {
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
		if err := writeCommitMessage(cfg.commitFile, message); err != nil {
			fmt.Fprintf(os.Stderr, "! 提交信息写入失败：%v\n", err)
		}
	}
	if result.blocked {
		return 3
	}
	if cfg.maxFailures >= 0 && len(result.failed) > cfg.maxFailures {
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

// filterRemotes 处理 --only：三种写法都认——地址、地址里的文件名、上一轮落盘的名字。
func filterRemotes(remotes []Remote, cfg config, previous map[string]Entry) []Remote {
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
			if wanted[remote.Stem] || wanted[remote.URL] || wanted[previous[remote.key()].Stem] {
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
	result := syncer.stats()
	added, updated, removed, failed, renamed, blocked :=
		result.added, result.updated, result.removed, result.failed, result.renamed, result.blocked

	// 排除（Surge 表达不了）与失败（真出问题）分开列，否则每天都有一堆"失败"。
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
	if cfg.dryRun {
		prefix = "（dry-run，未写入）"
	}
	fmt.Printf("%s共 %d 个插件：新增 %d，更新 %d，重命名 %d，删除 %d，失败 %d，Loon 专用 %d，需人工确认 %d，已适配 %d\n",
		prefix, len(entries), len(added), len(updated), len(renamed), len(removed), len(failed),
		len(loonOnly), len(reviewed), len(adapted))
	fmt.Printf("统计：新增 %d · 更新 %d · 重命名 %d · 删除 %d · 失败 %d · Loon 专用 %d · 需人工确认 %d · 已适配 %d · 合计 %d\n",
		len(added), len(updated), len(renamed), len(removed), len(failed), len(loonOnly),
		len(reviewed), len(adapted), len(entries))

	for _, group := range []struct {
		label string
		names []string
	}{
		{"+", added}, {"~", updated}, {"→", renameList(renamed)}, {"-", stemsOf(removed)},
	} {
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
		fmt.Sprintf("- 新增 %d · 更新 %d · 重命名 %d · 删除 %d · 转换失败 %d · 未生成 %d",
			len(added), len(updated), len(renamed), len(removed), len(bad), len(loonOnly)),
		fmt.Sprintf("- 需人工确认 %d · 已自动适配 %d", len(reviewed), len(adapted)),
		"- 同步时间：" + syncedAt,
	}
	if len(renamed) > 0 {
		summary = append(summary, "", fmt.Sprintf("### 文件改名 %d（文件名跟随插件里的 #!name）", len(renamed)), "")
		for _, change := range headRenames(renamed, 20) {
			summary = append(summary, fmt.Sprintf("- `%s` → `%s`", change.From, change.To))
		}
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

func headRenames(renamed []Rename, limit int) []Rename {
	if len(renamed) > limit {
		return renamed[:limit]
	}
	return renamed
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
