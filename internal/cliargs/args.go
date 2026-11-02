// Package cliargs 让命令行标志可以出现在路径后面。
//
// Go 标准库的 flag 遇到第一个非标志参数就停止解析，于是
// `loon2surge convert a.lpx -o out` 会把 -o 当成输入路径。
// 这里先把标志和位置参数分开，再交给 flag 解析。
package cliargs

import "strings"

// Split 返回（标志列表, 位置参数列表）。
// valueFlags 里登记的是"需要带一个取值"的标志名，例如 o、output、report。
func Split(argv []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]

		if arg == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue // --output=xx 自带取值
		}
		if valueFlags[name] && i+1 < len(argv) {
			i++
			flags = append(flags, argv[i])
		}
	}
	return flags, positional
}
