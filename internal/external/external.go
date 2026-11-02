// Package external 把第三方仓库里现成的 Surge 模块并进 surge/。
//
// 和 sync 的分工：sync 管"可莉的 Loon 插件 → 转换"，这里管"别人写好的模块 → 直接收"。
// 两件事的产物落在同一个目录，所以去重必须按 #!name（文件名会变，名字才是身份），
// 撞名时保留可莉那份——它的语法经过转换器逐行核对，可信度更高。
//
// 上游模块质量参差，处理原则和转换器一致：能机械修的就修（BOM、section 顺序），
// 修不了的（混着 Loon 语法）就不收，写进清单说明原因，绝不硬塞进 surge/。
package external

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"loon2surge/internal/fsutil"
	"loon2surge/internal/lpx"
	"loon2surge/internal/upstream"
)

const (
	DefaultRepo  = "ios151/Rewrite"
	DefaultRef   = "main"
	DefaultDir   = "external/ios151"
	DefaultSurge = "surge"
	// ManifestName 是放在快照目录里的清单，记录每个上游文件的下场。
	ManifestName = "manifest.json"
)

// DefaultManifestPath 是清单的默认位置，validate 用它读豁免。
var DefaultManifestPath = filepath.Join(DefaultDir, ManifestName)

// Source 是上游的一个模块文件。
type Source struct {
	Path  string // 上游仓库内的相对路径
	Data  []byte // 规范化之后的正文
	Name  string // #!name
	Desc  string
	Icon  string
	Date  string
	Stamp string   // 整理后正文的 SHA-256，作为"这份内容有没有变"的基准
	Tidy  []string // 整理时动了什么，写进清单备查
}

// 清单里的处理结论。
const (
	statusMerged     = "merged"
	statusDuplicate  = "duplicate"
	statusUpstreamDu = "upstream-duplicate"
	statusIncompat   = "incompatible"
	statusNonSurge   = "non-surge"
	statusNoName     = "no-name"
	statusRetired    = "retired"
)

