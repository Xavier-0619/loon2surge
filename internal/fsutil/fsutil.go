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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return false, err
	}
	return true, nil
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
