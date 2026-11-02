package upstream

// index.json 与 INDEX.md 的读写：同步的"上一轮基线"和"对外索引"都落在这里。

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"loon2surge/internal/fsutil"
)

// Entry 是 index.json 里的一条，同时也是下一轮比对的基线。
type Entry struct {
	Stem      string   `json:"stem"`
	Name      string   `json:"name"`
	Desc      string   `json:"desc"`
	Icon      string   `json:"icon,omitempty"`
	Tags      []string `json:"tags"`
	Authors   []string `json:"authors"`
	Date      string   `json:"date"`
	URL       string   `json:"url"`
	LpxSHA    string   `json:"lpx_sha"`
	ModuleSHA string   `json:"module_sha"`
	Status    string   `json:"status"`
	Notes     []string `json:"notes"`
	// MergedInto 指向并进的那个模块，status 为 merged 时才有值。
	MergedInto string `json:"merged_into,omitempty"`
	// Merged 是这个模块融合了哪些插件（只有主源才有），用来判断要不要重转：
	// 插件字节没变但组里多了一个同名插件，模块必须重做。
	Merged []string `json:"merged,omitempty"`
	// Converter 是生成这个模块时的转换器版本。插件没变但这个值变了，
	// 说明规则升级过，必须重新生成，不能继续用旧产物。
	Converter string `json:"converter,omitempty"`
	// Adapted 记转换器做过的等价改写（属性名归一、重名脚本加后缀…），
	// Review 记还没能自动处理、要人看一眼的地方。两者都会写进提交信息。
	Adapted []string `json:"adapted,omitempty"`
	Review  []string `json:"review,omitempty"`
}

type indexFile struct {
	Source   string  `json:"source"`
	SyncedAt string  `json:"synced_at"`
	Count    int     `json:"count"`
	Plugins  []Entry `json:"plugins"`
}

// Vault 负责仓库里所有落盘位置。
type Vault struct{ root string }

// NewVault 绑定仓库根目录。
func NewVault(root string) *Vault { return &Vault{root: root} }

func (v *Vault) lpxDir() string    { return filepath.Join(v.root, "loon") }
func (v *Vault) surgeDir() string  { return filepath.Join(v.root, "surge") }
func (v *Vault) indexPath() string { return filepath.Join(v.root, "index.json") }
func (v *Vault) tablePath() string { return filepath.Join(v.root, "INDEX.md") }

func (v *Vault) lpxPath(stem string) string    { return filepath.Join(v.lpxDir(), stem+".lpx") }
func (v *Vault) modulePath(stem string) string { return filepath.Join(v.surgeDir(), stem+".sgmodule") }

// ReadState 读上一轮的索引；读不了就按空状态处理，不让一次损坏卡住同步。
//
// 键用插件地址而不是文件名：文件名跟着 #!name 走，改一次名就会被当成新插件。
func (v *Vault) ReadState() map[string]Entry {
	data, err := os.ReadFile(v.indexPath())
	if err != nil {
		return map[string]Entry{}
	}
	var parsed indexFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "! index.json 读不了，按空状态处理：%v\n", err)
		return map[string]Entry{}
	}
	state := make(map[string]Entry, len(parsed.Plugins))
	for _, entry := range parsed.Plugins {
		if key := entryKey(entry); key != "" {
			state[key] = entry
		}
	}
	return state
}

// WriteState 落盘 index.json；内容没变就不碰文件，避免每轮多出一个只改时间戳的提交。
func (v *Vault) WriteState(entries []Entry, syncedAt string) (bool, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.Stem, b.Stem) })
	return fsutil.WriteJSON(v.indexPath(), indexFile{
		Source:   IndexURL,
		SyncedAt: syncedAt,
		Count:    len(sorted),
		Plugins:  sorted,
	})
}

// WriteTable 落盘 INDEX.md，同样按内容变化与否决定是否写。
func (v *Vault) WriteTable(entries []Entry, syncedAt string) (bool, error) {
	return fsutil.WriteString(v.tablePath(), renderTable(entries, syncedAt))
}

func renderTable(entries []Entry, syncedAt string) string {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })

	var b strings.Builder
	fmt.Fprintf(&b, "# 插件索引\n\n共 %d 个插件，同步于 %s。\n\n", len(entries), syncedAt)
	b.WriteString("原始 Loon 插件在 [`loon/`](loon)，转换后的 Surge 模块在 [`surge/`](surge)。\n")
	b.WriteString("本文件由 `loon2surge sync` 生成，请勿手改。\n\n")
	b.WriteString("| 插件 | 标签 | 作者 | 上游日期 | 文件 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")

	for _, entry := range sorted {
		mark, module := "", "—"
		switch entry.Status {
		case StatusOK:
			module = fmt.Sprintf("[module](%s)", linkPath("surge", entry.Stem+".sgmodule"))
			if len(entry.Merged) > 0 {
				module += fmt.Sprintf("（融合 %d 份同名插件）", len(entry.Merged)+1)
			}
		case StatusExcluded:
			// 不是故障，只是 Surge 模块装不下这个插件的内容，别打警告符号。
			module = "Loon 专用"
		case StatusMerged:
			module = "并入「" + entry.MergedInto + "」"
		default:
			mark = " ⚠️"
		}
		fmt.Fprintf(&b, "| %s%s | %s | %s | %s | [`%s`](%s) · %s |\n",
			tableCell(entry.Name), mark,
			tableCell(strings.Join(entry.Tags, "/")),
			tableCell(strings.Join(entry.Authors, "、")),
			tableCell(entry.Date),
			entry.Stem, linkPath("loon", entry.Stem+".lpx"), module,
		)
	}
	return b.String()
}

// linkPath 生成 Markdown 链接里的路径。
//
// 文件名现在取自 #!name，可能带空格和中文（例如 `Hong Kong Toolbar去广告.lpx`），
// 直接塞进括号里会把链接截断在空格处，所以逐段转义。
func linkPath(dir, name string) string {
	return dir + "/" + url.PathEscape(name)
}

func tableCell(text string) string {
	cleaned := strings.ReplaceAll(text, "|", `\|`)
	if strings.TrimSpace(cleaned) == "" {
		return "—"
	}
	return cleaned
}

// contentChanged 判断插件条目是否真的变了（synced_at 不参与比较）。
func contentChanged(entries []Entry, previous map[string]Entry) bool {
	if len(entries) != len(previous) {
		return true
	}
	for _, entry := range entries {
		old, ok := previous[entryKey(entry)]
		if !ok || !reflect.DeepEqual(entry, old) {
			return true
		}
	}
	return false
}
