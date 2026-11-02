package external

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"loon2surge/internal/kelee"
	"loon2surge/internal/lpx"
)

// 把第三方仓库里现成的 Surge 模块并进 surge/。
//
// 和 sync 的关系：sync 管的是"可莉的 Loon 插件 → 转换"，这里是"别人写好的模块 → 直接收"。
// 两件事的产物落在同一个目录，所以去重必须按 #!name（文件名会变，名字才是身份），
// 撞名时保留可莉那份——它的语法经过转换器逐行核对，可信度更高。
//
// 上游模块质量参差，这里的处理原则和转换器一致：能机械修的就修（BOM、section 顺序），
// 修不了的（混着 Loon 语法）就不收，写进清单说明原因，绝不硬塞进 surge/。

const (
	DefaultRepo  = "ios151/Rewrite"
	DefaultRef   = "main"
	DefaultDir   = "ios151 Rewrite"
	DefaultSurge = "surge"
	// ManifestName 是放在快照目录里的清单，记录每个上游文件的下场。
	ManifestName = "manifest.json"
)

// 这些词只表示"怎么处理"而不是"对谁处理"，比对名字时先摘掉。
// 摘掉之后同一个 App 的「去广告」和「会员解锁」才能认出是同一件事。
var noiseWords = []string{
	"解锁", "去广告", "广告", "会员", "vip", "净化", "增强", "破解", "跳过", "屏蔽",
	"去开屏", "签到", "助手", "面板", "panel", "专业版", "pro", "跳转", "重定向",
	"链接", "视频", "短剧", "社区", "最新版", "新版",
}

var stemPunct = strings.NewReplacer(
	" ", "", "\t", "", "-", "", "_", "", "·", "", "・", "", ".", "", ",", "", "，", "",
	"。", "", "!", "", "！", "", "?", "", "？", "", ":", "", "：", "", ";", "", "；", "",
	"\"", "", "'", "", "“", "", "”", "", "‘", "", "’", "", "|", "", "/", "", "\\", "",
	"+", "", "~", "", "～", "", "[", "", "]", "", "【", "", "】", "", "(", "", ")", "",
	"（", "", "）", "", "&", "", "＆", "", "→", "", "·", "",
)

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

// Entry 是清单里的一条。
type Entry struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Snapshot  string   `json:"snapshot,omitempty"`
	SHA       string   `json:"sha256"`
	Bytes     int      `json:"bytes"`
	Status    string   `json:"status"` // merged / duplicate / upstream-duplicate / incompatible / non-surge / no-name
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

// Main 是 `loon2surge external` 的入口。
func Main(argv []string) int {
	opts := Options{Token: os.Getenv("GITHUB_TOKEN")}
	args := argv
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case arg == "--dir":
			opts.Dir = value()
		case arg == "--surge":
			opts.SurgeDir = value()
		case arg == "--repo":
			opts.Repo = value()
		case arg == "--ref":
			opts.Ref = value()
		case arg == "--source-dir":
			opts.SourceDir = value()
		case arg == "--dry-run":
			opts.DryRun = true
		case arg == "-h" || arg == "--help":
			fmt.Print(externalUsage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "不认识的参数：%s\n", arg)
			return 2
		}
	}
	report, err := Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "外部模块同步失败：%v\n", err)
		return 1
	}
	printReport(report)
	return 0
}

const externalUsage = `用法：loon2surge external [选项]

把第三方仓库里现成的 Surge 模块同步到快照目录，并把 surge/ 里还没有的并进去。

选项：
  --dir          快照目录，默认 "ios151 Rewrite"
  --surge        模块目录，默认 surge
  --repo         上游仓库，默认 ios151/Rewrite
  --ref          分支或标签，默认 main
  --source-dir   用本地已解压的仓库代替下载（离线、测试用）
  --dry-run      只报告，不写文件
`

