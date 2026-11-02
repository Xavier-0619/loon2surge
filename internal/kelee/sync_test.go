package kelee

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"loon2surge/internal/lpx"
)

const samplePlugin = `#!name=测试插件
#!desc=单元测试用的假插件
#!author=tester

[Rule]
DOMAIN-SUFFIX,ads.example.com,REJECT

[Rewrite]
^https:\/\/a\.com\/ad url reject-dict

[MITM]
hostname = %APPEND% a.com
`

// namedPlugin 换掉 #!name，用来造多个"不同名"的假插件。
func namedPlugin(name string) string {
	return strings.Replace(samplePlugin, "#!name=测试插件", "#!name="+name, 1)
}

type fakeSite struct {
	server   *httptest.Server
	plugins  map[string]string // path -> 内容
	denyAll  atomic.Bool
	requests atomic.Int32
}

func newFakeSite(t *testing.T, plugins map[string]string) *fakeSite {
	t.Helper()
	site := &fakeSite{plugins: plugins}

	mux := http.NewServeMux()
	mux.HandleFunc("/list.json", func(w http.ResponseWriter, r *http.Request) {
		site.requests.Add(1)
		if site.denyAll.Load() {
			http.Error(w, "Sorry, you have been blocked", http.StatusForbidden)
			return
		}
		items := make([]map[string]any, 0, len(site.plugins))
		for path := range site.plugins {
			items = append(items, map[string]any{
				"name":   strings.TrimSuffix(filepath.Base(path), ".lpx"),
				"desc":   "假数据",
				"tag":    []string{"测试"},
				"date":   "2026-10-02 13:57:49",
				"url":    "loon://import?plugin=" + site.server.URL + path,
				"author": []map[string]any{{"name": "tester"}},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "测试站", "lists": items})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		site.requests.Add(1)
		if site.denyAll.Load() {
			http.Error(w, "blocked", http.StatusForbidden)
			return
		}
		body, ok := site.plugins[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})

	site.server = httptest.NewServer(mux)
	t.Cleanup(site.server.Close)
	return site
}

// sources 只保留本地站点，避免测试真的去访问 kelee.one；
// 用 Go 通道跑，省掉每个请求一个 python 进程的开销。
func (s *fakeSite) sources() *Sources {
	src, _ := NewSourcesWith("", "go")
	src.hosts = []string{s.server.URL}
	return src
}

func (s *fakeSite) remotes(t *testing.T) []Remote {
	t.Helper()
	data, err := s.sources().Fetch(s.server.URL + "/list.json")
	if err != nil {
		t.Fatalf("拉列表失败：%v", err)
	}
	remotes, err := loadRemotes(data)
	if err != nil {
		t.Fatalf("解析列表失败：%v", err)
	}
	return remotes
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s：%v", path, err)
	}
	return string(data)
}

func stateOf(entries []Entry) map[string]Entry {
	state := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		state[entryKey(entry)] = entry
	}
	return state
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

func TestLoadRemotes(t *testing.T) {
	raw := []byte(`{"lists":[
	  {"name":"知乎去广告","desc":"d","tag":["去广告"],"date":"2026-10-02",
	   "url":"loon://import?plugin=https%3A%2F%2Fkelee.one%2FTool%2FLoon%2FLpx%2FZhihu_remove_ads.lpx",
	   "author":[{"name":"可莉"}]},
	  {"name":"重名","url":"loon://import?plugin=https%3A%2F%2Fa.com%2Fx%2FSame.lpx"},
	  {"name":"重名2","url":"https://a.com/y/Same.lpx"},
	  {"name":"没地址","url":"not-a-link"},
	  {"url":"https://a.com/no-name.lpx"}
	]}`)

	remotes, err := loadRemotes(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(remotes) != 3 {
		t.Fatalf("期望 3 条有效记录，得到 %d", len(remotes))
	}
	if remotes[0].URL != "https://kelee.one/Tool/Loon/Lpx/Zhihu_remove_ads.lpx" {
		t.Errorf("loon:// 没有解出真实地址：%s", remotes[0].URL)
	}
	if remotes[0].Stem != "Zhihu_remove_ads" {
		t.Errorf("兜底名字不对：%s", remotes[0].Stem)
	}
	if remotes[0].Authors[0] != "可莉" {
		t.Errorf("作者没解析出来：%v", remotes[0].Authors)
	}
	if remotes[1].Stem == remotes[2].Stem {
		t.Errorf("同名文件应自动加后缀区分：%s / %s", remotes[1].Stem, remotes[2].Stem)
	}
}

func TestRenderTableEscapesPipe(t *testing.T) {
	table := renderTable([]Entry{{
		Name: "含|竖线", Stem: "x", Status: StatusOK, Tags: []string{"a/b"}, Date: "2026-10-02",
	}}, "2026-11-02 00:00:00 UTC")

	if !strings.Contains(table, `含\|竖线`) {
		t.Errorf("竖线没有转义，表格会被破坏：\n%s", table)
	}
}

// ---------------------------------------------------------------------------
// 文件名
// ---------------------------------------------------------------------------

func TestPluginName(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"正常", samplePlugin, "测试插件"},
		{"大小写", "#!Name=大写\n[Rule]\nDOMAIN,a.com,REJECT\n", "大写"},
		{"缺字段", "#!desc=没名字\n[Rule]\nDOMAIN,a.com,REJECT\n", ""},
		{"section 之后不再找", "#!desc=d\n[Rule]\n#!name=晚到的\n", ""},
		{"注释里不算", "# 说明\n#!name=真名\n", "真名"},
	}
	for _, c := range cases {
		if got := lpx.PluginName(c.text); got != c.want {
			t.Errorf("%s：得到 %q，想要 %q", c.name, got, c.want)
		}
	}
}

