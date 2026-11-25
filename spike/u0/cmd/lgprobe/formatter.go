package main

import (
	"fmt"
	"strings"

	"go.mitchellh.com/libghostty"
)

// runFormatter 对应 U0 第 2 项：官方 formatter 的 VT 输出能否在克隆终端里原样重建
// 活动屏——文本、每格样式、超链接、光标位置——以及各 extras 分别还原什么。
// 顺带确认 formatter 不包含滚动历史（spec 已按此分工设计）。
func runFormatter() error {
	failed := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  [通过] %s\n", name)
			return
		}
		failed++
		fmt.Printf("  [失败] %s —— %s\n", name, detail)
	}

	// buildOriginal 构造一个样式大杂烩的终端：8/256/RGB 前景与背景、粗体/斜体/
	// 花下划线/反显/删除线、超链接、中文宽字符、emoji，光标停在一个明确的非常规位置。
	buildOriginal := func() *libghostty.Terminal {
		t, err := newTerminal()
		if err != nil {
			panic(err)
		}
		var b strings.Builder
		b.WriteString("\x1b[31m红字\x1b[32m绿字\x1b[0m 默认\r\n")
		b.WriteString("\x1b[1;3;4:3m粗斜花下划线\x1b[22;23;24m 已还原\r\n")
		b.WriteString("\x1b[38;5;46m256 色\x1b[48;5;17m底 256\x1b[0m\r\n")
		b.WriteString("\x1b[38;2;255;128;0mRGB 前景\x1b[0m ")
		b.WriteString("\x1b[48;2;20;60;130mRGB 背景\x1b[0m\r\n")
		b.WriteString("\x1b[7m反显\x1b[27m \x1b[9m删除线\x1b[29m\r\n")
		b.WriteString("\x1b]8;;https://example.com\x1b\\链接\x1b]8;;\x1b\\ 与 ")
		b.WriteString("宽字符 中國🚚 结束\r\n")
		b.WriteString(strings.Repeat("填充行\r\n", 30))  // 制造滚动历史
		b.WriteString("\x1b[5;10H\x1b[93m游标行\x1b[0m") // 光标停在 (10,4)，前面有彩色字
		t.VTWrite([]byte(b.String()))
		return t
	}

	// clone 用指定 extras 把 orig 的活动屏格式化为 VT 并喂入全新的终端。
	clone := func(orig *libghostty.Terminal, extras libghostty.FormatterExtras) *libghostty.Terminal {
		f, err := libghostty.NewFormatter(orig,
			libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
			libghostty.WithFormatterExtras(extras),
		)
		if err != nil {
			panic(err)
		}
		out, err := f.Format()
		if err != nil {
			panic(err)
		}
		f.Close()
		c, err := newTerminal()
		if err != nil {
			panic(err)
		}
		feed(c, out)
		lastVT = out
		return c
	}

	fmt.Println("1. formatter 只输出活动屏（不含滚动历史）")
	orig := buildOriginal()
	defer orig.Close()
	{
		sb, _ := orig.ScrollbackRows()
		total, _ := orig.TotalRows()
		fmt.Printf("     原终端：scrollback=%d 行，total=%d 行\n", sb, total)
		c := clone(orig, libghostty.FormatterExtrasNone)
		defer c.Close()
		sb2, _ := c.ScrollbackRows()
		check("克隆终端的 scrollback 为空（滚动历史归自建序列化器）", sb2 == 0, fmt.Sprintf("scrollback=%d", sb2))
	}

	fmt.Println("2. extras=None：文本与每格样式逐格一致")
	{
		c := clone(orig, libghostty.FormatterExtrasNone)
		defer c.Close()
		rows, _ := orig.Rows()
		cols, _ := orig.Cols()
		badText, badStyle := compareGrid(orig, c, libghostty.PointTagActive, int(rows), int(cols))
		check("全部行的文本一致", badText == 0, fmt.Sprintf("%d 行文本不一致（首例见下）", badText))
		check("全部行的样式一致（含颜色/粗斜体/反显/下划线样式）", badStyle == 0,
			fmt.Sprintf("%d 行样式不一致；首个差异: %s", badStyle, firstDiff))
		fmt.Printf("     VT 字节（节选转义显示）: %s\n", escapeVT(lastVT, 220))
	}

	fmt.Println("3. extras=Cursor：光标位置还原")
	{
		noExtra := clone(orig, libghostty.FormatterExtrasNone)
		defer noExtra.Close()
		withExtra := clone(orig, libghostty.FormatterExtraCursor|libghostty.FormatterExtraStyle)
		defer withExtra.Close()
		x0, _ := orig.CursorX()
		y0, _ := orig.CursorY()
		xn, _ := noExtra.CursorX()
		yn, _ := noExtra.CursorY()
		xc, _ := withExtra.CursorX()
		yc, _ := withExtra.CursorY()
		check("原终端光标在写入位置", x0 == 10 && y0 == 4, fmt.Sprintf("(%d,%d)", x0, y0))
		check("无 Cursor extra 时光标不在原位", xn != x0 || yn != y0, fmt.Sprintf("(%d,%d)", xn, yn))
		check("有 Cursor extra 时光标复原", xc == x0 && yc == y0, fmt.Sprintf("(%d,%d)", xc, yc))
	}

	fmt.Println("4. 其他 extras：调色板、模式、滚动区域、制表位、PWD")
	{
		// 设置一批「非默认」状态，再用全量 extras 重建。
		orig.SetPwd("/workspace/limpet")
		orig.VTWrite([]byte("\x1b[3;40r\x1b[?5h")) // 滚动区域 3..40（上边小于下边即可生效）+ DECCOLM? no, ?5 是反显模式
		_ = orig.VTWrite
		c := clone(orig, libghostty.FormatterExtrasAll)
		defer c.Close()
		pwdO, _ := orig.Pwd()
		pwdC, _ := c.Pwd()
		check("PWD 经 OSC 7 还原", pwdC == pwdO && pwdO != "", fmt.Sprintf("orig=%q clone=%q", pwdO, pwdC))
		palO, _ := orig.ColorPalette()
		palC, _ := c.ColorPalette()
		check("调色板经 OSC 4 还原", paletteSig(palO) == paletteSig(palC), "调色板不一致")
		check("重建后仍在 ground、可继续吃实时流", mustGround(c), "不在 ground")
	}

	fmt.Println("5. 样式覆盖抽查（人读格式）")
	{
		c := clone(orig, libghostty.FormatterExtrasNone)
		defer c.Close()
		for _, at := range [][2]int{{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 14}, {5, 20}} {
			gO := gridCell(orig, at[0], at[1])
			gC := gridCell(c, at[0], at[1])
			fmt.Printf("     (%2d,%2d) 原=%s\n", at[1], at[0], gO)
			fmt.Printf("            克=%s\n", gC)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d 项失败", failed)
	}
	return nil
}

