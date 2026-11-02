package cli

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"loon2surge/internal/fsutil"
	"loon2surge/internal/lpx"
)

// pluginSuffixes 是 convert 认的输入后缀；目录递归时按它过滤。
var pluginSuffixes = map[string]bool{".lpx": true, ".plugin": true, ".txt": true}

var unsafeFilename = regexp.MustCompile(`[\\/:*?"<>|]`)

func runConvert(argv []string) int {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	output := fs.String("o", "", "输出文件或输出目录，默认与输入同目录")
	fs.StringVar(output, "output", "", "输出文件或输出目录")
	reportPath := fs.String("report", "", "把转换诊断写成 JSON 报告")
	category := fs.String("category", "插件转换", "模块 #!category")
	noBanner := fs.Bool("no-banner", false, "不写转换说明注释")
	check := fs.Bool("check", false, "只检查，不落盘")

	targets, err := parse(fs, argv)
	if err != nil {
		return exitCode(err)
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
				if _, err := fsutil.WriteString(target, result.Text); err != nil {
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
		if _, err := fsutil.WriteJSON(*reportPath, payload); err != nil {
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
		if _, err := fsutil.WriteString(output, result.Text); err != nil {
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
	queue := make(chan int)

	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), len(files)) {
		wg.Go(func() {
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
		})
	}
	for index := range files {
		queue <- index
	}
	close(queue)
	wg.Wait()
	return results
}

// collect 展开输入：文件直接收下，目录递归挑出插件后缀。
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
		err = filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
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
	slices.Sort(files)
	return files, nil
}

// targetPath 决定一个插件写到哪儿：给了文件就用它，给了目录就按 #!name 起名。
func targetPath(source, output, name string) string {
	filename := safeFilename(name) + ".sgmodule"
	switch {
	case output == "":
		return strings.TrimSuffix(source, filepath.Ext(source)) + ".sgmodule"
	case isDir(output):
		return filepath.Join(output, filename)
	case filepath.Ext(output) != "":
		return output
	default:
		return filepath.Join(output, filename)
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func safeFilename(name string) string {
	cleaned := strings.Trim(unsafeFilename.ReplaceAllString(name, "-"), " .")
	if cleaned == "" {
		return "module"
	}
	return cleaned
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
