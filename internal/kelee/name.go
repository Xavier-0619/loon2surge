package kelee

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 落盘文件名一律取插件里的 #!name（`#!name=前程无忧去广告` → `前程无忧去广告.lpx`）。
//
// 直接拿 #!name 当文件名要挡四件事：
//   - 路径分隔符和 Windows 保留字符，否则 `a/b` 会被当成子目录；
//   - 首尾的空白和句点，Windows 会默默吞掉，写进去和读出来不是一个名字；
//   - 255 字节的长度上限，中文一个字占三字节；
//   - 同一批里出现同名（可莉那边就有两个"挖财记账去广告"），否则两个插件写进同一个文件。
//
// 前三条在这里处理，第四条交给 uniqueStem。

// maxStemBytes 给 ".sgmodule"（9 字节）留足余量，远低于文件系统的 255 字节上限。
const maxStemBytes = 200

// windowsReserved 是 Windows 上不能做文件名的设备名，不分大小写，也不管扩展名。
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

var (
	illegalStemChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f\x7f]`)
	stemSpaceRun     = regexp.MustCompile(`\s+`)
)

// FileStem 是 fileStem 的导出形态：外部仓库并入 surge/ 时也要用同一套命名规则，
// 免得同一个 #!name 在两条流水线里落成两个不同的文件名。
func FileStem(name, fallback string) string { return fileStem(name, fallback) }

// fileStem 把 #!name 变成文件名主干；name 不可用时退回 fallback（从插件地址取的名字）。
func fileStem(name, fallback string) string {
	cleaned := stemSpaceRun.ReplaceAllString(strings.TrimSpace(name), " ")
	cleaned = illegalStemChars.ReplaceAllString(cleaned, "-")
	cleaned = truncateBytes(strings.Trim(cleaned, " ."), maxStemBytes)
	cleaned = strings.Trim(cleaned, " .")
	if strings.Trim(cleaned, "-") == "" { // 全是分隔符，等于没名字
		cleaned = fallback
	}
	if windowsReserved[strings.ToUpper(cleaned)] {
		cleaned = "_" + cleaned
	}
	return cleaned
}

// truncateBytes 按字节截断，但不切碎一个多字节字符。
func truncateBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
