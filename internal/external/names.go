package external

// 模块身份与去重：一律按 #!name 判断，文件名只是结果。
//
// 上游同一件事常有七八种叫法（「去广告」「会员解锁」「净化」），所以名字比对
// 要先把"怎么处理"这类词摘掉，剩下的才是"对谁处理"。摘完还一样，就是同一个模块。

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"loon2surge/internal/lpx"
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
				Status: statusUpstreamDu, Reason: "内容与 " + first + " 完全相同",
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
					Status: statusUpstreamDu, Reason: "同名保留内容更全的 " + source.Path,
				})
				kept[index] = source
				byHash[source.Stamp] = source.Path
			} else {
				dropped = append(dropped, Entry{
					Name: source.Name, Path: source.Path, SHA: source.Stamp, Bytes: len(source.Data),
					Status: statusUpstreamDu, Reason: "同名保留内容更全的 " + kept[index].Path,
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
		if entry.Status != statusMerged || entry.Stem == "" {
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

// retireReason 判断上一轮并进来的模块为什么不该留了。
func retireReason(old Entry, current map[string]Entry) string {
	entry, ok := current[normName(old.Name)]
	if !ok {
		return "上游已不再提供这个模块"
	}
	switch entry.Status {
	case statusDuplicate:
		return "可莉已有同名模块（" + entry.Reason + "）"
	case statusIncompat:
		return "上游改动后不再兼容：" + entry.Reason
	default:
		return ""
	}
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

// similarity 是字符级的多重集相似度（类似 Dice 系数），够用就行，不引第三方库。
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	left, right := []rune(a), []rune(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	count := make(map[rune]int, len(left))
	for _, char := range left {
		count[char]++
	}
	common := 0
	for _, char := range right {
		if count[char] > 0 {
			count[char]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(left)+len(right))
}
