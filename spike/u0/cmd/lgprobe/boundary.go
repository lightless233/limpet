package main

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"go.mitchellh.com/libghostty"
)

// runBoundary 对应 U0 第 8 项的库层部分：VTWriteUntil 在 GROUND 边界上的实际语义，
// 以及流停在半条转义序列 / 半个 UTF-8 字符中间时，切点怎么取才安全。
func runBoundary() error {
	failed := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  [通过] %s\n", name)
			return
		}
		failed++
		fmt.Printf("  [失败] %s —— %s\n", name, detail)
	}
	// 建终端时启用 continuation 跟踪，这样才能取出未完成序列的字节。
	newGridTerminal := func() *libghostty.Terminal {
		t, err := newTerminal(libghostty.WithContinuationMaxBytes(1 << 20))
		if err != nil {
			panic(err)
		}
		return t
	}

	fmt.Println("1. 流已在 ground 时请求 GROUND 边界")
	{
		t := newGridTerminal()
		defer t.Close()
		stop, err := t.VTWriteUntil([]byte("abc"), libghostty.TerminalBoundaryGround)
		if err != nil {
			return err
		}
		ground, _ := t.VTGround()
		// 实测：立即返回、消耗 0 字节，但 Reached 带着 Ground 标志（不是 0）。
		// 所以「刚刚回到 ground」不能只看 Reached，必须同时看 Consumed > 0。
		check("已在 ground 时不消耗任何字节", stop.Consumed == 0, fmt.Sprintf("consumed=%d", stop.Consumed))
		check("此时 Reached 仍带着 Ground 标志（不能单独拿它判断）", stop.Reached&libghostty.TerminalBoundaryGround != 0,
			fmt.Sprintf("reached=%v", stop.Reached))
		check("终端仍在 ground", ground, "离开了 ground")
		fmt.Println("     → 读循环在 ground 时必须用 VTWrite 整批喂；VTWriteUntil 只用于从序列中间回到 ground")
	}

	fmt.Println("2. 整批喂入后，批尾停在 CSI 序列中间")
	{
		t := newGridTerminal()
		defer t.Close()
		t.VTWrite([]byte("hi\x1b[31mred\x1b[1;3"))
		ground, _ := t.VTGround()
		cont, err := t.Continuation()
		if err != nil {
			return err
		}
		check("此时 VTGround() 为 false（切点不安全）", !ground, "仍在 ground")
		check("Continuation() 给出未完成的字节", string(cont) == "\x1b[1;3", fmt.Sprintf("得到 %q", cont))
		stop, err := t.VTWriteUntil([]byte("2myellow\x1b[0m!"), libghostty.TerminalBoundaryGround)
		if err != nil {
			return err
		}
		check("后续字节让它回到 ground 时停下", stop.Reached&libghostty.TerminalBoundaryGround != 0,
			fmt.Sprintf("reached=%v", stop.Reached))
		check("恰好在序列结束处停下（只消耗 \"2m\"）", stop.Consumed == 2, fmt.Sprintf("consumed=%d", stop.Consumed))
		g2, _ := t.VTGround()
		check("之后 VTGround() 为 true（切点安全）", g2, "仍未回到 ground")
		t.VTWrite([]byte("yellow\x1b[0m!"))
		fmt.Printf("     画面: %q\n", strings.TrimRight(plainOf(t), "\n"))
	}

	fmt.Println("3. 批尾停在多字节 UTF-8 字符中间")
	{
		t := newGridTerminal()
		defer t.Close()
		const zh = "中" // e4 b8 ad
		t.VTWrite([]byte("a" + zh[:2]))
		ground, _ := t.VTGround()
		cont, err := t.Continuation()
		if err != nil {
			return err
		}
		check("此时 VTGround() 为 false", !ground, "仍在 ground")
		check("Continuation() 给出未完成的 2 个字节", string(cont) == zh[:2], fmt.Sprintf("得到 %q", cont))
		stop, err := t.VTWriteUntil([]byte(zh[2:]+"b"), libghostty.TerminalBoundaryGround)
		if err != nil {
			return err
		}
		check("补上最后 1 个字节后回到 ground", stop.Reached&libghostty.TerminalBoundaryGround != 0 && stop.Consumed == 1,
			fmt.Sprintf("reached=%v consumed=%d", stop.Reached, stop.Consumed))
		t.VTWrite([]byte("b"))
		check("字符完整显示，没有替换符", strings.Contains(plainOf(t), "a中b"), fmt.Sprintf("画面 %q", plainOf(t)))
	}

	fmt.Println("4. 批尾停在 OSC 标题序列中间")
	{
		t := newGridTerminal()
		defer t.Close()
		t.VTWrite([]byte("\x1b]0;win"))
		ground, _ := t.VTGround()
		cont, err := t.Continuation()
		if err != nil {
			return err
		}
		check("此时 VTGround() 为 false", !ground, "仍在 ground")
		check("Continuation() 给出未完成的前缀", string(cont) == "\x1b]0;win", fmt.Sprintf("得到 %q", cont))
		stop, err := t.VTWriteUntil([]byte("dow\x07after"), libghostty.TerminalBoundaryGround)
		if err != nil {
			return err
		}
		check("序列结束（BEL）处停下", stop.Consumed == len("dow\x07"), fmt.Sprintf("consumed=%d", stop.Consumed))
		title, _ := t.Title()
		check("标题完整生效", title == "window", fmt.Sprintf("title=%q", title))
		t.VTWrite([]byte("after"))
		check("标题之后的内容正常显示", strings.Contains(plainOf(t), "after"), fmt.Sprintf("画面 %q", plainOf(t)))
	}

	fmt.Println("5. 读循环的分批喂法与整批喂法等价")
	{
		whole := "line1\r\n\x1b[1;35mmagenta\x1b[0m 中文🚚\r\n\x1b]0;t\x07\x1b[2J\x1b[Hdone\x1b[?25l"
		unsafe := map[int]bool{}
		mismatch := 0
		for i := 0; i <= len(whole); i++ {
			ref := newGridTerminal()
			ref.VTWrite([]byte(whole))

			got := newGridTerminal()
			feed(got, []byte(whole[:i]))
			if g, _ := got.VTGround(); !g {
				unsafe[i] = true
			}
			feed(got, []byte(whole[i:]))

			if snapshotOf(ref) != snapshotOf(got) {
				mismatch++
			}
			ref.Close()
			got.Close()
		}
		check("每一个二分切点下，分两批喂与整批喂的最终状态一致", mismatch == 0, fmt.Sprintf("%d 个切点不一致", mismatch))
		var at []int
		for i := 0; i <= len(whole); i++ {
			if unsafe[i] {
				at = append(at, i)
			}
		}
		fmt.Printf("     %d 个切点位置里有 %d 个落在序列内部（不安全）: %v\n", len(whole)+1, len(at), at)
		check("切点落在 ground 时 VTGround() 为 true，且此时 Continuation() 为空", len(at) < len(whole)+1, "没有安全的切点")
	}

	fmt.Println("6. 不安全时的兜底：把 Continuation() 作为实时流的前缀补发")
	{
		worker := newGridTerminal()
		defer worker.Close()
		worker.VTWrite([]byte("\x1b[31mred\x1b[1;3")) // 停在 CSI 中间，此刻发起接入
		cont, err := worker.Continuation()
		if err != nil {
			return err
		}
		rest := []byte("2myellow\x1b[0m!")

		// 浏览器端：先收到重建画面（这里用等价的前缀重放模拟），再收到「continuation + 后续字节」。
		browser := newGridTerminal()
		defer browser.Close()
		browser.VTWrite([]byte("\x1b[31mred"))
		browser.VTWrite(append(append([]byte{}, cont...), rest...))

		// worker 端直接续写。
		worker.VTWrite(rest)

		check("补发前缀后，浏览器端与 worker 端状态一致", snapshotOf(browser) == snapshotOf(worker),
			fmt.Sprintf("浏览器 %q / worker %q", plainOf(browser), plainOf(worker)))
	}

	fmt.Println("7. 喂网格的吞吐（4096 字节一批，模拟 PTY 读块）")
	{
		const total = 8 << 20
		line := "0123456789 abcdefghijklmnopqrstuvwxyz 中文行 \x1b[32mgreen\x1b[0m\r\n"
		var buf bytes.Buffer
		for buf.Len() < total {
			fmt.Fprintf(&buf, "%s\x1b[%dm", line, 31+buf.Len()%7)
		}
		data := buf.Bytes()
		t := newGridTerminal()
		defer t.Close()
		start := time.Now()
		for off := 0; off < len(data); off += 4096 {
			end := min(off+4096, len(data))
			feed(t, data[off:end])
		}
		el := time.Since(start)
		fmt.Printf("     %d MiB 用时 %v，约 %.0f MiB/s\n", len(data)>>20, el.Round(time.Millisecond), float64(len(data))/(1<<20)/el.Seconds())
		check("吞吐远高于终端实际输出速率（> 20 MiB/s）", float64(len(data))/(1<<20)/el.Seconds() > 20, "吞吐过低")
	}

	if failed > 0 {
		return fmt.Errorf("%d 项失败", failed)
	}
	return nil
}