// Run 执行一次完整同步。
func Run(opts Options) (Report, error) {
	opts.fill()

	var sources []Source
	var skipped []Entry // 连快照都不值得留的（没名字、明显不是 Surge）

	if opts.SourceDir != "" {
		read, drop, err := readLocal(opts.SourceDir, opts.Repo)
		if err != nil {
			return Report{}, err
		}
		sources, skipped = read, drop
	} else {
		blob, err := fetchTarball(opts.Repo, opts.Ref, opts.Token)
		if err != nil {
			return Report{}, err
		}
		read, drop, err := readTarball(blob, opts.Repo)
		if err != nil {
			return Report{}, err
		}
		sources, skipped = read, drop
	}

	sources, upstreamDup := pickUnique(sources)

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
		if _, mine := oursByStem[module.Stem]; mine {
			continue
		}
		others = append(others, module)
	}

	used := make(map[string]bool, len(others))
	for _, module := range others {
		used[module.Stem] = true
	}

	snapshotDir := opts.Dir
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
			if err := writeFile(filepath.Join(snapshotDir, filepath.FromSlash(source.Path)), source.Data); err != nil {
				return Report{}, err
			}
		}
		entry.Snapshot = source.Path
		key := normName(source.Name)

		problems := lpx.Validate(string(source.Data))
		accepted, reject := judge(problems)
		if reject != "" {
			entry.Status = "incompatible"
			entry.Reason = reject
			entries = append(entries, entry)
			report.Skipped = append(report.Skipped, entry)
			currentByName[key] = entry
			continue
		}
		entry.Accepted = accepted

		if stem, mine := oursByName[key]; mine {
			// 上一轮已经并过同一个模块：沿用原文件名，只看内容变没变。
			entry.Status = "merged"
			entry.Stem = stem
			used[stem] = true
			if changed, err := syncTo(filepath.Join(opts.SurgeDir, stem+".sgmodule"), source.Data, opts.DryRun); err != nil {
				return Report{}, err
			} else if changed {
				report.Updated = append(report.Updated, entry)
			}
			mergedByStem[stem] = entry
			currentByName[key] = entry
			entries = append(entries, entry)
			continue
		}

		if stem, reason, ok := matchExisting(source.Name, others); ok {
			entry.Status = "duplicate"
			entry.Duplicate = stem
			entry.Reason = reason
			entries = append(entries, entry)
			report.Skipped = append(report.Skipped, entry)
			currentByName[key] = entry
			continue
		}

		stem := unique(kelee.FileStem(source.Name, strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))), used)
		entry.Status = "merged"
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
		if old.Status != "merged" || old.Stem == "" {
			continue
		}
		if _, alive := mergedByStem[old.Stem]; alive {
			continue
		}
		reason := retireReason(old, currentByName)
		if reason == "" {
			continue
		}
		old.Status = "retired"
		old.Reason = reason
		entries = append(entries, old)
		report.Removed = append(report.Removed, old)
		if !opts.DryRun {
			removeIfUnchanged(filepath.Join(opts.SurgeDir, old.Stem+".sgmodule"), old.SHA)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Path < entries[j].Path
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
		pruneSnapshot(snapshotDir, manifest, previous)
	}
	return report, nil
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

// pickUnique 处理上游自己内部的重复：同样的内容存了两份、同一个 #!name 有两个版本。
func pickUnique(sources []Source) ([]Source, []Entry) {
	byHash := map[string]string{}
	byName := map[string]int{}
	kept := make([]Source, 0, len(sources))
	var dropped []Entry
	for _, source := range sources {
		if first, ok := byHash[source.Stamp]; ok {
			dropped = append(dropped, Entry{
				Name: source.Name, Path: source.Path, SHA: source.Stamp, Bytes: len(source.Data),
				Status: "upstream-duplicate", Reason: "内容与 " + first + " 完全相同",
			})
			continue
		}
		key := coreName(source.Name)
		if key == "" {
			key = normName(source.Name)
		}
		if index, ok := byName[key]; ok {
			// 同名不同内容：留更大的那份（通常规则更全），丢小的但记下来。
			if len(source.Data) > len(kept[index].Data) {
				dropped = append(dropped, Entry{
					Name: kept[index].Name, Path: kept[index].Path, SHA: kept[index].Stamp, Bytes: len(kept[index].Data),
					Status: "upstream-duplicate", Reason: "同名保留内容更全的 " + source.Path,
				})
				kept[index] = source
				byHash[source.Stamp] = source.Path
			} else {
				dropped = append(dropped, Entry{
					Name: source.Name, Path: source.Path, SHA: source.Stamp, Bytes: len(source.Data),
					Status: "upstream-duplicate", Reason: "同名保留内容更全的 " + kept[index].Path,
				})
			}
			continue
		}
		byHash[source.Stamp] = source.Path
		byName[key] = len(kept)
		kept = append(kept, source)
	}
	return kept, dropped
}

