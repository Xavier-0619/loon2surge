package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	changed, err := WriteString(path, "一")
	if err != nil || !changed {
		t.Fatalf("首次写入应落盘，得到 changed=%v err=%v", changed, err)
	}
	if changed, err := WriteString(path, "一"); err != nil || changed {
		t.Errorf("内容没变不该再写，得到 changed=%v err=%v", changed, err)
	}
	if changed, err := WriteString(path, "二"); err != nil || !changed {
		t.Errorf("内容变了要重写，得到 changed=%v err=%v", changed, err)
	}
}

// 临时文件必须唯一且用完就没了：固定的 `<path>.tmp` 在两轮进程重叠时会互相踩。
func TestWriteFileLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	if _, err := WriteString(path, "一"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("权限应为 0644，得到 %O", perm)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := WriteString(path, strings.Repeat("x", 1<<14)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.txt" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("目录里只剩 a.txt，实际有 %v", names)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if Exists(path) {
		t.Error("不存在的文件不该报存在")
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Exists(path) {
		t.Error("普通文件应报存在")
	}
	if Exists(dir) {
		t.Error("目录不算存在这个文件")
	}
}
