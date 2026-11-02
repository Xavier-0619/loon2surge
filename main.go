// loon2surge 把 Loon 插件（.lpx）转换成 Surge 模块（.sgmodule）。
//
//	loon2surge convert <文件/目录> [-o 输出] [--check] [--report 报告.json]
//	loon2surge sync    [--root .] [--jobs 4] [--fetcher auto] [--probe] [--dry-run]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"loon2surge/internal/cliargs"
	"loon2surge/internal/kelee"
	"loon2surge/internal/lpx"
)

const version = "2.0.0"

const usage = `loon2surge %s —— Loon 插件 → Surge 模块

用法：
  loon2surge convert <文件|目录|->  [选项]     把 .lpx 转成 .sgmodule
  loon2surge sync                   [选项]     同步可莉插件中心并转换
  loon2surge <文件|目录>                       等价于 convert

convert 选项：
  -o, --output    输出文件或目录，默认与输入同目录
  --report        把转换诊断写成 JSON 报告
  --category      模块 #!category，默认「插件转换」
  --no-banner     不写转换说明注释
  --check         只检查，不落盘

sync 选项：
  --root          仓库根目录，默认当前目录
  --jobs          并发数，默认 8
  --mirror        备用站点前缀（也可用环境变量 KLEE_MIRROR）
  --fetcher       下载通道：auto（默认，有 python3 就借它的 TLS 栈）/ python / go
  --commit-file   把提交信息（模块清单 + 可莉更新时间）写到指定文件
  --index-file    用本地 list.json 代替在线列表
  --only          只处理这些文件名（逗号分隔）
  --limit         只处理前 N 个（调试用）
  --dry-run       只报告差异，不写文件
  --probe         只做连通性自检

示例：
  loon2surge convert Loon/知乎去广告.lpx -o Surge/
  loon2surge convert Loon -o Surge --report build/report.json
  loon2surge sync --root . --jobs 4
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Printf(usage, version)
		os.Exit(2)
	}

	switch args[0] {
	case "convert":
		os.Exit(runConvert(args[1:]))
	case "sync":
		os.Exit(kelee.Main(args[1:]))
	case "version", "-v", "--version":
		fmt.Println("loon2surge " + version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default: // 允许 `loon2surge a.lpx` 直接转换
		os.Exit(runConvert(args))
	}
}

// ---------------------------------------------------------------------------
// convert
// ---------------------------------------------------------------------------

var (
	pluginSuffixes = map[string]bool{".lpx": true, ".plugin": true, ".txt": true}
	unsafeFilename = regexp.MustCompile(`[\\/:*?"<>|]`)
)

func runConvert(argv []string) int {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	output := fs.String("o", "", "输出文件或输出目录，默认与输入同目录")
	fs.StringVar(output, "output", "", "输出文件或输出目录")
	reportPath := fs.String("report", "", "把转换诊断写成 JSON 报告")
	category := fs.String("category", "插件转换", "模块 #!category")
	noBanner := fs.Bool("no-banner", false, "不写转换说明注释")
	check := fs.Bool("check", false, "只检查，不落盘")

	flags, targets := cliargs.Split(argv, map[string]bool{
		"o": true, "output": true, "report": true, "category": true,
	})
	if err := fs.Parse(flags); err != nil {
		return 2
	}

	options := lpx.Options{Category: *category, Banner: !*noBanner}

	if len(targets) == 1 && targets[0] == "-" {
		return convertStdin(*output, options, *check)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "没有指定输入，用 `loon2surge help` 看用法")
		return 2
	}

	files, err := collect(targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "没有找到插件文件（.lpx/.plugin/.txt）")
		return 2
	}

	results := convertAll(files, options)
	records := make([]moduleRecord, 0, len(files))
	failed := 0

	for index, path := range files {
		result := results[index]
		target := ""
		if !*check {
			target = targetPath(path, *output, result.Name)
			if result.OK() {
				if err := writeText(target, result.Text); err != nil {
					fmt.Fprintf(os.Stderr, "写入失败 %s：%v\n", target, err)
					failed++
					continue
				}
			}
		}
		printResult(path, target, result)
		if !result.OK() {
			failed++
		}
		records = append(records, toRecord(path, result))
	}

	if *reportPath != "" {
		payload := reportFile{Tool: "loon2surge", Total: len(files), Failed: failed, Modules: records}
		if err := writeJSON(*reportPath, payload); err != nil {
			fmt.Fprintf(os.Stderr, "报告写入失败 %s：%v\n", *reportPath, err)
		} else {
			fmt.Printf("\n报告已写入 %s\n", *reportPath)
		}
	}

	if failed > 0 {
		return 1
	}
	return 0
}

