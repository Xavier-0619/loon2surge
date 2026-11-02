package cli

import (
	"flag"
	"strings"
)

// splitArgs 按「这个标志要不要取值」把参数分成（标志列表, 位置参数列表）。
//
// 标准库的 flag 遇到第一个非标志参数就停，所以要先把两者分开再交给它解析。
// 取值标志来自 valueFlagNames —— 从 FlagSet 推断，不用在这里再手写一份名单。
func splitArgs(argv []string, valueFlags []string) (flags, positional []string) {
	needsValue := make(map[string]bool, len(valueFlags))
	for _, name := range valueFlags {
		needsValue[name] = true
	}

	for index := 0; index < len(argv); index++ {
		arg := argv[index]

		switch {
		case arg == "--":
			return flags, append(positional, argv[index+1:]...)
		case len(arg) < 2 || arg[0] != '-' || arg == "-":
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			continue
		}
		if needsValue[name] && index+1 < len(argv) {
			index++
			flags = append(flags, argv[index])
		}
	}
	return flags, positional
}

// valueFlagNames 列出所有需要取值的标志名。
//
// 布尔标志实现 IsBoolFlag，形如 `--dry-run`，不吞下一个参数；其余（字符串、整数）
// 都要紧跟一个值。从注册表推断而不是另外维护名单，新加标志时不会漏。
func valueFlagNames(fs *flag.FlagSet) []string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			return
		}
		names = append(names, f.Name)
	})
	return names
}