// ---------------------------------------------------------------------------
// 抓取与拆包
// ---------------------------------------------------------------------------

func fetchTarball(repo, ref, token string) ([]byte, error) {
	urls := []string{
		fmt.Sprintf("https://codeload.github.com/%s/tar.gz/refs/heads/%s", repo, ref),
		fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", repo, ref),
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	var lastErr error
	for _, url := range urls {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "loon2surge")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s 返回 %d", url, response.StatusCode)
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("下载 %s 失败：%w", repo, lastErr)
}

// readTarball 从 GitHub 的 tar.gz 里挑出模块文件，跳过脚本和图片。
func readTarball(blob []byte, repo string) ([]Source, []Entry, error) {
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, nil, fmt.Errorf("压缩包读不了：%w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	var sources []Source
	var dropped []Entry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("压缩包解不开：%w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		// 去掉压缩包自带的那层 Rewrite-main/ 前缀
		path := header.Name
		if index := strings.IndexByte(path, '/'); index >= 0 {
			path = path[index+1:]
		}
		if path == "" {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, nil, err
		}
		source, drop := classify(path, data, repo)
		switch {
		case drop != nil:
			dropped = append(dropped, *drop)
			continue
		case source == nil:
			continue // 脚本、图标这类附件，不是模块
		}
		sources = append(sources, *source)
	}
	return sources, dropped, nil
}

func readLocal(root, repo string) ([]Source, []Entry, error) {
	var sources []Source
	var dropped []Entry
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			if item.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source, drop := classify(filepath.ToSlash(relative), data, repo)
		switch {
		case drop != nil:
			dropped = append(dropped, *drop)
			return nil
		case source == nil:
			return nil // 脚本、图标这类附件，不是模块
		}
		sources = append(sources, *source)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sources, dropped, nil
}

// classify 判断一个文件是不是能用的 Surge 模块，顺手做规范化。
func classify(path string, data []byte, repo string) (*Source, *Entry) {
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".sgmodule") && !strings.HasSuffix(lower, ".sgmodulle") {
		return nil, nil
	}
	text, notes := tidy(string(data), repo)
	name := lpx.PluginName(text)
	entry := &Entry{Name: name, Path: path, SHA: stamp(text), Bytes: len(text)}

	if name == "" {
		// 没有 #!name 的模块 Surge 直接拒收；留在快照里没有意义，只记一笔。
		entry.Status = "no-name"
		entry.Reason = "缺少 #!name，Surge 不会认这个模块"
		return nil, entry
	}
	if strings.Contains(lower, "shadowrocket") || isLoonOnly(text) {
		entry.Status = "non-surge"
		entry.Reason = "小火箭专用模块，Surge 里没有对应写法"
		return nil, entry
	}
	return &Source{
		Path:  path,
		Data:  []byte(text),
		Name:  name,
		Desc:  lpx.PluginMeta(text, "desc"),
		Icon:  lpx.PluginMeta(text, "icon"),
		Date:  lpx.PluginMeta(text, "date"),
		Stamp: entry.SHA,
		Tidy:  notes,
	}, nil
}

// isLoonOnly 看头部有没有写"小火箭专用"之类的自述。
func isLoonOnly(text string) bool {
	head := text
	if len(head) > 600 {
		head = head[:600]
	}
	for _, marker := range []string{"小火箭专用", "仅支持小火箭", "只支持小火箭"} {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 文本规范化
// ---------------------------------------------------------------------------

// sectionRank 给出 section 在 Surge 里的习惯位置；不认识的排最后。
func sectionRank(name string) int {
	for index, known := range lpx.SectionOrder {
		if strings.EqualFold(known, name) {
			return index
		}
	}
	// [Panel] 不在 SectionOrder 里（转换器不产出），Surge 的顺序放在最后。
	return len(lpx.SectionOrder) + 1
}

// scriptParams 是 [Script] 行里会出现的参数名。判断 pattern 的值到哪儿结束要靠它：
// 正则本身可能带逗号（比如 {2,3}），不能见到逗号就切。
var scriptParams = []string{
	"type", "pattern", "requires-body", "max-size", "script-path",
	"script-update-interval", "timeout", "argument", "binary-body-mode",
	"debug", "engine", "interval", "cronexp",
}

// tidy 把上游模块整理成这个仓库的样子，返回整理后的正文和"动了什么"。
//
// 边界是 Surge 语法层面的机械修正：头部写法、段与段的空行、section 顺序、
// 完全重复的行、[Script] 行尾多出来的逗号、pattern 里没转义的主机名点、
// MITM 里写了端口。规则内容、正则分支、脚本参数的值一律不碰——拿不准的
// 宁可留原样，也不猜上游想干什么。
func tidy(text, repo string) (string, []string) {
	var notes []string
	add := func(note string) {
		for _, old := range notes {
			if old == note {
				return
			}
		}
		notes = append(notes, note)
	}

	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	raw := strings.Split(text, "\n")
	lines := make([]string, len(raw))
	trimmedLines := 0
	for index, line := range raw {
		lines[index] = strings.TrimRight(line, " \t\u00a0")
		if lines[index] != line && strings.TrimSpace(line) != "" {
			trimmedLines++
		}
	}
	if trimmedLines > 0 {
		add(fmt.Sprintf("去掉 %d 处行尾空白", trimmedLines))
	}

	type section struct {
		name string
		body []string
	}
	var head []string
	var sections []section
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			canonical := canonicalSection(name)
			if canonical != name {
				add("section 名统一成 [" + canonical + "]")
			}
			sections = append(sections, section{name: canonical})
			continue
		}
		if len(sections) == 0 {
			head = append(head, line)
			continue
		}
		sections[len(sections)-1].body = append(sections[len(sections)-1].body, line)
	}

	sorted := true
	for index := 1; index < len(sections); index++ {
		if sectionRank(sections[index-1].name) > sectionRank(sections[index].name) {
			sorted = false
			break
		}
	}
	if !sorted {
		sort.SliceStable(sections, func(i, j int) bool {
			return sectionRank(sections[i].name) < sectionRank(sections[j].name)
		})
		add("section 按 Surge 惯例重排")
	}

	parts := make([]string, 0, len(sections))
	for _, item := range sections {
		body := trimBlank(item.body)
		body = dedupeLines(body, add)
		switch item.name {
		case "Script":
			body = tidyScript(body, add)
		case "MITM":
			body = tidyMITM(body, add)
		}
		body = trimBlank(body)
		if len(body) == 0 {
			add("删掉空的 [" + item.name + "] 段")
			continue
		}
		parts = append(parts, "["+item.name+"]\n"+strings.Join(body, "\n"))
	}

	var out strings.Builder
	if header := tidyHeader(head, repo, add); len(header) > 0 {
		out.WriteString(strings.Join(header, "\n"))
	}
	if len(parts) > 0 {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(strings.Join(parts, "\n\n"))
	}
	return strings.TrimRight(out.String(), "\n") + "\n", notes
}

// tidyHeader 规范头部：`#!key = value` 收成 `#!key=value`、按 MetaOrder 排序、
// 去掉重复与空值；缺 author / category 的补上来源，免得不认识这个模块的人
// 找不到出处。
func tidyHeader(lines []string, repo string, add func(string)) []string {
	type meta struct {
		key   string
		value string
	}
	var metas []meta
	var comments []string
	index := make(map[string]int, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "#!"):
			body := trimmed[2:]
			equal := strings.IndexByte(body, '=')
			if equal < 0 {
				add("头部有写坏的 #! 行")
				comments = append(comments, trimmed)
				continue
			}
			rawKey, rawValue := body[:equal], body[equal+1:]
			key, value := strings.TrimSpace(rawKey), strings.TrimSpace(rawValue)
			if key != rawKey || value != rawValue {
				add("头部 #!key = value 统一成 #!key=value")
			}
			if canonical := canonicalMetaKey(key); canonical != key {
				add("头部字段名统一成 #!" + canonical)
				key = canonical
			}
			lower := strings.ToLower(key)
			if at, ok := index[lower]; ok {
				if metas[at].value == "" && value != "" {
					metas[at].value = value
				}
				add("去掉重复的 #!" + key)
				continue
			}
			index[lower] = len(metas)
			metas = append(metas, meta{key: key, value: value})
		default:
			// 上游在头部写的说明性注释，位置保留在元数据之后。
			comments = append(comments, trimmed)
		}
	}

	fill := func(key, value, why string) {
		lower := strings.ToLower(key)
		if at, ok := index[lower]; ok {
			if metas[at].value != "" {
				return
			}
			metas[at].value = value
		} else {
			index[lower] = len(metas)
			metas = append(metas, meta{key: key, value: value})
		}
		add(why)
	}
	if repo != "" {
		owner := repo
		if slash := strings.IndexByte(owner, '/'); slash > 0 {
			owner = owner[:slash]
		}
		fill("author", owner+"[https://github.com/"+repo+"]", "补齐 #!author（标明来源）")
		fill("category", owner, "补齐 #!category（Surge 里好归类）")
		if note := provenance(repo); !contains(comments, note) {
			comments = append(comments, note)
		}
	}

	sort.SliceStable(metas, func(i, j int) bool {
		return metaRank(metas[i].key) < metaRank(metas[j].key)
	})

	out := make([]string, 0, len(metas)+len(comments))
	for _, item := range metas {
		if item.value == "" {
			add("删掉没有值的 #!" + item.key)
			continue
		}
		out = append(out, "#!"+item.key+"="+item.value)
	}
	return append(out, comments...)
}

