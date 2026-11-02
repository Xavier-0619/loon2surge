package upstream

// 一轮同步的执行者：并发下载 → 串行定名/落盘 → 同名合流转换 → 收拾旧文件。
//
// 顺序是刻意的：文件名取自插件里的 #!name，不先拿到内容就定不了名；
// 而"谁拿原名"必须可复现，所以定名阶段串行、按地址排序，不靠谁先跑完。
//
// 转换切成"定名(stage)"与"合流(produce)"两段，是因为上游会把同一个 App 的
// 规则拆成几个插件、都写同一个 #!name。它们该合成一个模块，而不是互相覆盖
// 或各占一个文件，见 groupStaged。

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

// staged 是一个已经定名、还没转换的插件。
type staged struct {
	entry   Entry
	source  string // 插件正文；下载失败时为空
	title   string // 正文里的 #!name，同名插件按它合流
	old     *Entry
	changed bool
	renamed bool
}

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

	items := make([]staged, len(remotes))
	for _, index := range order {
		items[index] = s.stage(remotes[index], downloads[index], old[index], write, used)
	}

	// 同名的插件在这里合流：一个模块只由一次转换产出，组内其余记为已并入。
	//
	// 注意 --only / --limit 只下了一部分时组可能不完整，这会被当成单插件转换，
	// 主源这一轮只带自己那份规则。调试模式本来就不写索引，可以接受。
	groups := groupStaged(items, order)
	primaries := make(map[string]bool, len(groups))
	for _, group := range groups {
		if group[0].source != "" {
			primaries[strings.ToLower(group[0].entry.Stem)] = true
		}
	}
	for _, group := range groups {
		s.produce(group, primaries, write)
	}

	entries := make([]Entry, len(items))
	for index, item := range items {
		entries[index] = item.entry
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

// stage 定名 + 落盘，把转换留给合流后的 produce。
func (s *Syncer) stage(remote Remote, got fetched, old *Entry, write bool, used map[string]bool) staged {
	if !got.usable() {
		return staged{entry: s.keep(old, remote, got.err, used), old: old}
	}

	source := decode(got.data)
	title := lpx.PluginName(source)
	entry := Entry{
		Name: remote.Name, Desc: remote.Desc, Icon: lpx.PluginIcon(source), Tags: remote.Tags,
		Authors: remote.Authors, Date: remote.Date, URL: remote.URL,
		Stem:   UniqueStem(fileStem(title, remote.Stem), used),
		LpxSHA: hashOf(got.data),
	}

	item := staged{
		entry:   entry,
		source:  source,
		title:   title,
		old:     old,
		changed: old == nil || old.LpxSHA != entry.LpxSHA,
		renamed: old != nil && old.Stem != entry.Stem,
	}
	if (item.changed || item.renamed) && write {
		if _, err := fsutil.WriteFile(s.vault.lpxPath(entry.Stem), got.data); err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %s：%v\n", entry.Stem, err)
			return staged{entry: s.keep(old, remote, err, used), old: old}
		}
	}
	return item
}

// groupStaged 把 #!name 相同的插件排到一组。
//
// 遍历顺序用定名时的 order（按地址），所以组内第一条就是拿到原名的那个，
// 也就是最先收录的主源；合并结果不会随下载完成顺序漂移。
func groupStaged(items []staged, order []int) [][]*staged {
	at := make(map[string]int, len(items))
	var groups [][]*staged
	for _, index := range order {
		item := &items[index]
		// 没有 #!name 就无从判断是不是同一个插件，各转各的。
		key := strings.ToLower(item.title)
		if key == "" {
			groups = append(groups, []*staged{item})
			continue
		}
		if slot, ok := at[key]; ok {
			groups[slot] = append(groups[slot], item)
			continue
		}
		at[key] = len(groups)
		groups = append(groups, []*staged{item})
	}
	return groups
}

