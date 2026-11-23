package main

import (
	"fmt"
	"strings"

	"go.mitchellh.com/libghostty"
)

// probeFormatterHistory 钉死 U0-2 的关键发现：formatter 的 VT 输出到底包不包含滚动
// 历史、包含多少、顺序如何。头文件说它只针对 active screen，实测克隆终端却出现了
// 12 行 scrollback（原终端 13 行）。用带编号的历史行做逐行对齐，看清它到底输出什么。
func probeFormatterHistory() error {
	// 原终端：80×24，喂 40 行带编号的行（每行短、不含软换行），末尾光标定位到 (3,10)
	// 写一个标记。40 行 > 24 行，必然产生 16 行滚动历史。
	orig, err := newTerminal()
	if err != nil {
		return err
	}
	defer orig.Close()

	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "\x1b[%dmL%02d\x1b[0m 行尾\r\n", 31+(i-1)%7, i)
	}
	b.WriteString("\x1b[11;3H\x1b[93m光标标记\x1b[0m")
	orig.VTWrite([]byte(b.String()))

	sbO, _ := orig.ScrollbackRows()
	totalO, _ := orig.TotalRows()
	rowsO, _ := orig.Rows()
	fmt.Printf("原终端: scrollback=%d total=%d rows=%d\n", sbO, totalO, rowsO)

	// 全量 extras 格式化。
	f, err := libghostty.NewFormatter(orig,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
		libghostty.WithFormatterExtras(libghostty.FormatterExtraCursor|libghostty.FormatterExtraStyle|libghostty.FormatterExtraPalette),
	)
	if err != nil {
		return err
	}
	out, err := f.Format()
	if err != nil {
		return err
	}
	f.Close()

	// 输出里有多少个换行，大致说明它写了几行。
	lf := strings.Count(string(out), "\n")
	cr := strings.Count(string(out), "\r")
	fmt.Printf("VT 输出: %d 字节，含 \\n×%d、\\r×%d\n", len(out), lf, cr)
	fmt.Printf("VT 开头: %s\n", escapeVT(out[:min(160, len(out))], 160))

	// 喂入克隆终端，看它的 scrollback / total / 各行内容。
	clone, err := newTerminal()
	if err != nil {
		return err
	}
	defer clone.Close()
	feed(clone, out)

	sbC, _ := clone.ScrollbackRows()
	totalC, _ := clone.TotalRows()
	fmt.Printf("克隆终端: scrollback=%d total=%d\n", sbC, totalC)

	// 逐行对比：两台终端的 total 行（屏幕坐标系，Y=0 是最老的历史行）。
	fmt.Println("\n逐行对比（屏幕坐标系；只列不一致的行）：")
	bad := 0
	var lastGoodY int = -1
	for y := 0; y < int(totalO) && y < 60; y++ {
		ta, tb := rowText(orig, y), rowText(clone, y)
		if ta != tb {
			bad++
			fmt.Printf("  y=%-2d 原=%q 克=%q\n", y, ta, tb)
		} else {
			lastGoodY = y
		}
	}
	fmt.Printf("  ……不一致行数=%d；最后一致的行 y=%d\n", bad, lastGoodY)

	// 头尾几行给人看：确认顺序（Y=0 是最老的 L01 还是最新行）。
	fmt.Println("\n原终端屏幕坐标头尾各 3 行：")
	for _, y := range []int{0, 1, 2, int(totalO) - 3, int(totalO) - 2, int(totalO) - 1} {
		fmt.Printf("  y=%-2d %q\n", y, rowText(orig, y))
	}
	fmt.Println("克隆终端屏幕坐标头尾各 3 行：")
	for _, y := range []int{0, 1, 2, int(totalC) - 3, int(totalC) - 2, int(totalC) - 1} {
		if y < 0 {
			continue
		}
		fmt.Printf("  y=%-2d %q\n", y, rowText(clone, y))
	}

	// 光标。
	xO, _ := orig.CursorX()
	yO, _ := orig.CursorY()
	xC, _ := clone.CursorX()
	yC, _ := clone.CursorY()
	fmt.Printf("\n光标: 原=(%d,%d) 克=(%d,%d)\n", xO, yO, xC, yC)
	return nil
}

// rowText 读屏幕坐标系（含历史）第 y 行的纯文本：逐格取 grapheme/codepoint，
// 跳过宽字符尾占位，行尾空白裁掉。
func rowText(t *libghostty.Terminal, y int) string {
	var sb strings.Builder
	lastNonBlank := -1
	var cells []string
	for x := 0; x < 80; x++ {
		p := libghostty.Point{Tag: libghostty.PointTagScreen, X: uint16(x), Y: uint32(y)}
		g, err := t.GridRef(p)
		if err != nil {
			cells = append(cells, "?")
			continue
		}
		cell, err := g.Cell()
		if err != nil {
			cells = append(cells, "?")
			continue
		}
		w, _ := cell.Wide()
		var s string
		if has, _ := cell.HasText(); has {
			tag, _ := cell.ContentTag()
			if tag == libghostty.CellContentCodepointGrapheme {
				gs, _ := g.Graphemes()
				for _, r := range gs {
					s += string(rune(r))
				}
			} else {
				cp, _ := cell.Codepoint()
				s = string(rune(cp))
			}
		} else {
			s = " "
		}
		if w == libghostty.CellWideSpacerTail {
			s = ""
		}
		cells = append(cells, s)
		if strings.TrimSpace(s) != "" {
			lastNonBlank = x
		}
	}
	for x := 0; x <= lastNonBlank; x++ {
		sb.WriteString(cells[x])
	}
	return sb.String()
}
