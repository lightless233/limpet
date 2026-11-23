// lgprobe 是 U0 spike 的命令行探针：不碰 PTY，直接往 libghostty 的终端里喂字节，
// 观察 API 的实际行为。每个子命令对应 Plan 003 U0 的一项验证。
package main

import (
	"fmt"
	"os"

	"go.mitchellh.com/libghostty"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: lgprobe <basic|...>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "basic":
		err = runBasic()
	case "boundary":
		err = runBoundary()
	case "formatter":
		err = runFormatter()
	case "fmthistory":
		err = probeFormatterHistory()
	case "altscreen":
		err = probeAltScreen()
	case "shadow":
		err = probeShadow()
	default:
		err = fmt.Errorf("未知子命令 %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "失败:", err)
		os.Exit(1)
	}
}

// newTerminal 建一个 80×24、滚动历史 10000 行的终端，尺寸与 worker 的约定一致。
func newTerminal(opts ...libghostty.TerminalOption) (*libghostty.Terminal, error) {
	opts = append([]libghostty.TerminalOption{
		libghostty.WithSize(80, 24),
		libghostty.WithMaxScrollbackLines(10000),
	}, opts...)
	return libghostty.NewTerminal(opts...)
}

// runBasic 对应 U0 第 1 项：绑定可用——喂字节、解析、无 panic。
func runBasic() error {
	term, err := newTerminal()
	if err != nil {
		return err
	}
	defer term.Close()

	term.VTWrite([]byte("Hello, \033[1;32mworld\033[0m!\r\n"))
	term.VTWrite([]byte("第二行：中文与 emoji 🚚\r\n"))

	x, err := term.CursorX()
	if err != nil {
		return err
	}
	y, err := term.CursorY()
	if err != nil {
		return err
	}
	cols, _ := term.Cols()
	rows, _ := term.Rows()
	screen, _ := term.ActiveScreen()
	ground, _ := term.VTGround()
	style, _ := term.CursorSGRStyle()

	plain, err := libghostty.NewFormatter(term,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain),
	)
	if err != nil {
		return err
	}
	defer plain.Close()
	text, err := plain.FormatString()
	if err != nil {
		return err
	}

	fmt.Printf("尺寸: %d×%d  光标: (%d,%d)  活动屏: %v  解析器在 ground: %v\n", cols, rows, x, y, screen, ground)
	fmt.Printf("光标处样式: %+v\n", style)
	fmt.Printf("纯文本内容（行数 %d）:\n%s", countLines(text), text)
	return nil
}

func countLines(s string) int {
	n := 0
	for range lines(s) {
		n++
	}
	return n
}

func lines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