func convertStdin(output string, options lpx.Options, check bool) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读标准输入失败：%v\n", err)
		return 2
	}
	result := lpx.ConvertText(string(data), options)
	if !check && result.OK() {
		if output == "" {
			output = "module.sgmodule"
		}
		if err := writeText(output, result.Text); err != nil {
			fmt.Fprintf(os.Stderr, "写入失败 %s：%v\n", output, err)
			return 2
		}
	}
	printResult("stdin", output, result)
	if !result.OK() {
		return 1
	}
	return 0
}

// convertAll 并行转换；文件都很小，瓶颈在读盘和正则，多核能直接吃到收益。
func convertAll(files []string, options lpx.Options) []lpx.Result {
	results := make([]lpx.Result, len(files))
	workers := runtime.NumCPU()
	if workers > len(files) {
		workers = len(files)
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	queue := make(chan int)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				result, err := lpx.ConvertFile(files[index], options)
				if err != nil {
					result = lpx.Result{
						Name:  filepath.Base(files[index]),
						Notes: []lpx.Note{{Code: "read-error", Message: err.Error(), Fatal: true}},
					}
				}
				results[index] = result
			}
		}()
	}
	for index := range files {
		queue <- index
	}
	close(queue)
	wg.Wait()
	return results
}

func collect(targets []string) ([]string, error) {
	var files []string
	for _, target := range targets {
		info, err := os.Stat(target)
		if err != nil {
			return nil, fmt.Errorf("找不到输入：%s", target)
		}
		if !info.IsDir() {
			files = append(files, target)
			continue
		}
		err = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if pluginSuffixes[strings.ToLower(filepath.Ext(path))] {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func targetPath(source, output, name string) string {
	filename := safeFilename(name) + ".sgmodule"
	if output == "" {
		return strings.TrimSuffix(source, filepath.Ext(source)) + ".sgmodule"
	}
	if info, err := os.Stat(output); err == nil && info.IsDir() {
		return filepath.Join(output, filename)
	}
	if filepath.Ext(output) != "" {
		return output
	}
	return filepath.Join(output, filename)
}

func safeFilename(name string) string {
	cleaned := strings.Trim(unsafeFilename.ReplaceAllString(name, "-"), " .")
	if cleaned == "" {
		return "module"
	}
	return cleaned
}

func writeText(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeText(path, string(data)+"\n")
}

func printResult(source, target string, result lpx.Result) {
	head := result.Name
	if target != "" {
		head = filepath.Base(source) + " → " + filepath.Base(target)
	}
	if result.OK() {
		fmt.Printf("✓ %s\n", head)
	} else {
		fmt.Printf("✗ %s（未生成）\n", head)
	}

	const limit = 12
	for index, note := range result.Notes {
		if index >= limit {
			fmt.Printf("  … 另有 %d 条诊断，见报告\n", len(result.Notes)-limit)
			break
		}
		mark := "-"
		if note.Fatal {
			mark = "!"
		}
		fmt.Printf("  %s %s\n", mark, note.String())
	}
}

type moduleRecord struct {
	Source   string         `json:"source"`
	Name     string         `json:"name"`
	Sections map[string]int `json:"sections"`
	Warnings map[string]int `json:"warnings"`
	Fatal    []string       `json:"fatal"`
	Notes    []string       `json:"notes"`
}

type reportFile struct {
	Tool    string         `json:"tool"`
	Total   int            `json:"total"`
	Failed  int            `json:"failed"`
	Modules []moduleRecord `json:"modules"`
}

func toRecord(path string, result lpx.Result) moduleRecord {
	sections := make(map[string]int, len(result.Sections))
	for name, lines := range result.Sections {
		sections[name] = len(lines)
	}
	fatal := make([]string, 0, 2)
	for _, note := range result.Fatal() {
		fatal = append(fatal, note.String())
	}
	notes := make([]string, 0, len(result.Notes))
	for _, note := range result.Notes {
		notes = append(notes, note.String())
	}
	return moduleRecord{
		Source:   path,
		Name:     result.Name,
		Sections: sections,
		Warnings: lpx.CountByCode(result.Warnings()),
		Fatal:    fatal,
		Notes:    notes,
	}
}
