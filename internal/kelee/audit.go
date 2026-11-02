package kelee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// 远程脚本审计。
//
// 转换只搬运 script-path 字符串，脚本本身在内网外。上游哪天把文件挪走、加个
// 防盗链，模块照样能导入，只是跑起来没反应——这种"看起来正常"的坏法最难发现。
//
// 这个命令把产物里引用到的脚本都抓一遍，记录状态码和 SHA-256：
//   - 报告只是一份"当前长什么样"的快照，不阻断发布；
//   - `--strict` 时下载失败会让命令返回非零，供 CI 单独当检查项用；
//   - 命中的 URL 全部列出，包括被 `#` 注释掉的脚本行（用户随时可能打开它们）。

// scriptRef 抓的是 `script-path=` 后面那一段，引号由后面统一剥掉。
var scriptRef = regexp.MustCompile(`script-path=("[^"]*"|\S+)`)

// ScriptAudit 是一条脚本的检查结果。
type ScriptAudit struct {
	URL     string   `json:"url"`
	Status  int      `json:"status"`
	Bytes   int64    `json:"bytes"`
	SHA256  string   `json:"sha256,omitempty"`
	Error   string   `json:"error,omitempty"`
	Modules []string `json:"modules"`
}

type auditReport struct {
	GeneratedAt string        `json:"generated_at"`
	Total       int           `json:"total"`
	OK          int           `json:"ok"`
	Failed      int           `json:"failed"`
	Scripts     []ScriptAudit `json:"scripts"`
}

// collectScriptRefs 从一个目录的模块里收集脚本地址（同一个地址合并模块名）。
func collectScriptRefs(dir string) (map[string][]string, error) {
	found := map[string][]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sgmodule") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, match := range scriptRef.FindAllStringSubmatch(string(data), -1) {
			url := strings.Trim(match[1], `"'`)
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
				continue
			}
			if !slices.Contains(found[url], entry.Name()) {
				found[url] = append(found[url], entry.Name())
			}
		}
	}
	return found, nil
}

// fetchOnce 抓一次脚本，只读前 8 MB——审计要看的是"能不能拿到"，不是内容全文。
func fetchOnce(client *http.Client, url string) ScriptAudit {
	result := ScriptAudit{URL: url}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	request.Header.Set("User-Agent", UserAgent)
	response, err := client.Do(request)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer response.Body.Close()

	result.Status = response.StatusCode
	if response.StatusCode != http.StatusOK {
		result.Error = "HTTP " + response.Status
		return result
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	sum := sha256.Sum256(body)
	result.SHA256 = hex.EncodeToString(sum[:])
	result.Bytes = int64(len(body))
	return result
}

// AuditMain 是 `loon2surge audit` 的入口。
func AuditMain(argv []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	dir := fs.String("dir", "surge", "模块目录")
	out := fs.String("out", "", "报告路径，默认 <dir>/script-audit.json")
	jobs := fs.Int("jobs", 8, "并发数")
	timeout := fs.Int("timeout", 15, "单个脚本的超时（秒）")
	strict := fs.Bool("strict", false, "有脚本抓不到时返回非零")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *jobs < 1 {
		*jobs = 1
	}
	target := *out
	if target == "" {
		target = filepath.Join(*dir, "script-audit.json")
	}

	refs, err := collectScriptRefs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到模块目录 %s：%v\n", *dir, err)
		return 2
	}

	urls := make([]string, 0, len(refs))
	for url := range refs {
		urls = append(urls, url)
	}
	sort.Strings(urls)

	client := &http.Client{Timeout: time.Duration(*timeout) * time.Second}
	results := make([]ScriptAudit, len(urls))
	queue := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < *jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				got := fetchOnce(client, urls[index])
				got.Modules = refs[urls[index]]
				results[index] = got
			}
		}()
	}
	for index := range urls {
		queue <- index
	}
	close(queue)
	wg.Wait()

	report := auditReport{
		GeneratedAt: time.Now().In(beijing).Format("2006-01-02 15:04"),
		Total:       len(results),
	}
	var failures []ScriptAudit
	for _, item := range results {
		if item.Error == "" {
			report.OK++
			continue
		}
		report.Failed++
		failures = append(failures, item)
	}
	report.Scripts = results

	if err := writeJSONFile(target, report); err != nil {
		fmt.Fprintf(os.Stderr, "报告写入失败 %s：%v\n", target, err)
		return 2
	}

	fmt.Printf("审计 %d 个脚本：%d 个可下载，%d 个失败\n", report.Total, report.OK, report.Failed)
	for index, item := range failures {
		if index >= 10 {
			break
		}
		fmt.Printf("  ✗ %s（%s）：%s\n", item.URL, strings.Join(item.Modules, "、"), item.Error)
	}
	if len(failures) > 10 {
		fmt.Printf("  … 另有 %d 个失败，见报告\n", len(failures)-10)
	}
	fmt.Printf("报告已写入 %s\n", target)

	if *strict && report.Failed > 0 {
		return 1
	}
	return 0
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