// Entry 是清单里的一条。
type Entry struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Snapshot  string   `json:"snapshot,omitempty"`
	SHA       string   `json:"sha256"`
	Bytes     int      `json:"bytes"`
	Status    string   `json:"status"`
	Stem      string   `json:"stem,omitempty"`
	Duplicate string   `json:"duplicate_of,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Accepted  []string `json:"accepted_problems,omitempty"`
	Tidy      []string `json:"tidy,omitempty"`
}

// Manifest 是快照目录里的 manifest.json。
type Manifest struct {
	Source   string  `json:"source"`
	Ref      string  `json:"ref"`
	SyncedAt string  `json:"synced_at"`
	Count    int     `json:"count"`
	Merged   int     `json:"merged"`
	Entries  []Entry `json:"entries"`
}

// Report 是一次运行的结果，给 CI 拼提交信息和 Job Summary 用。
type Report struct {
	Manifest Manifest
	Added    []Entry
	Updated  []Entry
	Removed  []Entry
	Skipped  []Entry
}

// Print 把结果打到控制台；格式固定，action 的 grep 依赖 `外部同步：` 这一行。
func (r Report) Print(w io.Writer) {
	fmt.Fprintf(w, "外部同步：%d 个模块，新增 %d，更新 %d，下线 %d，跳过 %d\n",
		r.Manifest.Count, len(r.Added), len(r.Updated), len(r.Removed), len(r.Skipped))
	for _, entry := range r.Added {
		fmt.Fprintf(w, "  + %s（%s）\n", entry.Name, entry.Stem)
	}
	for _, entry := range r.Updated {
		fmt.Fprintf(w, "  ~ %s（%s）\n", entry.Name, entry.Stem)
	}
	for _, entry := range r.Removed {
		fmt.Fprintf(w, "  - %s（%s）：%s\n", entry.Name, entry.Stem, entry.Reason)
	}
	for _, entry := range r.Skipped {
		// 带上路径：上游常有同名不同版本（比如 porntube 的小火箭版），
		// 只报名字会让人以为同一个模块既被跳过又被并入了。
		if entry.Path != "" {
			fmt.Fprintf(w, "  · 跳过 %s（%s）：%s\n", entry.Name, entry.Path, entry.Reason)
			continue
		}
		fmt.Fprintf(w, "  · 跳过 %s：%s\n", entry.Name, entry.Reason)
	}
}

// Options 是命令行的等价物。
type Options struct {
	Repo      string
	Ref       string
	Dir       string // 快照目录
	SurgeDir  string
	SourceDir string // 本地已下载的仓库根目录，留空则从 GitHub 拉
	Token     string
	DryRun    bool
	Now       func() time.Time
}

func (o *Options) fill() {
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.Ref == "" {
		o.Ref = DefaultRef
	}
	if o.Dir == "" {
		o.Dir = DefaultDir
	}
	if o.SurgeDir == "" {
		o.SurgeDir = DefaultSurge
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Token == "" {
		o.Token = os.Getenv("GITHUB_TOKEN")
	}
}

// Run 执行一次完整同步。
func Run(opts Options) (Report, error) {
	opts.fill()

	read, skipped, err := load(opts)
	if err != nil {
		return Report{}, err
	}
	sources, upstreamDup := pickUnique(read)

	all, err := existingModules(opts.SurgeDir)
	if err != nil {
		return Report{}, err
	}
	previous := readManifest(filepath.Join(opts.Dir, ManifestName))

	report := Report{}
	entries := make([]Entry, 0, len(sources)+len(skipped)+len(upstreamDup))
	entries = append(entries, skipped...)
	entries = append(entries, upstreamDup...)
	report.Skipped = append(report.Skipped, skipped...)
	report.Skipped = append(report.Skipped, upstreamDup...)

	// 上一轮并进来的模块要和我们自己"分家"：它们在 surge/ 里，但那是我们写的，
	// 不摘出来的话每轮都会撞上自己，先收进来再全部下线——这个坑踩过一次。
	oursByStem, oursByName := ownModules(previous, opts.SurgeDir)
	others := make([]existingModule, 0, len(all))
	for _, module := range all {
		if _, mine := oursByStem[module.Stem]; !mine {
			others = append(others, module)
		}
	}

	used := make(map[string]bool, len(others))
	for _, module := range others {
		used[strings.ToLower(module.Stem)] = true
	}

	mergedByStem := make(map[string]Entry, len(previous.Entries))
	currentByName := make(map[string]Entry, len(sources))

	for _, source := range sources {
		entry := Entry{
			Name:  source.Name,
			Path:  source.Path,
			SHA:   source.Stamp,
			Bytes: len(source.Data),
			Tidy:  source.Tidy,
		}
		// 快照一律保留，方便事后核对"上游到底给了什么"，不只是留并进去的那些。
		if !opts.DryRun {
			if _, err := fsutil.WriteFile(filepath.Join(opts.Dir, filepath.FromSlash(source.Path)), source.Data); err != nil {
				return Report{}, err
			}
		}
		entry.Snapshot = source.Path
		key := normName(source.Name)

		problems := lpx.Validate(string(source.Data))
		accepted, reject := judge(problems)
		if reject != "" {
			entry.Status = statusIncompat
			entry.Reason = reject
			entries = append(entries, entry)
			report.Skipped = append(report.Skipped, entry)
			currentByName[key] = entry
			continue
		}
		entry.Accepted = accepted

		if stem, mine := oursByName[key]; mine {
			// 上一轮已经并过同一个模块：沿用原文件名，只看内容变没变。
			entry.Status = statusMerged
			entry.Stem = stem
			used[strings.ToLower(stem)] = true
			changed, err := syncTo(filepath.Join(opts.SurgeDir, stem+".sgmodule"), source.Data, opts.DryRun)
			if err != nil {
				return Report{}, err
			}
			if changed {
				report.Updated = append(report.Updated, entry)
			}
			mergedByStem[stem] = entry
			currentByName[key] = entry
			entries = append(entries, entry)
			continue
		}

		if stem, reason, ok := matchExisting(source.Name, others); ok {
			entry.Status = statusDuplicate
			entry.Duplicate = stem
			entry.Reason = reason
			entries = append(entries, entry)
			report.Skipped = append(report.Skipped, entry)
			currentByName[key] = entry
			continue
		}

		fallback := strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))
		stem := upstream.UniqueStem(upstream.FileStem(source.Name, fallback), used)
		entry.Status = statusMerged
		entry.Stem = stem
		changed, err := syncTo(filepath.Join(opts.SurgeDir, stem+".sgmodule"), source.Data, opts.DryRun)
		if err != nil {
			return Report{}, err
		}
		if changed {
			report.Added = append(report.Added, entry)
		}
		mergedByStem[stem] = entry
		currentByName[key] = entry
		entries = append(entries, entry)
	}

	// 上一轮并进来、这一轮不该再存在的：上游删了、或者可莉自己也有了同名模块。
	for _, old := range previous.Entries {
		if old.Status != statusMerged || old.Stem == "" {
			continue
		}
		if _, alive := mergedByStem[old.Stem]; alive {
			continue
		}
		reason := retireReason(old, currentByName)
		if reason == "" {
			continue
		}
		old.Status = statusRetired
		old.Reason = reason
		entries = append(entries, old)
		report.Removed = append(report.Removed, old)
		if !opts.DryRun {
			removeIfUnchanged(filepath.Join(opts.SurgeDir, old.Stem+".sgmodule"), old.SHA)
		}
	}

	slices.SortFunc(entries, func(a, b Entry) int {
		if diff := strings.Compare(a.Name, b.Name); diff != 0 {
			return diff
		}
		return strings.Compare(a.Path, b.Path)
	})
	manifest := Manifest{
		Source:   "https://github.com/" + opts.Repo,
		Ref:      opts.Ref,
		SyncedAt: opts.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		Count:    len(entries),
		Merged:   len(mergedByStem),
		Entries:  entries,
	}
	report.Manifest = manifest
	if !opts.DryRun {
		if err := writeManifest(filepath.Join(opts.Dir, ManifestName), manifest); err != nil {
			return report, err
		}
		pruneSnapshot(opts.Dir, manifest, previous)
	}
	return report, nil
}

// load 拿到上游的全部模块：本地目录优先（离线调试、测试用），否则从 GitHub 拉。
func load(opts Options) (sources []Source, skipped []Entry, err error) {
	if opts.SourceDir != "" {
		return readLocal(opts.SourceDir, opts.Repo)
	}
	blob, err := fetchTarball(opts.Repo, opts.Ref, opts.Token)
	if err != nil {
		return nil, nil, err
	}
	return readTarball(blob, opts.Repo)
}

// judge 判断一份上游模块能不能收。
//
// 只放过一类问题：中文参数名。上游模块习惯写 `日志级别:info`，Surge 认，但转换器的
// 校验规则只认 ASCII（它要保证 Loon 的 `%Arg%` 能一一对应）。除此之外任何问题都不收，
// 尤其是 `script-type`——那说明这个文件其实还是 Loon 语法，导进 Surge 是空壳。
func judge(problems []lpx.Problem) (accepted []string, reject string) {
	if len(problems) == 0 {
		return nil, ""
	}
	for _, problem := range problems {
		if problem.Code != "argument-name" {
			return nil, fmt.Sprintf("L%d [%s] %s", problem.Line, problem.Code, problem.Message)
		}
	}
	return []string{"argument-name"}, ""
}

// syncTo 只在内容变化时写文件，返回是否真的变了。
func syncTo(path string, data []byte, dryRun bool) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == string(data) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	_, err = fsutil.WriteFile(path, data)
	return true, err
}
