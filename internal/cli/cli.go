// Package cli 实现 loon2surge 的命令行界面：解析参数、调用引擎、打印结果。
//
// 命令本身不在这里实现，只做分发与公共约定（退出码、帮助文本、标志与位置参数混写）。
package cli

import (
	"errors"
	"flag"
	"fmt"
)

// Version 是发布版本号；只随发布递增，和转换规则版本（lpx.Version）是两件事。
const Version = "2.3.0-rc.1"

const usage = `loon2surge %s —— Loon 插件 → Surge 模块

用法：
  loon2surge convert <文件|目录|->  [选项]     把 .lpx 转成 .sgmodule
  loon2surge sync                   [选项]     同步可莉插件中心并转换
  loon2surge validate               [选项]     校验已发布的模块产物
  loon2surge audit                  [选项]     检查模块引用的远程脚本还能不能下载
  loon2surge external               [选项]     把第三方仓库的 Surge 模块并入 surge
  loon2surge <文件|目录>                       等价于 convert

convert 选项：
  -o, --output    输出文件或目录，默认与输入同目录
  --report        把转换诊断写成 JSON 报告
  --category      模块 #!category，默认「插件转换」
  --no-banner     不写转换说明注释
  --check         只检查，不落盘

validate 选项：
  --dir           模块目录，默认 surge
  --external      外部模块清单，默认 external/ios151/manifest.json

audit 选项：
  --dir           模块目录，默认 surge
  --out           报告路径，默认 surge/script-audit.json
  --jobs          并发数，默认 8
  --timeout       单个脚本超时秒数，默认 15
  --strict        有脚本抓不到就返回非零

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
  loon2surge convert loon/知乎去广告.lpx -o surge/
  loon2surge convert loon -o surge --report build/report.json
  loon2surge validate --dir surge
  loon2surge sync --root . --jobs 4
`

// Run 执行一条子命令，返回进程退出码。
func Run(argv []string) int {
	if len(argv) == 0 {
		fmt.Printf(usage, Version)
		return 2
	}

	switch command := argv[0]; command {
	case "convert":
		return runConvert(argv[1:])
	case "sync":
		return runSync(argv[1:])
	case "validate":
		return runValidate(argv[1:])
	case "audit":
		return runAudit(argv[1:])
	case "external":
		return runExternal(argv[1:])
	case "version", "-v", "--version":
		fmt.Println("loon2surge " + Version)
		return 0
	case "help", "-h", "--help":
		fmt.Printf(usage, Version)
		return 0
	default: // 允许 `loon2surge a.lpx` 直接转换
		return runConvert(argv)
	}
}

// parse 解析可变位置的标志：`convert a.lpx -o out` 与 `convert -o out a.lpx` 等价。
//
// 标准库的 flag 遇到第一个非标志参数就停，所以先按标志是否取值把两者分开再解析。
// 调用前必须已经注册好全部标志，valueFlagNames 要读 FlagSet。
func parse(fs *flag.FlagSet, argv []string) (targets []string, err error) {
	flags, targets := splitArgs(argv, valueFlagNames(fs))
	if err := fs.Parse(flags); err != nil {
		// ErrHelp 也要原样交回：吞成 nil 的话调用方会以为解析通过，接着真的去跑这条命令
		// （`sync -h` 于是打印完帮助就开整轮同步）。exitCode 把它算正常退出。
		return nil, err
	}
	return targets, nil
}

// exitCode 把解析错误翻译成退出码：帮助信息算正常退出，其余算用法错误。
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		return 2
	}
}
