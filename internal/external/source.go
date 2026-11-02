package external

// 抓上游、拆包、挑出模块：只认 .sgmodule，脚本和图片一律不收。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loon2surge/internal/lpx"
)

// fetchTarball 下载 GitHub 的源码包。两条 URL 都试：带 refs/heads 的那条对分支有效，
// 纯 sha 只能走另一条。
func fetchTarball(repo, ref, token string) ([]byte, error) {
	urls := []string{
		fmt.Sprintf("https://codeload.github.com/%s/tar.gz/refs/heads/%s", repo, ref),
		fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", repo, ref),
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	var lastErr error
	for _, url := range urls {
		request, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "loon2surge")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
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

// readTarball 从 GitHub 的 tar.gz 里挑出模块文件。
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
		case source != nil:
			sources = append(sources, *source)
		}
	}
	return sources, dropped, nil
}

// readLocal 读一个已解压的仓库目录，规则与拆包一致。
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
		case source != nil:
			sources = append(sources, *source)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sources, dropped, nil
}

// classify 判断一个文件是不是能用的 Surge 模块，顺手做规范化。
// 返回 source 表示可收，返回 entry 表示"看一眼就知道不该收"，两个都 nil 表示不是模块。
func classify(path string, data []byte, repo string) (*Source, *Entry) {
	lower := strings.ToLower(path)
	// .sgmodulle 是上游真实存在的拼写错误，照收。
	if !strings.HasSuffix(lower, ".sgmodule") && !strings.HasSuffix(lower, ".sgmodulle") {
		return nil, nil
	}
	text, notes := tidy(string(data), repo)
	name := lpx.PluginName(text)
	entry := &Entry{Name: name, Path: path, SHA: stamp(text), Bytes: len(text)}

	if name == "" {
		// 没有 #!name 的模块 Surge 直接拒收；留在快照里没有意义，只记一笔。
		entry.Status = statusNoName
		entry.Reason = "缺少 #!name，Surge 不会认这个模块"
		return nil, entry
	}
	if strings.Contains(lower, "shadowrocket") || isLoonOnly(text) {
		entry.Status = statusNonSurge
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

func stamp(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