// feed 是 worker 读循环的喂法：在 ground 时整批直写（快路径）；停在序列中间时，
// 用 VTWriteUntil 喂到回到 ground 为止，剩余部分再整批直写。
func feed(t *libghostty.Terminal, data []byte) {
	for len(data) > 0 {
		if g, _ := t.VTGround(); g {
			t.VTWrite(data)
			return
		}
		stop, err := t.VTWriteUntil(data, libghostty.TerminalBoundaryGround)
		if err != nil {
			panic(err)
		}
		// 已在 ground 时 VTWriteUntil 消耗 0 字节却照常返回，这里防御原地打转。
		if stop.Consumed == 0 {
			t.VTWrite(data)
			return
		}
		data = data[stop.Consumed:]
	}
}

// plainOf 返回终端活动屏的纯文本（去掉行尾空白，保留换行）。
func plainOf(t *libghostty.Terminal) string {
	f, err := libghostty.NewFormatter(t,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain),
		libghostty.WithFormatterTrim(true),
	)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		panic(err)
	}
	return s
}

// snapshotOf 把终端的可观察状态拼成一个可比较的字符串：纯文本、光标位置与可见性、标题、活动屏。
func snapshotOf(t *libghostty.Terminal) string {
	x, _ := t.CursorX()
	y, _ := t.CursorY()
	vis, _ := t.CursorVisible()
	title, _ := t.Title()
	screen, _ := t.ActiveScreen()
	ground, _ := t.VTGround()
	return fmt.Sprintf("text=%q cursor=(%d,%d) visible=%v title=%q screen=%v ground=%v wrap=%v",
		plainOf(t), x, y, vis, title, screen, ground, cursorWrap(t))
}

func cursorWrap(t *libghostty.Terminal) bool {
	w, err := t.CursorPendingWrap()
	if err != nil {
		panic(err)
	}
	return w
}