// ---- 供各探针共用的网格读取与对比工具 ----

var lastVT []byte
var firstDiff string

func mustGround(t *libghostty.Terminal) bool {
	g, err := t.VTGround()
	if err != nil {
		panic(err)
	}
	return g
}

// gridCell 读取活动屏一格的可读摘要：文本（含 grapheme）、宽字符标志、样式、超链接。
func gridCell(t *libghostty.Terminal, x int, y int) string {
	p := libghostty.Point{Tag: libghostty.PointTagActive, X: uint16(x), Y: uint32(y)}
	g, err := t.GridRef(p)
	if err != nil {
		return fmt.Sprintf("<err %v>", err)
	}
	cell, err := g.Cell()
	if err != nil {
		return fmt.Sprintf("<err %v>", err)
	}
	var sb strings.Builder
	if has, _ := cell.HasText(); has {
		tag, _ := cell.ContentTag()
		if tag == libghostty.CellContentCodepointGrapheme {
			gs, _ := g.Graphemes()
			for _, r := range gs {
				sb.WriteRune(rune(r))
			}
		} else {
			cp, _ := cell.Codepoint()
			sb.WriteRune(rune(cp))
		}
	} else {
		sb.WriteString("·")
	}
	w, _ := cell.Wide()
	switch w {
	case libghostty.CellWideWide:
		sb.WriteString("/宽")
	case libghostty.CellWideSpacerTail:
		sb.WriteString("/尾占位")
	}
	st, err := g.Style()
	if err == nil && st != nil && !st.IsDefault() {
		sb.WriteString(" style=" + styleSig(st))
	}
	if uri, err := g.HyperlinkURI(); err == nil && uri != "" {
		sb.WriteString(" link=" + uri)
	}
	return sb.String()
}

