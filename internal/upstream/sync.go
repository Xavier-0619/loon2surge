package upstream

// 一轮同步的执行者：并发下载 → 串行定名/落盘/转换 → 收拾旧文件。
//
// 顺序是刻意的：文件名取自插件里的 #!name，不先拿到内容就定不了名；
// 而"谁拿原名"必须可复现，所以定名阶段串行、按地址排序，不靠谁先跑完。

import (
	"cmp"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"loon2surge/internal/fsutil"
	"loon2surge/internal/lpx"
)

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

	// 定名按地址排序：同名冲突时谁拿原名只取决于地址，不取决于下载完成顺序。
	order := make([]int, len(remotes))
	for index := range order {
		order[index] = index
	}
	slices.SortFunc(order, func(a, b int) int {
		return strings.Compare(remotes[a].key(), remotes[b].key())
	})

	entries := make([]Entry, len(remotes))
	for _, index := range order {
		entries[index] = s.finish(remotes[index], downloads[index], old[index], write, used)
	}

	if write && prune {
		s.sweep(entries, previous)
	}

	sortEntries(entries)
	return entries
}

// fetchAll 并发把每个插件下下来，顺序与 remotes 一致。
func (s *Syncer) fetchAll(remotes []Remote) []fetched {
	out := make([]fetched, len(remotes))
	sem := make(chan struct{}, s.jobs)

	var wg sync.WaitGroup
	for index, remote := range remotes {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out[index] = s.fetch(remote)
		})
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

	source := decode(got.data)
	entry := Entry{
		Name: remote.Name, Desc: remote.Desc, Icon: lpx.PluginIcon(source), Tags: remote.Tags,
		Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
		Stem:   UniqueStem(fileStem(lpx.PluginName(source), remote.Stem), used),
		LpxSHA: hashOf(got.data),
	}

	changed := old == nil || old.LpxSHA != entry.LpxSHA
	renamed := old != nil && old.Stem != entry.Stem
	if (changed || renamed) && write {
		if _, err := fsutil.WriteFile(s.vault.lpxPath(entry.Stem), got.data); err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
			return s.keep(old, remote, err, used)
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

	// 排除的结论也可以复用：插件字节没变，结论就不会变，不必每轮重跑一遍解析。
	if old != nil && old.LpxSHA == entry.LpxSHA && old.Status == StatusExcluded &&
		old.Converter == lpx.Version {
		entry.Status = StatusExcluded
		entry.Notes = old.Notes
		entry.Converter = lpx.Version
		s.mu.Lock()
		s.excluded = append(s.excluded, entry.Stem)
		s.mu.Unlock()
		return entry
	}

	entry.Converter = lpx.Version
	// 转换器版本必须一起比：插件没变、规则变了的时候，旧模块是错的。
	reusable := old != nil && old.LpxSHA == entry.LpxSHA &&
		old.Status == StatusOK && old.Converter == lpx.Version
	if reusable && (!write || fsutil.Exists(s.vault.modulePath(entry.Stem))) {
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
		// 不该每轮在流水线里亮红灯；真失败才需要人来看。
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
		if _, err := fsutil.WriteFile(s.vault.modulePath(entry.Stem), []byte(result.Text)); err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
			entry.Status = StatusFailed
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
		Stem: UniqueStem(remote.Stem, used), Name: remote.Name, Desc: remote.Desc,
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
	threshold := max(len(previous)*3/10, 3)
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
	slices.SortFunc(gone, func(a, b Entry) int { return strings.Compare(a.Stem, b.Stem) })
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

// sortEntries 按文件名排序，索引与报告都依赖这个顺序。
func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Stem, b.Stem) })
}

func renderNotes(notes []lpx.Note, limit int) []string {
	kept := notes[:min(limit, len(notes))]
	out := make([]string, 0, len(kept))
	for _, note := range kept {
		out = append(out, note.String())
	}
	return out
}
