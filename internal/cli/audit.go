package cli

import (
	"flag"

	"loon2surge/internal/upstream"
)

func runAudit(argv []string) int {
	opts := upstream.AuditOptions{}
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.StringVar(&opts.Dir, "dir", "surge", "模块目录")
	fs.StringVar(&opts.Out, "out", "", "报告路径，默认 <dir>/script-audit.json")
	fs.IntVar(&opts.Jobs, "jobs", 8, "并发数")
	fs.IntVar(&opts.Timeout, "timeout", 15, "单个脚本的超时（秒）")
	fs.BoolVar(&opts.Strict, "strict", false, "有脚本抓不到时返回非零")

	if _, err := parse(fs, argv); err != nil {
		return exitCode(err)
	}
	return upstream.RunAudit(opts)
}