func provenance(repo string) string {
	return "# 由 loon2surge 从 " + repo + " 同步整理"
}

func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// canonicalSection 把 [script] 这种写法收回标准大小写。
func canonicalSection(name string) string {
	for _, known := range lpx.SectionOrder {
		if strings.EqualFold(known, name) {
			return known
		}
	}
	if strings.EqualFold(name, "Panel") {
		return "Panel"
	}
	return name
}

func canonicalMetaKey(key string) string {
	for _, known := range lpx.MetaOrder {
		if strings.EqualFold(known, key) {
			return known
		}
	}
	return key
}

func metaRank(key string) int {
	for index, known := range lpx.MetaOrder {
		if strings.EqualFold(known, key) {
			return index
		}
	}
	return len(lpx.MetaOrder) + 1
}

// tidyScript 只动"写错了但一眼能看出本意"的地方。
func tidyScript(body []string, add func(string)) []string {
	out := make([]string, 0, len(body))
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		if strings.HasSuffix(line, ",") {
			line = strings.TrimSuffix(line, ",")
			add("[Script] 去掉行尾多余的逗号")
		}
		if strings.Contains(line, "requires-body=true") || strings.Contains(line, "requires-body=false") {
			line = strings.ReplaceAll(line, "requires-body=true", "requires-body=1")
			line = strings.ReplaceAll(line, "requires-body=false", "requires-body=0")
			add("requires-body 的 true/false 统一写成 1/0")
		}
		if fixed, ok := escapePatternHost(line); ok {
			line = fixed
			add("[Script] 转义 pattern 主机名里的点")
		}
		out = append(out, line)
	}
	return out
}

