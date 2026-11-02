// loon2surge 把 Loon 插件（.lpx）转换成 Surge 模块（.sgmodule）。
//
// 命令实现在 internal/cli；这里只把进程参数交出去。
package main

import (
	"os"

	"loon2surge/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
