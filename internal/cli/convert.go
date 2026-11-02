package cli

import (
	"cmp"
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

	batches := loadBatches(files)
	results := convertBatches(batches, options)
	records := make([]moduleRecord, 0, len(batches))
	failed := 0

	for index, item := range batches {
		result := results[index]
		target := ""
		if !*check {
			target = targetPath(item.paths[0], *output, result.Name)
			if result.OK() {
				if _, err := fsutil.WriteString(target, result.Text); err != nil {
					fmt.Fprintf(os.Stderr, "写入失败 %s：%v\n", target, err)
					failed++
					continue
				}
			}
		}
		printResult(item, target, result)
		if !result.OK() {
			failed++
		}
		records = append(records, toRecord(item, result))
	}

	if *reportPath != "" {
		payload := reportFile{Tool: "loon2surge", Total: len(batches), Sources: len(files), Failed: failed, Modules: records}
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
	printResult(batch{}, output, result)
	if !result.OK() {
		return 1
	}
	return 0
}

// batch 是一组同名插件：转换后只产出一个模块。
//
// 上游会把同一个 App 的去广告规则拆成几份、都写同一个 #!name（挖财记账就是
// 两份，各补一半接口）。按名字各自落盘会互相覆盖，只留最后转出的那份；
// 所以同名的一律归成一组，交给 lpx 合并。
type batch struct {
	paths   []string
	sources []lpx.Source
	err     error // 读盘失败，此时 sources 为空
}

// loadBatches 读完所有插件，按 #!name 归组，保持输入顺序。
func loadBatches(files []string) []batch {
	texts := make([]string, len(files))
	errs := make([]error, len(files))

	var wg sync.WaitGroup
	for index := range files {
		wg.Go(func() {
			data, err := os.ReadFile(files[index])
			texts[index], errs[index] = string(data), err
		})
	}
	wg.Wait()

	batches := make([]batch, 0, len(files))
	at := make(map[string]int, len(files))
	for index, file := range files {
		if errs[index] != nil {
			batches = append(batches, batch{paths: []string{file}, err: errs[index]})
			continue
		}
		source := lpx.Source{Name: filepath.Base(file), Text: texts[index]}
		// 没有 #!name 就无从判断是不是同一个插件，各转各的。
		key := strings.ToLower(lpx.PluginName(texts[index]))
		if key == "" {
			batches = append(batches, batch{paths: []string{file}, sources: []lpx.Source{source}})
			continue
		}
		if slot, ok := at[key]; ok {
			batches[slot].paths = append(batches[slot].paths, file)
			batches[slot].sources = append(batches[slot].sources, source)
			continue
		}
		at[key] = len(batches)
		batches = append(batches, batch{paths: []string{file}, sources: []lpx.Source{source}})
	}
	for index := range batches {
		preferName(&batches[index])
	}
	return batches
}

// preferName 把最贴近 #!name 的文件排到最前。
//
// 同步落盘时第一个插件拿原名、后面的原始 .lpx 依次加 -2/-3，所以文件名
// 最短的那份就是最先收录的主源。合并以它为准，desc 和 icon 才不会随目录
// 遍历顺序漂移。（-2 后缀现在只出现在 loon/ 的原始快照里，模块已合一。）
func preferName(item *batch) {
	if len(item.sources) < 2 {
		return
	}
	order := make([]int, len(item.sources))
	for index := range order {
		order[index] = index
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(len(item.paths[a]), len(item.paths[b]))
	})

	paths := make([]string, len(item.paths))
	sources := make([]lpx.Source, len(item.sources))
	for slot, from := range order {
		paths[slot] = item.paths[from]
		sources[slot] = item.sources[from]
	}
	item.paths, item.sources = paths, sources
}

// convertBatches 并行转换；插件都很小，瓶颈在读盘和正则，多核能直接吃到收益。
func convertBatches(batches []batch, options lpx.Options) []lpx.Result {
	results := make([]lpx.Result, len(batches))
	if len(batches) == 0 {
		return results
	}

	queue := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), len(batches)) {
		wg.Go(func() {
			for index := range queue {
				results[index] = convertBatch(batches[index], options)
			}
		})
	}
	for index := range batches {
		queue <- index
	}
	close(queue)
	wg.Wait()
	return results
}

func convertBatch(item batch, options lpx.Options) lpx.Result {
	if item.err != nil {
		return lpx.Result{
			Name:  filepath.Base(item.paths[0]),
			Notes: []lpx.Note{{Code: "read-error", Message: item.err.Error(), Fatal: true}},
		}
	}
	return lpx.ConvertSources(item.sources, options)
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

func printResult(item batch, target string, result lpx.Result) {
	head := describeSources(item)
	if target != "" {
		head += " → " + filepath.Base(target)
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

// describeSources 写清这组插件由哪些文件合出来。
func describeSources(item batch) string {
	if len(item.paths) == 0 {
		return "stdin"
	}
	names := make([]string, 0, len(item.paths))
	for _, path := range item.paths {
		names = append(names, filepath.Base(path))
	}
	if len(names) > 2 {
		return fmt.Sprintf("%s + %s 等 %d 个", names[0], names[1], len(names))
	}
	return strings.Join(names, " + ")
}

type moduleRecord struct {
	Source   string         `json:"source"`
	Sources  []string       `json:"sources,omitempty"`
	Name     string         `json:"name"`
	Sections map[string]int `json:"sections"`
	Warnings map[string]int `json:"warnings"`
	Fatal    []string       `json:"fatal"`
	Notes    []string       `json:"notes"`
}

type reportFile struct {
	Tool    string         `json:"tool"`
	Total   int            `json:"total"`   // 产出的模块数
	Sources int            `json:"sources"` // 输入文件数，融合时比 total 多
	Failed  int            `json:"failed"`
	Modules []moduleRecord `json:"modules"`
}

func toRecord(item batch, result lpx.Result) moduleRecord {
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
	record := moduleRecord{
		Name:     result.Name,
		Sections: sections,
		Warnings: lpx.CountByCode(result.Warnings()),
		Fatal:    fatal,
		Notes:    notes,
	}
	if len(item.paths) > 0 {
		record.Source = item.paths[0]
		record.Sources = item.paths
	}
	return record
}