// escapePatternHost 把 pattern 里 scheme 与主机名之间那些没转义的点补上反斜杠。
//
// `pattern=^https://api.agcplayer.com/` 里的点是正则通配，能匹配
// `apiXagcplayerYcom`；补成 `\.` 才是"这个域名"。只处理主机名部分且只处理
// 形态干净的主机名，路径和带分组的正则一概不碰。
func escapePatternHost(line string) (string, bool) {
	const marker = "pattern="
	at := strings.Index(line, marker)
	if at < 0 {
		return line, false
	}
	start := at + len(marker)
	rest := line[start:]
	end := scriptValueEnd(rest)
	fixed, changed := escapeHostDots(rest[:end])
	if !changed {
		return line, false
	}
	return line[:start] + fixed + rest[end:], true
}

func scriptValueEnd(rest string) int {
	for index := 0; index < len(rest); index++ {
		if rest[index] != ',' {
			continue
		}
		tail := strings.TrimLeft(rest[index+1:], " \t")
		for _, name := range scriptParams {
			if strings.HasPrefix(tail, name+"=") {
				return index
			}
		}
	}
	return len(rest)
}

func escapeHostDots(pattern string) (string, bool) {
	prefix, host, rest, ok := splitScheme(pattern)
	if !ok || host == "" {
		return pattern, false
	}
	for _, char := range host {
		switch {
		case char == '.' || char == '-' || char == '_' || char == '*':
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		default:
			return pattern, false // 带分组、字符类的正则，改了会变味
		}
	}
	var out strings.Builder
	changed := false
	for index := 0; index < len(host); index++ {
		if host[index] == '.' && (index == 0 || host[index-1] != '\\') {
			out.WriteString(`\.`)
			changed = true
			continue
		}
		out.WriteByte(host[index])
	}
	if !changed {
		return pattern, false
	}
	return prefix + out.String() + rest, true
}