func TestFileStem(t *testing.T) {
	cases := []struct {
		name, fallback, want string
	}{
		{"前程无忧去广告", "51Job_remove_ads", "前程无忧去广告"},
		{"贴吧/知乎 去广告", "x", "贴吧-知乎 去广告"},
		{"  前后空白  ", "x", "前后空白"},
		{"结尾句点.", "x", "结尾句点"},
		{"a:b*c?d\"e<f>g|h\\i", "x", "a-b-c-d-e-f-g-h-i"},
		{"", "Fallback", "Fallback"},
		{"...", "Fallback", "Fallback"},
		{"CON", "x", "_CON"},
		{"中文\n换行", "x", "中文 换行"},
	}
	for _, c := range cases {
		if got := fileStem(c.name, c.fallback); got != c.want {
			t.Errorf("fileStem(%q)：得到 %q，想要 %q", c.name, got, c.want)
		}
	}

	// 200 字节以内才安全：.sgmodule 还要占 9 个字节，而文件系统只给 255
	long := strings.Repeat("长", 120)
	if got := fileStem(long, "x"); len(got) > maxStemBytes {
		t.Errorf("截断没生效：%d 字节", len(got))
	} else if !utf8.ValidString(got) {
		t.Errorf("截断把汉字切碎了：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 同步
// ---------------------------------------------------------------------------

func TestSyncPipeline(t *testing.T) {
	site := newFakeSite(t, map[string]string{"/Tool/Loon/Lpx/Demo.lpx": samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)
	remotes := site.remotes(t)

	syncer := newSyncer(vault, site.sources(), 4, 0)
	entries := syncer.run(remotes, vault.ReadState(), true, true)

	if len(entries) != 1 || entries[0].Status != StatusOK {
		t.Fatalf("应转换成功：%+v", entries)
	}
	if _, err := vault.WriteState(entries, "2026-11-02 00:00:00 UTC"); err != nil {
		t.Fatalf("写索引失败：%v", err)
	}
	if _, err := vault.WriteTable(entries, "2026-11-02 00:00:00 UTC"); err != nil {
		t.Fatalf("写表格失败：%v", err)
	}

	// 落盘文件名取自 #!name，不是地址里的 Demo
	module := readFile(t, vault.modulePath("测试插件"))
	if !strings.Contains(module, "[MITM]") || !strings.Contains(module, "hostname = %APPEND% a.com") {
		t.Errorf("模块内容不对：\n%s", module)
	}
	if _, err := os.Stat(vault.lpxPath("Demo")); !os.IsNotExist(err) {
		t.Errorf("不该按地址命名文件")
	}
	if !strings.Contains(readFile(t, vault.indexPath()), `"lpx_sha"`) {
		t.Errorf("索引里应记录内容哈希")
	}

	// 第二轮：上游没变，既不该重下重转，也不该改文件
	previous := vault.ReadState()
	before := readFile(t, vault.modulePath("测试插件"))
	syncer2 := newSyncer(vault, site.sources(), 4, 0)
	entries = syncer2.run(remotes, previous, true, true)
	got := syncer2.stats()

	if len(got.added)+len(got.updated)+len(got.renamed)+len(got.removed)+len(got.failed) != 0 || got.blocked {
		t.Errorf("没有变化却报了变更：%+v", got)
	}
	if readFile(t, vault.modulePath("测试插件")) != before {
		t.Errorf("模块被无谓重写了")
	}

	// 索引内容没变时也不该重写文件
	changed, err := vault.WriteState(entries, "2026-11-02 00:00:00 UTC")
	if err != nil {
		t.Fatalf("写索引失败：%v", err)
	}
	if changed {
		t.Errorf("内容没变却重写了 index.json，git 会天天多一个空提交")
	}

	// synced_at 变了不算内容变化，runSync 据此决定不落盘
	if contentChanged(entries, previous) {
		t.Errorf("条目没变却被判定为有变化")
	}
}

// TestSyncFollowsPluginName 是本轮需求的核心：文件名取 #!name，上游改名文件跟着改。
func TestSyncFollowsPluginName(t *testing.T) {
	path := "/Tool/Loon/Lpx/51Job_remove_ads.lpx"
	site := newFakeSite(t, map[string]string{path: namedPlugin("前程无忧去广告")})
	vault := NewVault(t.TempDir())

	syncer := newSyncer(vault, site.sources(), 2, 0)
	entries := syncer.run(site.remotes(t), map[string]Entry{}, true, true)
	if _, err := os.Stat(vault.lpxPath("前程无忧去广告")); err != nil {
		t.Fatalf("应按 #!name 落盘：%v", err)
	}

	previous := stateOf(entries)
	site.plugins[path] = namedPlugin("前程无忧去广告 Pro") // 上游改了名

	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	syncer2.run(site.remotes(t), previous, true, true)
	got := syncer2.stats()

	if len(got.renamed) != 1 || got.renamed[0].From != "前程无忧去广告" || got.renamed[0].To != "前程无忧去广告 Pro" {
		t.Fatalf("应记一次改名：%+v", got.renamed)
	}
	if len(got.added) != 0 {
		t.Errorf("改名不该算新增：%v", got.added)
	}
	if _, err := os.Stat(vault.modulePath("前程无忧去广告")); !os.IsNotExist(err) {
		t.Errorf("旧名字的模块应删掉")
	}
	if _, err := os.Stat(vault.modulePath("前程无忧去广告 Pro")); err != nil {
		t.Errorf("新名字的模块没落盘：%v", err)
	}
}

func TestSyncDedupesIdenticalNames(t *testing.T) {
	// 可莉那边真有两个"挖财记账去广告"，不能都写成同一个文件
	site := newFakeSite(t, map[string]string{
		"/Tool/Loon/Lpx/WaCaiJiZhang_remove_ads.lpx": namedPlugin("挖财记账去广告"),
		"/Tool/Loon/Lpx/Wacai_remove_ads.lpx":        namedPlugin("挖财记账去广告"),
	})
	vault := NewVault(t.TempDir())

	entries := newSyncer(vault, site.sources(), 2, 0).run(site.remotes(t), map[string]Entry{}, true, true)
	if len(entries) != 2 {
		t.Fatalf("应有两个条目：%+v", entries)
	}
	if entries[0].Stem == entries[1].Stem {
		t.Fatalf("同名插件落进了同一个文件：%s", entries[0].Stem)
	}
	if entries[1].Stem != "挖财记账去广告-2" {
		t.Errorf("重名应加后缀：%s", entries[1].Stem)
	}

	// 再来一轮必须得到同样的名字，否则每天都会改名
	previous := stateOf(entries)
	again := newSyncer(vault, site.sources(), 2, 0).run(site.remotes(t), previous, true, true)
	for i := range entries {
		if again[i].Stem != entries[i].Stem {
			t.Fatalf("同一份列表两轮名字不一致：%s / %s", entries[i].Stem, again[i].Stem)
		}
	}
}

func TestSyncDetectsUpdate(t *testing.T) {
	path := "/Tool/Loon/Lpx/Demo.lpx"
	site := newFakeSite(t, map[string]string{path: samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)
	remotes := site.remotes(t)

	syncer := newSyncer(vault, site.sources(), 2, 0)
	entries := syncer.run(remotes, map[string]Entry{}, true, true)
	previous := stateOf(entries)

	site.plugins[path] = strings.Replace(samplePlugin,
		"DOMAIN-SUFFIX,ads.example.com,REJECT",
		"DOMAIN-SUFFIX,ads.example.com,REJECT\nDOMAIN,banner.example.com,REJECT", 1)

	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	syncer2.run(remotes, previous, true, true)
	got := syncer2.stats()

	if len(got.added) != 0 || len(got.updated) != 1 {
		t.Fatalf("应报 1 个更新：+%v ~%v", got.added, got.updated)
	}
	if !strings.Contains(readFile(t, vault.modulePath("测试插件")), "banner.example.com") {
		t.Errorf("模块没有跟上新内容")
	}
}

func TestSyncPrunesRemoved(t *testing.T) {
	keep := "/Tool/Loon/Lpx/Keep.lpx"
	drop := "/Tool/Loon/Lpx/Drop.lpx"
	site := newFakeSite(t, map[string]string{keep: namedPlugin("保留插件"), drop: namedPlugin("下架插件")})
	root := t.TempDir()
	vault := NewVault(root)

	syncer := newSyncer(vault, site.sources(), 2, 0)
	entries := syncer.run(site.remotes(t), map[string]Entry{}, true, true)
	previous := stateOf(entries)
	if _, err := os.Stat(vault.lpxPath("下架插件")); err != nil {
		t.Fatalf("首轮应落盘：%v", err)
	}

	delete(site.plugins, drop) // 上游下架
	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	syncer2.run(site.remotes(t), previous, true, true)
	got := syncer2.stats()

	if _, err := os.Stat(vault.lpxPath("下架插件")); !os.IsNotExist(err) {
		t.Errorf("下架的插件应被删除")
	}
	if _, err := os.Stat(vault.modulePath("下架插件")); !os.IsNotExist(err) {
		t.Errorf("下架的模块应被删除")
	}
	if _, err := os.Stat(vault.lpxPath("保留插件")); err != nil {
		t.Errorf("在架的插件不该被牵连：%v", err)
	}
	if len(got.removed) != 1 || got.removed[0].Stem != "下架插件" {
		t.Errorf("提交信息要能列出被删的模块：%+v", got.removed)
	}
	if got.removed[0].Date != "2026-10-02 13:57:49" {
		t.Errorf("删除也要带可莉的更新时间：%+v", got.removed[0])
	}
}

func TestSyncSkipsMassRemoval(t *testing.T) {
	site := newFakeSite(t, map[string]string{"/Tool/Loon/Lpx/Only.lpx": samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)

	// 伪造"上一轮有 10 个插件"，这一轮只剩 1 个：应判定为列表不完整，不删
	previous := map[string]Entry{}
	for _, stem := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "Only"} {
		previous[stem] = Entry{Stem: stem, Name: stem, Status: StatusOK}
		if err := writeFile(vault.lpxPath(stem), []byte(samplePlugin)); err != nil {
			t.Fatal(err)
		}
	}

	syncer := newSyncer(vault, site.sources(), 2, 0)
	syncer.run(site.remotes(t), previous, true, true)

	if _, err := os.Stat(vault.lpxPath("A")); err != nil {
		t.Errorf("一次少了太多插件时应保留旧文件，避免上游抽风清空仓库")
	}
}

func TestSyncKeepsFilesWhenBlocked(t *testing.T) {
	path := "/Tool/Loon/Lpx/Demo.lpx"
	site := newFakeSite(t, map[string]string{path: samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)

	syncer := newSyncer(vault, site.sources(), 2, 0)
	remotes := site.remotes(t)
	entries := syncer.run(remotes, map[string]Entry{}, true, true)
	previous := stateOf(entries)
	before := readFile(t, vault.modulePath("测试插件"))

	site.denyAll.Store(true) // 整站被 Cloudflare 拦
	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	entries = syncer2.run(remotes, previous, true, true)
	got := syncer2.stats()

	if !got.blocked {
		t.Errorf("403 应被标记为被拦截")
	}
	if entries[0].Status != StatusOK {
		t.Errorf("下载失败时不该把已有条目降级：%+v", entries[0])
	}
	if readFile(t, vault.modulePath("测试插件")) != before {
		t.Errorf("被拦截时不该动已有模块")
	}
}

// TestRequestShapeMatchesLoonClient 锁住另一类事故的经验：
// 只有"UA = Loon + 只发三个头 + 只走 HTTP/1.1 + 不要 gzip"这一套能在 runner 上拿到 200。
// 任何一项被"优化"回去（换浏览器 UA、加 Referer、默认走 h2、自动加 Accept-Encoding），
// 表现都是整轮 403，而不是某一条失败，所以值得钉死。
func TestRequestShapeMatchesLoonClient(t *testing.T) {
	var seen http.Header
	var proto string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		proto = r.Proto
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	sources, err := NewSourcesWith("", "go") // 这条用例检查的是 Go 通道的请求形状
	if err != nil {
		t.Fatalf("建通道失败：%v", err)
	}
	if _, err := sources.Fetch(server.URL + "/Tool/Loon/Lpx/a.lpx"); err != nil {
		t.Fatalf("请求失败：%v", err)
	}

	if got := seen.Get("User-Agent"); got != UserAgent {
		t.Errorf("UA 被改了：%q", got)
	}
	if got := seen.Get("Referer"); got != "" {
		t.Errorf("不该带 Referer：%q", got)
	}
	if got := seen.Get("Accept-Encoding"); got != "" {
		t.Errorf("不该发 Accept-Encoding：%q", got)
	}
	if got := seen.Get("Accept-Language"); got != AcceptLanguage {
		t.Errorf("Accept-Language 是 %q，想要 %q", got, AcceptLanguage)
	}
	if proto != "HTTP/1.1" {
		t.Errorf("应只走 HTTP/1.1，实际 %s", proto)
	}

	channel, ok := sources.sender.(*goSender)
	if !ok {
		t.Fatal("下载通道不是 Go 原生，请求特征不再可控")
	}
	transport, ok := channel.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("传输层被换掉了，请求特征不再可控")
	}
	if transport.ForceAttemptHTTP2 {
		t.Errorf("ForceAttemptHTTP2 被打开了，Go 的 h2 指纹更容易被判成爬虫")
	}
	if transport.TLSNextProto == nil {
		t.Errorf("TLSNextProto 应是空表（明确关掉 ALPN 协商 h2），nil 会回到默认行为")
	}
	if !transport.DisableCompression {
		t.Errorf("自动 gzip 没关掉，会多出一个 Accept-Encoding 头")
	}
}

// TestCommitMessage 检查提交正文：每行"时间在前、模块在后"，且只写真正动过的模块。
func TestCommitMessage(t *testing.T) {
	entries := []Entry{
		{Stem: "前程无忧去广告", Date: "2026-10-02 13:57:49", Status: StatusOK},
		{Stem: "知乎去广告", Date: "2026-10-02 13:57:49", Status: StatusOK},
	}
	now := time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC) // 北京 11-03 02:30

	message := commitMessage(changeSet{
		entries: entries,
		added:   []string{"前程无忧去广告"},
		updated: []string{"知乎去广告"},
		renamed: []Rename{{From: "Zhihu_remove_ads", To: "知乎去广告", Date: "2026-10-19 23:24:57"}},
		removed: []Entry{{Stem: "Old_Plugin", Date: "2026-10-16 10:00:00"}},
		now:     now,
	})

	for _, want := range []string{
		"🔄 自动更新 2026-11-03",
		"新增 1 · 更新 1 · 重命名 1 · 删除 1 · 失败 0 · 合计 2",
		"新增模块 1 个：",
		"- 2026-10-02 13:57:49 · 前程无忧去广告.sgmodule",
		"更新模块 1 个：",
		"- 2026-10-02 13:57:49 · 知乎去广告.sgmodule",
		"重命名 1 个（文件名跟随 #!name）：",
		"- 2026-10-19 23:24:57 · Zhihu_remove_ads → 知乎去广告",
		"删除模块 1 个（上游已下架）：",
		"- 2026-10-16 10:00:00 · Old_Plugin.sgmodule",
		"同步时间：2026-11-03 02:30:00（北京时间）",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("提交信息里缺少 %q：\n%s", want, message)
		}
	}

	// 每一条明细都必须是同一个形状：`- <时间> · <模块>`
	shape := regexp.MustCompile(`^- (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}|未知时间) · \S`)
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, "- ") && !shape.MatchString(line) {
			t.Errorf("明细格式不统一：%q", line)
		}
	}

	// 没有的类别不该留空小节
	only := commitMessage(changeSet{entries: entries, added: []string{"前程无忧去广告"}, now: now})
	for _, unwanted := range []string{"\n更新模块 ", "\n重命名 ", "\n删除模块 "} {
		if strings.Contains(only, unwanted) {
			t.Errorf("不该出现空小节 %q：\n%s", strings.TrimSpace(unwanted), only)
		}
	}

	// 缺日期时也要占住时间位，形状保持一致
	blank := commitMessage(changeSet{
		entries: []Entry{{Stem: "无日期插件"}},
		added:   []string{"无日期插件"},
		now:     now,
	})
	if !strings.Contains(blank, "- 未知时间 · 无日期插件.sgmodule") {
		t.Errorf("缺日期应写未知时间：\n%s", blank)
	}

	// 超过 60 条折叠，免得首次全量时提交信息比代码还长
	stems := make([]string, 0, 70)
	many := make([]Entry, 0, 70)
	for i := 0; i < 70; i++ {
		stem := fmt.Sprintf("Mod%02d", i)
		stems = append(stems, stem)
		many = append(many, Entry{Stem: stem, Date: "2026-10-02 13:57:49", Status: StatusOK})
	}
	folded := commitMessage(changeSet{entries: many, added: stems, now: now})
	if !strings.Contains(folded, "… 其余 10 个略") {
		t.Errorf("超过 60 条应折叠：\n%s", folded)
	}
	if got := strings.Count(folded, "- 2026-10-02 13:57:49 · "); got != commitLineLimit {
		t.Errorf("折叠后应剩 %d 条明细，实际 %d", commitLineLimit, got)
	}
}

// TestPythonChannel 确认嵌入的 pyfetch.py 真的能跑：它是绕开 Cloudflare
// TLS 指纹质询的关键一环，脚本写坏了必须在这里就暴露。
func TestPythonChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	sources, err := NewSourcesWith("", "python")
	if err != nil {
		t.Skipf("这台机器没有 python3：%v", err)
	}
	body, err := sources.Fetch(server.URL + "/x.lpx")
	if err != nil {
		t.Fatalf("python 通道下载失败：%v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("下载内容不对：%q", body)
	}
}

// TestCandidatesKeepOriginalHost 锁住一次真实事故：
// 早期实现会把索引地址 hub.kelee.one/list.json 改写成 kelee.one/list.json，
// 那是另一个服务，只会返回 404，于是整轮同步在第一步就失败。
func TestCandidatesKeepOriginalHost(t *testing.T) {
	sources := NewSources("")
	got := sources.candidates(IndexURL)
	if len(got) != 1 || got[0] != IndexURL {
		t.Fatalf("索引地址被改写成 %v，hub 的路径不能搬到 kelee.one 上", got)
	}

	withMirror := NewSources("https://mirror.example")
	got = withMirror.candidates("https://kelee.one/Tool/Loon/Lpx/a.lpx")
	want := []string{
		"https://kelee.one/Tool/Loon/Lpx/a.lpx",
		"https://mirror.example/Tool/Loon/Lpx/a.lpx",
	}
	if len(got) != len(want) {
		t.Fatalf("候选数量不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个候选是 %q，想要 %q", i, got[i], want[i])
		}
	}
}
