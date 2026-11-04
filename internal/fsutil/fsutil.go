// Package fsutil 统一仓库里的落盘方式。
//
// 两条规矩贯穿全仓：内容没变就不碰文件（否则每轮同步都会产生只改时间戳的提交），
// 要写就写临时文件再改名（中途失败不会留下半份索引）。
package fsutil

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// WriteFile 按需写入 data。返回是否真的动了文件。
func WriteFile(path string, data []byte) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	// 临时名必须唯一：两轮进程重叠（比如手动跑了一次 sync，CI 的定时任务也在写）
	// 时，固定的 path+".tmp" 会被两边互相覆盖，rename 之后内容和文件名就对不上。
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tempName := temp.Name()
	if err := writeAndClose(temp, data); err != nil {
		_ = os.Remove(tempName)
		return false, err
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return false, err
	}
	return true, nil
}

// writeAndClose 写完就关，顺手把权限补回 0644（CreateTemp 给的是 0600）。
func writeAndClose(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// WriteString 是 WriteFile 的字符串版本。
func WriteString(path, text string) (bool, error) { return WriteFile(path, []byte(text)) }

// WriteJSON 以两空格缩进落盘 JSON，不转义 & < >（模块名里常见，转义后不好读）。
func WriteJSON(path string, value any) (bool, error) {
	data, err := MarshalJSON(value)
	if err != nil {
		return false, err
	}
	return WriteFile(path, data)
}

// MarshalJSON 是 WriteJSON 的内存版，供需要自己决定路径的调用方使用。
func MarshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Exists 报告 path 是否是一个已存在的普通文件。
func Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