func splitScheme(pattern string) (prefix, host, rest string, ok bool) {
	for _, candidate := range []string{
		"^https?://", "https?://", "^https://", "^http://", "https://", "http://",
	} {
		if !strings.HasPrefix(pattern, candidate) {
			continue
		}
		body := pattern[len(candidate):]
		if slash := strings.IndexByte(body, '/'); slash >= 0 {
			return candidate, body[:slash], body[slash:], true
		}
		return candidate, body, "", true
	}
	return "", "", "", false
}

// tidyMITM 合并多行 hostname、去掉重复项、去掉端口——Surge 的 hostname 只认
// 主机名，写成 `host:port` 整个条目会被忽略，MITM 就静默失效了。
func tidyMITM(body []string, add func(string)) []string {
	var hosts []string
	appendMode := false
	seen := make(map[string]bool, len(body))
	out := make([]string, 0, len(body))
	hostAt := -1

	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "hostname") {
			out = append(out, line)
			continue
		}
		if hostAt < 0 {
			hostAt = len(out)
			out = append(out, "") // 占位，等所有 hostname 收齐再写
		} else {
			add("[MITM] 合并多行 hostname")
		}
		rest := strings.TrimSpace(value)
		if strings.HasPrefix(rest, "%APPEND%") {
			appendMode = true
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "%APPEND%"))
		}
		for _, host := range strings.Split(rest, ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			if bare, changed := stripPort(host); changed {
				add("[MITM] 去掉 hostname 里的端口")
				host = bare
			}
			if seen[host] {
				add("[MITM] 去掉重复的 hostname")
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	if hostAt < 0 {
		return out
	}
	if len(hosts) == 0 {
		add("[MITM] 删掉空的 hostname")
		return append(out[:hostAt], out[hostAt+1:]...)
	}
	line := "hostname = "
	if appendMode {
		line += "%APPEND% "
	}
	out[hostAt] = line + strings.Join(hosts, ", ")
	return out
}

func stripPort(host string) (string, bool) {
	colon := strings.LastIndexByte(host, ':')
	if colon <= 0 || colon == len(host)-1 {
		return host, false
	}
	for _, char := range host[colon+1:] {
		if char < '0' || char > '9' {
			return host, false
		}
	}
	if strings.ContainsRune(host[:colon], ':') {
		return host, false // IPv6 原样留着
	}
	return host[:colon], true
}

func trimBlank(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

func dedupeLines(lines []string, add func(string)) []string {
	seen := make(map[string]bool, len(lines))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, ";") {
			out = append(out, line)
			continue
		}
		if seen[trimmed] {
			add("去掉段内完全重复的行")
			continue
		}
		seen[trimmed] = true
		out = append(out, line)
	}
	return out
}

