package kelee

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectScriptRefs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 注释掉的脚本行也算引用：用户随时可能把开关打开。
	write("a.sgmodule", "[Script]\nA = type=cron, script-path=https://e.com/a.js\n# B = type=cron, script-path=\"https://e.com/b.js\"\n")
	write("b.sgmodule", "[Script]\nC = type=cron, script-path=https://e.com/a.js\n")
	write("readme.md", "script-path=https://ignore.example/x.js")

	refs, err := collectScriptRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("应只收集 2 个脚本，得到 %d：%v", len(refs), refs)
	}
	if len(refs["https://e.com/a.js"]) != 2 {
		t.Errorf("a.js 应被两个模块引用：%v", refs["https://e.com/a.js"])
	}
	if _, ok := refs["https://ignore.example/x.js"]; ok {
		t.Errorf("非 .sgmodule 文件不该参与审计")
	}
}

func TestFetchOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("User-Agent") != UserAgent {
			t.Errorf("应以 Loon 客户端身份请求，收到 %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte("var a = 1"))
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	ok := fetchOnce(client, server.URL+"/a.js")
	if ok.Error != "" || ok.Status != 200 {
		t.Fatalf("应下载成功：%+v", ok)
	}
	if len(ok.SHA256) != 64 || ok.Bytes != 9 {
		t.Errorf("摘要或长度不对：%+v", ok)
	}

	missing := fetchOnce(client, server.URL+"/missing")
	if missing.Status != 404 || missing.Error == "" {
		t.Errorf("404 应记为失败：%+v", missing)
	}
}
