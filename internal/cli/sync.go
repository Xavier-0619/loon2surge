package cli

import (
	"flag"
	"os"

	"loon2surge/internal/upstream"
)

func runSync(argv []string) int {
	cfg := upstream.Config{}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.StringVar(&cfg.Root, "root", ".", "仓库根目录")
	fs.StringVar(&cfg.IndexURL, "index-url", upstream.IndexURL, "插件列表地址")
	fs.StringVar(&cfg.IndexFile, "index-file", "", "用本地 list.json 代替在线列表（离线调试）")
	fs.StringVar(&cfg.Mirror, "mirror", os.Getenv("KLEE_MIRROR"), "备用站点前缀，例如 https://mirror.example.com")
	fs.StringVar(&cfg.Fetcher, "fetcher", "auto", "下载通道：auto（有 python3 就用）/ python / go")
	fs.StringVar(&cfg.CommitFile, "commit-file", "", "把提交信息写到这个文件（action 里交给 git commit -F）")
	fs.StringVar(&cfg.Only, "only", "", "只处理这些插件（文件名、地址或上一轮的模块名，逗号分隔）")
	fs.IntVar(&cfg.Limit, "limit", 0, "只处理前 N 个（调试用）")
	fs.IntVar(&cfg.Jobs, "jobs", 8, "并发下载数")
	fs.Float64Var(&cfg.Delay, "delay", 0.15, "每个请求前随机等待上限（秒）")
	fs.IntVar(&cfg.MaxFailures, "max-failures", 25, "失败超过这个数就返回非零")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "只报告差异，不写文件")
	fs.BoolVar(&cfg.Probe, "probe", false, "只做连通性自检")

	if _, err := parse(fs, argv); err != nil {
		return exitCode(err)
	}
	return upstream.Run(cfg)
}