func stamp(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 名字比对
// ---------------------------------------------------------------------------

// 已存在的模块（surge/ 里的一份），按 #!name 而不是文件名做身份。
type existingModule struct {
	Stem string
	Name string
}

func existingModules(dir string) ([]existingModule, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	modules := make([]existingModule, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".sgmodule") {
			continue
		}
		stem := strings.TrimSuffix(item.Name(), ".sgmodule")
		data, err := os.ReadFile(filepath.Join(dir, item.Name()))
		if err != nil {
			return nil, err
		}
		name := lpx.PluginName(strings.TrimPrefix(string(data), "\ufeff"))
		if name == "" {
			name = stem
		}
		modules = append(modules, existingModule{Stem: stem, Name: name})
	}
	return modules, nil
}

// ownModules 找出上一轮由我们写进 surge/ 的那批文件。
//
// 判据是"清单里记为 merged，且文件内容还等于当初写进去的 SHA"：内容被改过就说明
// 有人接管了这个文件（例如可莉的转换器后来也产出了同名模块），那就不再算我们的。
func ownModules(previous Manifest, surgeDir string) (byStem map[string]Entry, byName map[string]string) {
	byStem = map[string]Entry{}
	byName = map[string]string{}
	for _, entry := range previous.Entries {
		if entry.Status != "merged" || entry.Stem == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(surgeDir, entry.Stem+".sgmodule"))
		if err != nil || stamp(string(data)) != entry.SHA {
			continue
		}
		byStem[entry.Stem] = entry
		byName[normName(entry.Name)] = entry.Stem
	}
	return byStem, byName
}

// syncTo 只在内容变化时写文件，返回是否真的变了。
func syncTo(path string, data []byte, dryRun bool) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	return true, writeFile(path, data)
}

// matchExisting 判断上游模块是不是已经有人做了同一件事。
//
// 三层判断，从严到松：
//  1. 规范化后完全一样（`酷狗音乐会员解锁` vs `酷狗音乐去广告` 摘掉处理后也只剩品牌）；
//  2. 摘掉"解锁/去广告"这类词之后一样，或者一方是另一方的前缀（`快对答案` vs `快对`）；
//  3. 都含中文且整体相似度够高。
//
// 纯英文名只认前两层里的完全相等：`linearitcurve` 和 `Line去广告` 前缀一样，但显然是两个东西。
func matchExisting(name string, modules []existingModule) (string, string, bool) {
	key := normName(name)
	core := coreName(name)
	cjk := hasCJK(name)
	for _, module := range modules {
		otherKey := normName(module.Name)
		otherCore := coreName(module.Name)
		switch {
		case key != "" && key == otherKey:
			return module.Stem, "与 " + module.Name + " 同名", true
		case core != "" && core == otherCore:
			return module.Stem, "与 " + module.Name + " 是同一个功能", true
		case cjk && utf8.RuneCountInString(core) >= 2 && strings.HasPrefix(otherCore, core):
			return module.Stem, "与 " + module.Name + " 名字高度重合", true
		case cjk && utf8.RuneCountInString(otherCore) >= 2 && strings.HasPrefix(core, otherCore):
			return module.Stem, "与 " + module.Name + " 名字高度重合", true
		case cjk && similarity(core, otherCore) >= 0.86:
			return module.Stem, "与 " + module.Name + " 高度相似", true
		}
	}
	return "", "", false
}

