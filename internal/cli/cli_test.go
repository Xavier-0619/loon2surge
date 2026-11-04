package cli

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// 帮助只能打印帮助。子命令里 sync / external / audit 都会发请求或写文件，
// 解析函数把 ErrHelp 吞成 nil 的话，`sync -h` 打印完就真跑一轮同步。
func TestParseKeepsErrHelp(t *testing.T) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.String("root", ".", "仓库根目录")
	fs.SetOutput(io.Discard)

	if _, err := parse(fs, []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("期望 ErrHelp，得到 %v", err)
	}

	// 目录里放一份缺 #!name 的模块：真去校验就会返回 1，用它确认 -h 没有接着跑。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.sgmodule"), []byte("[Rule]\nDOMAIN, a.com, REJECT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"validate", "-h", "--dir", dir}); code != 0 {
		t.Errorf("-h 不该执行这条命令，得到退出码 %d", code)
	}
}

// 退出码要把「看了帮助」和「参数写错」分开。
func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{flag.ErrHelp, 0},
		{errors.New("bad flag"), 2},
	}
	for _, test := range cases {
		if got := exitCode(test.err); got != test.want {
			t.Errorf("exitCode(%v) = %d，期望 %d", test.err, got, test.want)
		}
	}
}

// 位置参数与标志混写时，标志在前在后都要认。
func TestSplitArgsMixesFlagsAndTargets(t *testing.T) {
	flags, positional := splitArgs([]string{"a.lpx", "--category", "可莉", "-o", "out", "b.lpx"},
		[]string{"category", "o"})
	if len(positional) != 2 || positional[0] != "a.lpx" || positional[1] != "b.lpx" {
		t.Errorf("位置参数不符：%v", positional)
	}
	if len(flags) != 4 {
		t.Errorf("标志没配对齐：%v", flags)
	}
	for _, arg := range flags {
		if arg == "a.lpx" || arg == "b.lpx" {
			t.Errorf("位置参数混进了标志：%v", flags)
		}
	}
}

// convert 的 --check 不落盘，用来确认命令确实按标志走分支。
func TestConvertCheckWritesNothing(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "a.lpx")
	if err := os.WriteFile(plugin, []byte("#!name=A\n[Rule]\nDOMAIN, a.com, REJECT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if code := Run([]string{"convert", plugin, "-o", out, "--check"}); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(filepath.Join(out, "A.sgmodule")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--check 不该落盘，得到 %v", err)
	}
}
