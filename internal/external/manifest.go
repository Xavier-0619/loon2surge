package external

// 清单的读写与快照清理：清单是"上游每个文件后来怎么了"的唯一账本。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"loon2surge/internal/fsutil"
)

// ReadManifest 读清单。解析失败会把错误交回调用方，由它决定告警还是中止。
func ReadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%s 不是合法清单：%w", path, err)
	}
	return manifest, nil
}

// readManifest 是容错版：读不了就按"首次同步"处理，不让一份坏清单卡住整轮。
func readManifest(path string) Manifest {
	manifest, err := ReadManifest(path)
	if err == nil {
		return manifest
	}
	if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "! %s 读不了，按首次同步处理：%v\n", path, err)
	}
	return Manifest{}
}

func writeManifest(path string, manifest Manifest) error {
	_, err := fsutil.WriteJSON(path, manifest)
	return err
}

// removeIfUnchanged 只在文件还是我们当初写进去的那份时才删，人工改过的一律不动。
func removeIfUnchanged(path, expected string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if stamp(string(data)) != expected {
		fmt.Fprintf(os.Stderr, "! %s 被人工改过，不删除\n", path)
		return
	}
	_ = os.Remove(path)
}

// pruneSnapshot 删掉上游已经下线的快照文件，目录跟着上游走。
func pruneSnapshot(dir string, current, previous Manifest) {
	keep := make(map[string]bool, len(current.Entries))
	for _, entry := range current.Entries {
		// 标记为 retired 的条目留着是为了说明"这个模块怎么没的"，文件本身已经删了。
		if entry.Snapshot == "" || entry.Status == statusRetired {
			continue
		}
		keep[entry.Snapshot] = true
	}
	for _, entry := range previous.Entries {
		if entry.Snapshot == "" || keep[entry.Snapshot] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(entry.Snapshot)))
	}
}
