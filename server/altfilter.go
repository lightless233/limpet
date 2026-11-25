package main

import "strings"

// altFilter 把 PTY 字节流切分为「主屏段 / 备用屏段」：影子网格只消费主屏段
// （spec 003「全屏程序运行中重接」）。真实网格始终吃全量原始字节，不受本过滤器
// 影响——过滤器只为影子的喂入切分服务。
//
// 识别 7-bit CSI ? 1049 / 1047 / 47 的 h 或 l，C1 字节按 UTF-8 数据处理。
// OSC / DCS / APC / SOS / PM 的字符串体不参与切分，只有 OSC 接受 BEL 终止。
// RIS 退出备用屏，DECSTR 软重置当前屏，采用 libghostty 的活动屏幕语义。
//
// 批尾停在序列中间时暂存字节，下一批到达后确定归属及影子应执行的控制序列。
type altFilter struct {
	inAlt bool
	state int
	osc   bool
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
				f.osc = b == ']'
			case 'c': // RIS 重置主屏，并离开备用屏。
				flush()
				f.inAlt = false
				curShadow = true
				f.emitSeq(&out, &cur, &curShadow)
				f.state = stGround
			case 0x1b: // ESC ESC：重新开始
				f.buf = append(f.buf[:0], b)
			default:
				// 非 CSI/字符串的两字节序列，归属当前段
				f.emitSeq(&out, &cur, &curShadow)
				f.state = stGround
			}
		case stCSI:
			if b == 0x1b {
				// 中止旧 CSI，当前 ESC 开始新序列。
				f.buf = append(f.buf[:0], b)
				f.state = stEsc
				continue
			}
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
			case 0x07:
				if f.osc {
					f.state = stGround
				}
				cur = append(cur, b)
			case 0x18, 0x1a:
				f.state = stGround
				cur = append(cur, b)
			case 0x1b:
				f.state = stStrEsc
			default:
				cur = append(cur, b)
			}
		case stStrEsc:
			cur = append(cur, 0x1b, '\\')
			f.state = stGround
			if b != '\\' {
				// libghostty 遇到 ESC 已离开字符串。先在影子流结束字符串，
				// 再裁决后续序列，它可能是需要从影子流剔除的备用屏切换。
				f.buf = append(f.buf[:0], 0x1b)
				f.state = stEsc
				i--
			}
		}
	}
	flush()
	return out
}

// emitSeq 把已裁决为「非切换」的序列交给当前段（影子/非影子由当前状态定）。
func (f *altFilter) emitSeq(out *[]seg, cur *[]byte, curShadow *bool) {
	*cur = append(*cur, f.buf...)
	flushOut(out, cur, curShadow)
}

// decideCSI 剔除备用屏切换参数，将组合序列中其余模式交给影子网格。
func (f *altFilter) decideCSI(out *[]seg, cur *[]byte, curShadow *bool) {
	final := f.buf[len(f.buf)-1]
	if len(f.buf) <= 3 || f.buf[2] != '?' || (final != 'h' && final != 'l') {
		f.emitSeq(out, cur, curShadow)
		return
	}

	params := strings.Split(string(f.buf[3:len(f.buf)-1]), ";")
	remaining := params[:0]
	switchScreen := false
	for _, param := range params {
		switch param {
		case "1049", "1047", "47":
			switchScreen = true
		default:
			remaining = append(remaining, param)
		}
	}
	if !switchScreen {
		f.emitSeq(out, cur, curShadow)
		return
	}

	flushOut(out, cur, curShadow)
	if len(remaining) > 0 {
		seq := []byte("\x1b[?" + strings.Join(remaining, ";"))
		*out = append(*out, seg{data: append(seq, final), shadow: true})
	}
	f.inAlt = final == 'h'
	*curShadow = !f.inAlt
}

// flushOut 输出当前段。
func flushOut(out *[]seg, cur *[]byte, curShadow *bool) {
	if len(*cur) > 0 {
		*out = append(*out, seg{data: *cur, shadow: *curShadow})
		*cur = []byte{}
	}
}
