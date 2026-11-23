package main

import (
	"fmt"
	"strings"

	"go.mitchellh.com/libghostty"
)

// probeShadow 验证「影子主屏」方案：备用屏激活期间主屏不可读（altscreen 探针的结论），
// 那么重建时用一台从不进入备用屏的影子网格承载主屏画面——把影子 formatter 的输出、
// 进入备用屏的切换、真实网格的活动屏输出按序喂入一台全新终端，再喂退出切换，
// 终端应呈现与真实主屏一致的画面（含光标落点）。
func probeShadow() error {
	failed := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  [通过] %s\n", name)
			return
		}
		failed++
		fmt.Printf("  [失败] %s —— %s\n", name, detail)
	}

	// 真实网格：主屏 10 行（1 行滚动出屏）+ 提示符，随后进入备用屏画 vim。
	real, err := newTerminal()
	if err != nil {
		return err
	}
	defer real.Close()
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "\x1b[%dm主屏行 %02d\x1b[0m\r\n", 32+(i-1)%4, i)
	}
	b.WriteString("提示符> ")
	real.VTWrite([]byte(b.String()))
	real.VTWrite([]byte("\x1b[?1049h\x1b[2J\x1b[H"))
	real.VTWrite([]byte("\x1b[7m~ vim\x1b[0m\r\n~ 行 2\r\n"))
	real.VTWrite([]byte("\x1b[6;3H\x1b[5mI\x1b[0m"))

	// 影子网格：只吃主屏阶段的同一份字节（切到备用屏之后的字节一律不给它）。
	shadow, err := newTerminal()
	if err != nil {
		return err
	}
	defer shadow.Close()
	shadow.VTWrite([]byte(b.String()))

	// 各自格式化：影子=主屏画面；真实=备用屏画面。
	fmtStr := func(t *libghostty.Terminal) string {
		f, err := libghostty.NewFormatter(t,
			libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
			libghostty.WithFormatterExtras(libghostty.FormatterExtrasAll),
		)
		if err != nil {
			panic(err)
		}
		out, err := f.Format()
		if err != nil {
			panic(err)
		}
		f.Close()
		return string(out)
	}

	// 重放序列：主屏画面 → 进入备用屏 → 备用屏画面 → （退出由后续实时流触发）。
	replay := fmtStr(shadow) + "\x1b[?1049h" + fmtStr(real)

	// 全新终端吃下重放序列，然后喂退出切换，看它还原出的主屏。
	browser, err := newTerminal()
	if err != nil {
		return err
	}
	defer browser.Close()
	feed(browser, []byte(replay))
	inAlt, _ := browser.ActiveScreen()
	check("重放后浏览器进入备用屏", inAlt == libghostty.ScreenAlternate, fmt.Sprintf("active=%v", inAlt))
	check("备用屏内容与真实网格一致", plainOf(browser) == plainOf(real), "备用屏画面不一致")

	browser.VTWrite([]byte("\x1b[?1049l"))
	check("退出备用屏后，主屏与真实网格一致（影子重建成功）", plainOf(browser) == plainOf(shadow),
		fmt.Sprintf("浏览器=%q 影子=%q", plainOf(browser)[:min(60, len(plainOf(browser)))], plainOf(shadow)[:min(60, len(plainOf(shadow)))]))
	xB, _ := browser.CursorX()
	yB, _ := browser.CursorY()
	xR, _ := real.CursorX()
	check("退出后的光标落在提示符后（接近真实保存光标）", int(xB) >= 7, fmt.Sprintf("浏览器光标=(%d,%d)", xB, yB))
	_ = xR

	if failed > 0 {
		return fmt.Errorf("%d 项失败", failed)
	}
	return nil
}
