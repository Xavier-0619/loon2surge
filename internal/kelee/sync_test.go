package kelee

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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

// sourcesFor 只保留本地站点，避免测试真的去访问 kelee.one。
func (s *fakeSite) sources() *Sources {
	src := NewSources("")
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
		t.Errorf("stem 不对：%s", remotes[0].Stem)
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

	module := readFile(t, vault.modulePath("Demo"))
	if !strings.Contains(module, "[MITM]") || !strings.Contains(module, "hostname = %APPEND% a.com") {
		t.Errorf("模块内容不对：\n%s", module)
	}
	if !strings.Contains(readFile(t, vault.indexPath()), `"lpx_sha"`) {
		t.Errorf("索引里应记录内容哈希")
	}

	// 第二轮：上游没变，既不该重下重转，也不该改文件
	previous := vault.ReadState()
	before := readFile(t, vault.modulePath("Demo"))
	syncer2 := newSyncer(vault, site.sources(), 4, 0)
	entries = syncer2.run(remotes, previous, true, true)
	added, updated, removed, failed, blocked := syncer2.stats()

	if len(added)+len(updated)+len(removed)+len(failed) != 0 || blocked {
		t.Errorf("没有变化却报了变更：+%v ~%v -%v !%v", added, updated, removed, failed)
	}
	if readFile(t, vault.modulePath("Demo")) != before {
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

func TestSyncDetectsUpdate(t *testing.T) {
	path := "/Tool/Loon/Lpx/Demo.lpx"
	site := newFakeSite(t, map[string]string{path: samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)
	remotes := site.remotes(t)

	syncer := newSyncer(vault, site.sources(), 2, 0)
	entries := syncer.run(remotes, map[string]Entry{}, true, true)
	previous := map[string]Entry{entries[0].Stem: entries[0]}

	site.plugins[path] = strings.Replace(samplePlugin,
		"DOMAIN-SUFFIX,ads.example.com,REJECT",
		"DOMAIN-SUFFIX,ads.example.com,REJECT\nDOMAIN,banner.example.com,REJECT", 1)

	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	syncer2.run(remotes, previous, true, true)
	added, updated, _, _, _ := syncer2.stats()

	if len(added) != 0 || len(updated) != 1 {
		t.Fatalf("应报 1 个更新：+%v ~%v", added, updated)
	}
	if !strings.Contains(readFile(t, vault.modulePath("Demo")), "banner.example.com") {
		t.Errorf("模块没有跟上新内容")
	}
}

func TestSyncPrunesRemoved(t *testing.T) {
	keep := "/Tool/Loon/Lpx/Keep.lpx"
	drop := "/Tool/Loon/Lpx/Drop.lpx"
	site := newFakeSite(t, map[string]string{keep: samplePlugin, drop: samplePlugin})
	root := t.TempDir()
	vault := NewVault(root)

	syncer := newSyncer(vault, site.sources(), 2, 0)
	entries := syncer.run(site.remotes(t), map[string]Entry{}, true, true)
	previous := map[string]Entry{}
	for _, entry := range entries {
		previous[entry.Stem] = entry
	}
	if _, err := os.Stat(vault.lpxPath("Drop")); err != nil {
		t.Fatalf("首轮应落盘：%v", err)
	}

	delete(site.plugins, drop) // 上游下架
	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	syncer2.run(site.remotes(t), previous, true, true)

	if _, err := os.Stat(vault.lpxPath("Drop")); !os.IsNotExist(err) {
		t.Errorf("下架的插件应被删除")
	}
	if _, err := os.Stat(vault.modulePath("Drop")); !os.IsNotExist(err) {
		t.Errorf("下架的模块应被删除")
	}
	if _, err := os.Stat(vault.lpxPath("Keep")); err != nil {
		t.Errorf("在架的插件不该被牵连：%v", err)
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
	previous := map[string]Entry{entries[0].Stem: entries[0]}
	before := readFile(t, vault.modulePath("Demo"))

	site.denyAll.Store(true) // 整站被 Cloudflare 拦
	syncer2 := newSyncer(vault, site.sources(), 2, 0)
	entries = syncer2.run(remotes, previous, true, true)
	_, _, _, _, blocked := syncer2.stats()

	if !blocked {
		t.Errorf("403 应被标记为被拦截")
	}
	if entries[0].Status != StatusOK {
		t.Errorf("下载失败时不该把已有条目降级：%+v", entries[0])
	}
	if readFile(t, vault.modulePath("Demo")) != before {
		t.Errorf("被拦截时不该动已有模块")
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
