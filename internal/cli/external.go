package cli

import (
	"flag"
	"fmt"
	"os"

	"loon2surge/internal/external"
)

func runExternal(argv []string) int {
	opts := external.Options{}
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	fs.StringVar(&opts.Dir, "dir", external.DefaultDir, "快照目录")
	fs.StringVar(&opts.SurgeDir, "surge", external.DefaultSurge, "模块目录")
	fs.StringVar(&opts.Repo, "repo", external.DefaultRepo, "上游仓库")
	fs.StringVar(&opts.Ref, "ref", external.DefaultRef, "分支或标签")
	fs.StringVar(&opts.SourceDir, "source-dir", "", "用本地已解压的仓库代替下载（离线、测试用）")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "只报告，不写文件")

	if _, err := parse(fs, argv); err != nil {
		return exitCode(err)
	}

	report, err := external.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "外部模块同步失败：%v\n", err)
		return 1
	}
	report.Print(os.Stdout)
	return 0
}