// styleSig 把样式拼成可比较、人可读的字符串。
func styleSig(s *libghostty.Style) string {
	var parts []string
	if s.Bold() {
		parts = append(parts, "B")
	}
	if s.Italic() {
		parts = append(parts, "I")
	}
	if s.Faint() {
		parts = append(parts, "F")
	}
	if s.Blink() {
		parts = append(parts, "K")
	}
	if s.Inverse() {
		parts = append(parts, "V")
	}
	if s.Invisible() {
		parts = append(parts, "H")
	}
	if s.Strikethrough() {
		parts = append(parts, "S")
	}
	if s.Overline() {
		parts = append(parts, "O")
	}
	switch u := s.Underline(); u {
	case libghostty.UnderlineNone:
	case libghostty.UnderlineSingle:
		parts = append(parts, "u1")
	default:
		parts = append(parts, fmt.Sprintf("u%d", u))
	}
	fg := colorSig(s.FgColor(), "fg")
	bg := colorSig(s.BgColor(), "bg")
	ul := colorSig(s.UnderlineColor(), "ul")
	parts = append(parts, fg, bg, ul)
	return "{" + strings.Join(nonEmpty(parts), ",") + "}"
}

func colorSig(c libghostty.StyleColor, name string) string {
	switch c.Tag {
	case libghostty.StyleColorNone:
		return ""
	case libghostty.StyleColorPalette:
		return fmt.Sprintf("%s=p%d", name, c.Palette)
	case libghostty.StyleColorRGB:
		return fmt.Sprintf("%s=#%02x%02x%02x", name, c.RGB.R, c.RGB.G, c.RGB.B)
	default:
		return fmt.Sprintf("%s=?", name)
	}
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// compareGrid 对比两台终端同一坐标系下 rows×cols 的文本与样式，返回不一致的行数。
// firstDiff 记录首个差异供人查看。
func compareGrid(a, b *libghostty.Terminal, tag libghostty.PointTag, rows, cols int) (badText, badStyle int) {
	firstDiff = ""
	for y := 0; y < rows; y++ {
		textSame, styleSame := true, true
		var diffDetail string
		for x := 0; x < cols; x++ {
			ca, cb := gridCell(a, x, y), gridCell(b, x, y)
			if ca != cb {
				// 文本与样式分开判：串里 style= 之前是文本部分
				ta, sa := splitSig(ca)
				tb, sb2 := splitSig(cb)
				if ta != tb {
					textSame = false
				}
				if sa != sb2 {
					styleSame = false
				}
				if firstDiff == "" {
					diffDetail = fmt.Sprintf("(%d,%d) 原=%q 克=%q", x, y, ca, cb)
				}
			}
		}
		if !textSame {
			badText++
			if firstDiff == "" {
				firstDiff = fmt.Sprintf("y=%d 文本不一致", y)
			}
		}
		if !styleSame {
			badStyle++
			if firstDiff == "" {
				firstDiff = diffDetail
			}
		}
	}
	return badText, badStyle
}

func splitSig(sig string) (text, style string) {
	if i := strings.Index(sig, " style="); i >= 0 {
		text, style = sig[:i], sig[i:]
		// style 里可能还有 link=，一并归入样式
		return text, style
	}
	return sig, ""
}

func escapeVT(b []byte, max int) string {
	var sb strings.Builder
	for i := 0; i < len(b) && sb.Len() < max; i++ {
		switch c := b[i]; c {
		case '\r':
			sb.WriteString("\\r")
		case '\n':
			sb.WriteString("\\n")
		case '\x1b':
			sb.WriteString("\\e")
		case '\a':
			sb.WriteString("\\a")
		default:
			if c < 0x20 {
				sb.WriteString(fmt.Sprintf("\\x%02x", c))
			} else {
				sb.WriteByte(c)
			}
		}
	}
	if sb.Len() >= max {
		sb.WriteString("…")
	}
	return sb.String()
}

func paletteSig(p *libghostty.Palette) string {
	if p == nil {
		return "<nil>"
	}
	var sb strings.Builder
	for i := 0; i < libghostty.PaletteSize; i++ {
		c := p[i]
		fmt.Fprintf(&sb, "%02x%02x%02x,", c.R, c.G, c.B)
	}
	return sb.String()
}