// produce 把一组同名插件转成一个模块：主源产出文件，其余记为已并入。
func (s *Syncer) produce(group []*staged, primaries map[string]bool, write bool) {
	primary := group[0]

	if len(group) == 1 {
		if primary.source == "" {
			return // 下载失败，沿用上一轮，不重转
		}
		if primary.old != nil && primary.old.Status == StatusMerged &&
			!primaries[strings.ToLower(primary.old.MergedInto)] {
			// 上一轮它并进了别人，这一轮主源没下到。此刻自立门户会多出一个
			// 只含半套规则的模块，不如保持并入，等主源恢复。
			s.adopt(primary, primary.old.MergedInto, write)
			return
		}
	}

	sources := make([]lpx.Source, 0, len(group))
	for _, item := range group {
		sources = append(sources, lpx.Source{Name: item.entry.Stem, Text: item.source})
	}

	entry := primary.entry
	entry.Merged = nil
	if len(group) > 1 {
		// 模块头部的 desc 已经把两份拼在一起；索引里的描述来自上游列表，
		// 不跟着合的话网站会只显示主源那半句，用户以为另一半广告没去。
		if desc := lpx.MergeDesc(sources); desc != "" {
			entry.Desc = desc
		}
	}
	contentChanged := false
	for _, item := range group[1:] {
		entry.Merged = append(entry.Merged, item.entry.Stem)
	}
	for _, item := range group {
		contentChanged = contentChanged || item.changed
	}
	// 融合的组成变了也算变：插件字节没动，但这一轮多了一个同名插件，
	// 模块内容会不一样，提交信息里得露出来。
	grown := primary.old == nil || len(entry.Merged) > len(primary.old.Merged)

	primary.entry = s.rebuild(entry, sources, primary.old, write, contentChanged || grown)
	s.record(primary.entry, primary.old, contentChanged || grown, primary.renamed)

	for _, item := range group[1:] {
		s.adopt(item, primary.entry.Stem, write)
	}
}

// adopt 把一个插件记为"规则已并进目标模块"，并收掉它可能残留的旧模块文件。
func (s *Syncer) adopt(item *staged, target string, write bool) {
	item.entry.Status = StatusMerged
	item.entry.MergedInto = target
	item.entry.ModuleSHA = ""
	item.entry.Notes = []string{"规则已并入「" + target + "」"}
	if write {
		// 上一轮它可能还有自己的模块，现在归主源了。
		_ = os.Remove(s.vault.modulePath(item.entry.Stem))
	}
	s.record(item.entry, item.old, item.changed, item.renamed)
}

// rebuild 把一组源转成模块并落盘。
//
// fresh 为 false 表示插件字节没变、融合组也没变大，上一轮的结论可以照用。
func (s *Syncer) rebuild(entry Entry, sources []lpx.Source, old *Entry, write, fresh bool) Entry {
	entry.Status = StatusOK
	entry.Converter = lpx.Version

	// 复用上一轮结论有两个前提：插件字节没变，参与的源也没变多。后者不能省——
	// 昨天一个源、今天多了一个同名插件，字节不动但模块必须重做。
	// 反过来"源变少了"（某个同名插件这轮没下到）要照旧：此刻重转会把它的规则
	// 从模块里抹掉，而它在 loon/ 里的原文还好好的。
	if !fresh && old != nil && old.Converter == lpx.Version {
		entry.Merged = old.Merged
		switch {
		case old.Status == StatusExcluded:
			entry.Status = StatusExcluded
			entry.Notes = old.Notes
			s.mu.Lock()
			s.excluded = append(s.excluded, entry.Stem)
			s.mu.Unlock()
			return entry
		case old.Status == StatusOK && (!write || fsutil.Exists(s.vault.modulePath(entry.Stem))):
			entry.ModuleSHA = old.ModuleSHA
			entry.Notes = old.Notes
			entry.Adapted = old.Adapted
			entry.Review = old.Review
			return entry
		}
	}

	result := lpx.ConvertSources(sources, s.opts)
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
