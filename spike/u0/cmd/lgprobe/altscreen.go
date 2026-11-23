package main

import (
	"fmt"
	"strings"

	"go.mitchellh.com/libghostty"
)

// probeAltScreen 对应 U0 第 6 项：全屏程序（备用屏激活）运行中，能否读出被盖住的
// 主屏内容与光标——重建时还原主屏的前提（退出全屏后画面正确的关键）。
func probeAltScreen() error {
	failed := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  [通过] %s\n", name)
			return
		}
		failed++
		fmt.Printf("  [失败] %s —— %s\n", name, detail)
	}

	t, err := newTerminal(libghostty.WithContinuationMaxBytes(1 << 20))
	if err != nil {
		return err
	}
	defer t.Close()

	// 主屏阶段：6 行带样式的内容 + 滚动历史。
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "\x1b[%dm主屏行 %02d\x1b[0m\r\n", 32+(i-1)%4, i)
	}
	b.WriteString("提示符> ") // 光标停在这行末尾
	t.VTWrite([]byte(b.String()))

	priRows, _ := t.ScrollbackRows()
	fmt.Printf("  进入备用屏前：scrollback=%d\n", priRows)

	// 进入备用屏（CSI ?1049h = 保存光标 + 清屏 + 备用屏），画 vim 风格内容，光标停在 (5,3)。
	t.VTWrite([]byte("\x1b[?1049h\x1b[H\x1b[2J"))
	t.VTWrite([]byte("\x1b[7m~ vim 界面\x1b[0m\r\n~ 第 2 行\r\n~ 第 3 行\r\n"))
	t.VTWrite([]byte("\x1b[6;5H\x1b[5mINSERT 模式标记\x1b[0m"))

	screen, _ := t.ActiveScreen()
	x, _ := t.CursorX()
	y, _ := t.CursorY()
	fmt.Printf("  备用屏中：active=%v(1=alt) 光标=(%d,%d)\n", screen, x, y)

	// 关键问题 1：formatter 读到的是哪块屏？
	f, err := libghostty.NewFormatter(t,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain),
		libghostty.WithFormatterTrim(true),
	)
	if err != nil {
		return err
	}
	act, _ := f.FormatString()
	f.Close()
	check("formatter 覆盖备用屏（活动屏）", strings.Contains(act, "vim 界面"), fmt.Sprintf("内容 %q", act[:min(60, len(act))]))

	// 关键问题 2：GridRef 用 SCREEN/HISTORY 坐标系读到的是主屏还是备用屏？
	// 主屏阶段有 10 行内容（部分滚动进历史），逐屏扫读寻找「主屏行 06」。
	foundPrimary := false
	altLeak := false
	total, _ := t.TotalRows()
	for tag, name := range map[libghostty.PointTag]string{
		libghostty.PointTagScreen:  "SCREEN",
		libghostty.PointTagHistory: "HISTORY",
	} {
		hits := 0
		for yy := 0; yy < int(total)+30; yy++ {
			p := libghostty.Point{Tag: tag, X: 0, Y: uint32(yy)}
			g, err := t.GridRef(p)
			if err != nil {
				break
			}
			cell, err := g.Cell()
			if err != nil {
				break
			}
			cp, _ := cell.Codepoint()
			if cp == 0 {
				if name == "HISTORY" && yy > 0 && hits == 0 {
					continue
				}
				continue
			}
			// 取整行拼串
			var sb strings.Builder
			for xx := 0; xx < 80; xx++ {
				pp := libghostty.Point{Tag: tag, X: uint16(xx), Y: uint32(yy)}
				gg, err := t.GridRef(pp)
				if err != nil {
					break
				}
				cc, err := gg.Cell()
				if err != nil {
					break
				}
				if h, _ := cc.HasText(); h {
					cp2, _ := cc.Codepoint()
					sb.WriteRune(rune(cp2))
				} else {
					sb.WriteByte(' ')
				}
			}
			line := strings.TrimRight(sb.String(), " ")
			if strings.Contains(line, "主屏行") {
				hits++
				if hits == 1 {
					fmt.Printf("  %s 坐标系发现主屏内容: y=%d %q\n", name, yy, line)
				}
			}
			if strings.Contains(line, "vim 界面") {
				altLeak = true
			}
		}
		if name == "SCREEN" {
			foundPrimary = hits > 0
		}
	}
	check("备用屏激活时，网格仍可读到主屏内容（SCREEN 坐标系）", foundPrimary, "没找到主屏行")

	// 关键问题 3：主屏的光标位置可否推知？库里 CursorX/Y 是活动屏光标；
	// 主屏保存光标没有独立 API——验证一下主屏内容行是否包含提示符行，供启发式还原。
	t.VTWrite([]byte("\x1b[?1049l")) // 退出备用屏
	back, _ := t.ActiveScreen()
	x2, _ := t.CursorX()
	fmt.Printf("  退出备用屏：active=%v 光标 x=%d（恢复到「提示符>」后应 x=%d）\n", back, x2, x2)
	plain := plainOf(t)
	check("退出后主屏原样还原（库自身行为）", strings.Contains(plain, "提示符>") && strings.Contains(plain, "主屏行 10"), fmt.Sprintf("%q", plain[:min(80, len(plain))]))
	_ = altLeak

	if failed > 0 {
		return fmt.Errorf("%d 项失败", failed)
	}
	return nil
}