func normName(name string) string {
	return stemPunct.Replace(strings.ToLower(strings.TrimSpace(name)))
}

func coreName(name string) string {
	core := normName(name)
	for _, word := range noiseWords {
		core = strings.ReplaceAll(core, word, "")
	}
	return core
}

func hasCJK(text string) bool {
	for _, char := range text {
		if unicode.Is(unicode.Han, char) {
			return true
		}
	}
	return false
}

// similarity 是字符级的相似度，够用就行，不引第三方库。
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	left, right := []rune(a), []rune(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	common := 0
	count := map[rune]int{}
	for _, char := range left {
		count[char]++
	}
	for _, char := range right {
		if count[char] > 0 {
			count[char]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(left)+len(right))
}

// unique 给文件名主干加后缀，避免两个模块写进同一个文件。
func unique(stem string, used map[string]bool) string {
	candidate := stem
	for index := 2; used[candidate]; index++ {
		candidate = fmt.Sprintf("%s-%d", stem, index)
	}
	used[candidate] = true
	return candidate
}

// ---------------------------------------------------------------------------
// 落盘
// ---------------------------------------------------------------------------

func writeFile(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeManifest(path string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func readManifest(path string) Manifest {
	var manifest Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "! %s 读不了，按首次同步处理：%v\n", path, err)
		return Manifest{}
	}
	return manifest
}

// retireReason 判断上一轮并进来的模块为什么不该留了。
func retireReason(old Entry, current map[string]Entry) string {
	entry, ok := current[normName(old.Name)]
	if !ok {
		return "上游已不再提供这个模块"
	}
	switch entry.Status {
	case "duplicate":
		return "可莉已有同名模块（" + entry.Reason + "）"
	case "incompatible":
		return "上游改动后不再兼容：" + entry.Reason
	default:
		return ""
	}
}

// removeIfUnchanged 只在文件还是我们当初写进去的那份时才删，人工改过的一律不动。
func removeIfUnchanged(path, expected string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if stamp(string(data)) != expected {
		fmt.Fprintf(os.Stderr, "! %s 被人工改过，不删除\n", path)
		return
	}
	_ = os.Remove(path)
}

// pruneSnapshot 删掉上游已经下线的快照文件，目录跟着上游走。
func pruneSnapshot(dir string, current, previous Manifest) {
	keep := map[string]bool{}
	for _, entry := range current.Entries {
		// 标记为 retired 的条目留着是为了说明"这个模块怎么没的"，文件本身已经删了。
		if entry.Snapshot == "" || entry.Status == "retired" {
			continue
		}
		keep[entry.Snapshot] = true
	}
	for _, entry := range previous.Entries {
		if entry.Snapshot == "" || keep[entry.Snapshot] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(entry.Snapshot)))
	}
}

func printReport(report Report) {
	manifest := report.Manifest
	fmt.Printf("外部同步：%d 个模块，新增 %d，更新 %d，下线 %d，跳过 %d\n",
		manifest.Count, len(report.Added), len(report.Updated), len(report.Removed), len(report.Skipped))
	for _, entry := range report.Added {
		fmt.Printf("  + %s（%s）\n", entry.Name, entry.Stem)
	}
	for _, entry := range report.Updated {
		fmt.Printf("  ~ %s（%s）\n", entry.Name, entry.Stem)
	}
	for _, entry := range report.Removed {
		fmt.Printf("  - %s（%s）：%s\n", entry.Name, entry.Stem, entry.Reason)
	}
	for _, entry := range report.Skipped {
		// 带上路径：上游常有同名不同版本（比如 porntube 的小火箭版），
		// 只报名字会让人以为同一个模块既被跳过又被并入了。
		if entry.Path != "" {
			fmt.Printf("  · 跳过 %s（%s）：%s\n", entry.Name, entry.Path, entry.Reason)
			continue
		}
		fmt.Printf("  · 跳过 %s：%s\n", entry.Name, entry.Reason)
	}
}
