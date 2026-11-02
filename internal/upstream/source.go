package upstream

// 可莉插件中心的元信息：地址、身份、列表解析，以及"拿到的东西是不是插件"的判断。
//
// 上游的 date 字段是站点重建时间（几百条完全相同），不能当版本号用，
// 所以"对比更新"一律靠下载后的内容哈希：哈希没变就不碰文件，Git 里也就没有噪音。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

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
	// 请求上只带 UA / Accept / Accept-Language 三个头，细节见 fetch.go 顶部注释。

	// StatusOK 表示插件与模块都已就绪。
	StatusOK = "ok"
	// StatusExcluded 表示插件本身没问题，只是 Surge 模块表达不了（例如规则用 PROXY）。
	// 这类不生成模块，但在报告里与"转换失败"分开写，免得天天报警。
	StatusExcluded = "loon-only"
	// StatusMerged 表示这个插件没有自己的模块：规则已并进另一个同名插件。
	// 文件照样跟踪（地址、哈希都在），只是不再单独产出一个文件。
	StatusMerged = "merged"
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

// loadRemotes 解析 list.json；地址或名字缺失的条目直接跳过。
func loadRemotes(data []byte) ([]Remote, error) {
	var doc rawList
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("插件列表不是预期的 JSON：%w", err)
	}

	remotes := make([]Remote, 0, len(doc.Lists))
	used := make(map[string]bool, len(doc.Lists))
	for _, item := range doc.Lists {
		target := pluginURL(item.URL)
		name := strings.TrimSpace(item.Name)
		if target == "" || name == "" {
			continue
		}
		remotes = append(remotes, Remote{
			Stem:    UniqueStem(stemOf(target), used),
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
	if _, tail, found := strings.Cut(value, "plugin="); found {
		value = tail
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

// stemOf 从地址里抠出文件名主干，仅作 #!name 缺失时的兜底。
func stemOf(raw string) string {
	path := raw
	if parsed, err := url.Parse(raw); err == nil {
		path = parsed.Path
	}
	tail := path
	if index := strings.LastIndexByte(tail, '/'); index >= 0 {
		tail = tail[index+1:]
	}
	if index := strings.LastIndexByte(tail, '.'); index > 0 {
		tail = tail[:index]
	}
	stem := strings.Trim(badStemChars.ReplaceAllString(tail, "-"), "-")
	if stem == "" {
		return "unknown"
	}
	return stem
}

// UniqueStem 给文件名主干加后缀，避免两个插件写进同一个文件。
//
// 比较时不分大小写：macOS 与 Windows 的文件系统也不分，只差大小写的一对名字
// 在 Linux 上能共存、在开发机上会互相覆盖。
func UniqueStem(stem string, used map[string]bool) string {
	candidate := stem
	for index := 2; used[strings.ToLower(candidate)]; index++ {
		candidate = fmt.Sprintf("%s-%d", stem, index)
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

// authorNames 只取作者对象的 name，其余字段（链接、头像）在索引里没有用处。
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
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

// looksLikePlugin 判断拿到的是插件正文还是拦截页 / 错误页。
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

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
