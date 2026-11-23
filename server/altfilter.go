package main

import "bytes"

// altFilter 把 PTY 字节流切分为「主屏段 / 备用屏段」：影子网格只消费主屏段
// （spec 003「全屏程序运行中重接」）。真实网格始终吃全量原始字节，不受本过滤器
// 影响——过滤器只为影子的喂入切分服务。
//
// 识别的切换序列：CSI '?'+参数为 1049 / 1047 / 47、终结字节 h 或 l（h 进入、
// l 离开）。7-bit（ESC [）与 8-bit C1（0x9B）起始都支持。OSC / DCS / APC /
// SOS / PM 字符串体内不扫描（避免内容里的假切换），已知局限：字符串体内的
// ESC 转义路径做了简化处理，实际程序不会在字符串里夹带备用屏切换。
//
// 批尾停在序列中间的字节暂存（pending），下一批到达后裁决归属——因此影子与
// 真实网格的切分是字节精确的。
type altFilter struct {
	inAlt bool
	state int
	buf   []byte // 批尾未完结的序列字节（跨批保持，裁决后并入归属段）
}

const (
	stGround = iota
	stEsc
	stCSI
	stStr    // OSC / DCS / APC / SOS / PM 字符串体
	stStrEsc // 字符串体内的 ESC（可能是 ST）
)

// seg 是一段字节及其影子归属。
type seg struct {
	data   []byte
	shadow bool
}

// process 处理一批字节，返回切分结果。switch 序列本身不进入任何影子段
// （影子网格永不切换屏幕）。
func (f *altFilter) process(batch []byte) []seg {
	var out []seg
	cur := []byte{}
	curShadow := !f.inAlt

	flush := func() {
		if len(cur) > 0 {
			out = append(out, seg{data: cur, shadow: curShadow})
			cur = []byte{}
		}
	}

	i := 0
	for i < len(batch) {
		b := batch[i]
		i++
		switch f.state {
		case stGround:
			// ground 态只有 7-bit ESC 起始序列；C1 字节（0x80-0x9F）是 UTF-8
			// 数据——库文档明确「ground 态它们按 UTF-8 解码、不起始字符串」，
			// 且若当序列引入符会把中文的连续字节吞进假序列（实测踩过）。
			if b == 0x1b {
				flush()
				f.state = stEsc
				f.buf = append(f.buf[:0], b)
				continue
			}
			cur = append(cur, b)
		case stEsc:
			f.buf = append(f.buf, b)
			switch b {
			case '[':
				f.state = stCSI
			case ']', 'P', 'X', '^', '_':
				// 字符串体经当前段流动（buf 里的 ESC+引入符一并并入）
				cur = append(cur, f.buf...)
				f.buf = f.buf[:0]
				f.state = stStr
			case 0x1b: // ESC ESC：重新开始
				f.buf = append(f.buf[:0], b)
			default:
				// 非 CSI/字符串的两字节序列，归属当前段
				f.emitSeq(&out, &cur, &curShadow)
				f.state = stGround
			}
		case stCSI:
			f.buf = append(f.buf, b)
			if b >= 0x40 && b <= 0x7e { // 终结字节，序列完整
				f.decideCSI(&out, &cur, &curShadow)
				f.state = stGround
			} else if b == 0x18 || b == 0x1a { // CAN/SUB 中止
				f.emitSeq(&out, &cur, &curShadow)
				f.state = stGround
			}
		case stStr:
			switch b {
			// 终结符只留 7-bit 形态（BEL/CAN/SUB/ESC\）：0x9C 可能是 UTF-8
			// 连续字节，不能当终止。
			case 0x07, 0x18, 0x1a:
				f.state = stGround
				cur = append(cur, b)
			case 0x1b:
				f.state = stStrEsc
				cur = append(cur, b)
			default:
				cur = append(cur, b)
			}
		case stStrEsc:
			cur = append(cur, b)
			if b == '\\' {
				f.state = stGround
			} else if b != 0x1b {
				// 简化处理：非 ST 的 ESC 序列按字符串体继续
				f.state = stStr
			}
		}
	}
	flush()
	return out
}

// emitSeq 把已裁决为「非切换」的序列交给当前段（影子/非影子由当前状态定）。
func (f *altFilter) emitSeq(out *[]seg, cur *[]byte, curShadow *bool) {
	*cur = append(*cur, f.buf...)
	if len(*cur) > 0 {
		*out = append(*out, seg{data: *cur, shadow: *curShadow})
		*cur = []byte{}
	}
}

// decideCSI 裁决一条完整 CSI：是备用屏切换（不进影子、翻转状态）还是普通序列
// （归属当前段）。
func (f *altFilter) decideCSI(out *[]seg, cur *[]byte, curShadow *bool) {
	isSwitch := false
	enter := false
	q := 1 // '?' 的下标：7-bit 是 ESC [ ?，C1 是 0x9B ?
	if len(f.buf) > 1 && f.buf[0] == 0x1b {
		if len(f.buf) > 2 && f.buf[1] == '[' {
			q = 2
		}
	}
	if len(f.buf) > q+1 && f.buf[q] == '?' {
		params := string(f.buf[q+1 : len(f.buf)-1])
		final := f.buf[len(f.buf)-1]
		if final == 'h' || final == 'l' {
			for _, p := range []string{"1049", "1047", "47"} {
				if params == p || bytes.Contains(f.buf, []byte(";"+p)) || bytes.Contains(f.buf, []byte(p+";")) {
					isSwitch = true
					enter = final == 'h'
					break
				}
			}
		}
	}
	if isSwitch {
		flushOut(out, cur, curShadow)
		f.inAlt = enter
		*curShadow = !f.inAlt
		return
	}
	f.emitSeq(out, cur, curShadow)
}

// flushOut 输出当前段。
func flushOut(out *[]seg, cur *[]byte, curShadow *bool) {
	if len(*cur) > 0 {
		*out = append(*out, seg{data: *cur, shadow: *curShadow})
		*cur = []byte{}
	}
}
