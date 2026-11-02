package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"loon2surge/internal/external"
	"loon2surge/internal/lpx"
)

// runValidate 把仓库里已经发布的模块当成品再读一遍。
//
// 转换器自检只保证"这一轮写出来的"是对的；这个命令保证"仓库里躺着的"也是对的——
// 手工改过的文件、上一版遗留的文件都会在这里暴露。
func runValidate(argv []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dir := fs.String("dir", "surge", "要校验的模块目录")
	manifestPath := fs.String("external", external.DefaultManifestPath,
		"外部模块清单，用于豁免上游写法带来的已知问题")
	if _, err := parse(fs, argv); err != nil {
		return exitCode(err)
	}

	waivers := loadWaivers(*manifestPath)

	entries, err := os.ReadDir(*dir)
	if err != nil {
		// 目录还不存在 = 还没同步过，不是错误。首次上仓库时 CI 就会走到这里。
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("目录 %s 还不存在，没有需要校验的模块\n", *dir)
			return 0
		}
		fmt.Fprintf(os.Stderr, "读不到目录 %s：%v\n", *dir, err)
		return 2
	}

	total, bad, waived := 0, 0, 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sgmodule") {
			continue
		}
		total++
		path := filepath.Join(*dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读不了 %s：%v\n", path, err)
			bad++
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".sgmodule")
		problems, skipped := waive(lpx.Validate(string(data)), waivers[stem])
		waived += skipped
		for _, problem := range problems {
			fmt.Printf("✗ %s L%d [%s] %s\n", entry.Name(), problem.Line, problem.Code, problem.Message)
		}
		if len(problems) > 0 {
			bad++
		}
	}

	switch {
	case bad > 0:
		fmt.Printf("\n校验 %d 个模块，%d 个有问题\n", total, bad)
		return 1
	case waived > 0:
		fmt.Printf("校验 %d 个模块，全部通过（外部模块按清单豁免 %d 条）\n", total, waived)
		return 0
	default:
		fmt.Printf("校验 %d 个模块，全部通过\n", total)
		return 0
	}
}

// loadWaivers 读清单里登记的豁免：模块主干 → 允许保留的问题代码。
//
// 上游手写模块有自己的习惯（比如中文参数名），既不归转换器管，也不该让转换器的
// 校验规则去改它。豁免写在清单里而不是写死在代码里，谁被放宽了、放宽了哪一条，都能查。
func loadWaivers(path string) map[string]map[string]bool {
	manifest, err := external.ReadManifest(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "! %s 读不了，本轮不豁免任何问题：%v\n", path, err)
		}
		return nil
	}

	waivers := make(map[string]map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if entry.Stem == "" || len(entry.Accepted) == 0 {
			continue
		}
		codes := waivers[entry.Stem]
		if codes == nil {
			codes = make(map[string]bool, len(entry.Accepted))
			waivers[entry.Stem] = codes
		}
		for _, code := range entry.Accepted {
			codes[code] = true
		}
	}
	return waivers
}

// waive 去掉清单允许的问题，返回剩下的和被豁免掉的条数。
func waive(problems []lpx.Problem, allowed map[string]bool) ([]lpx.Problem, int) {
	if len(allowed) == 0 {
		return problems, 0
	}
	kept := make([]lpx.Problem, 0, len(problems))
	for _, problem := range problems {
		if !allowed[problem.Code] {
			kept = append(kept, problem)
		}
	}
	return kept, len(problems) - len(kept)
}
