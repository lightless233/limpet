package main

import (
	"bytes"
	"strings"
	"testing"

	"go.mitchellh.com/libghostty"
)

// filterCase 单条用例：多批输入 + 期望影子流 + 不得泄漏进影子流的内容 + 最终 inAlt。
// 注意：被丢弃的段（备用屏内容与切换序列）只属于影子喂入的切分，真实网格始终
// 吃原始整批字节，与过滤器无关，因此不对「丢弃侧」做精确断言。
type filterCase struct {
	name      string
	batches   []string
	wantPri   string   // 影子流应精确等于的主屏内容
	ban       []string // 不得出现在影子流里的内容
	wantInAlt bool
}

func TestAltFilter(t *testing.T) {
	SW_IN := "\x1b[?1049h"
	SW_OUT := "\x1b[?1049l"
	cases := []filterCase{
		{
			name:    "纯文本，整段归影子",
			batches: []string{"hello 世界\r\n"},
			wantPri: "hello 世界\r\n",
		},
		{
			name:      "一条批内完整进出备用屏",
			batches:   []string{"abc" + SW_IN + "备用内容" + SW_OUT + "back"},
			wantPri:   "abcback",
			ban:       []string{"备用内容", SW_IN},
			wantInAlt: false,
		},
		{
			name:      "切换序列跨批拆开（批尾停在参数中间）",
			batches:   []string{"abc\x1b[?10", "49hX"},
			wantPri:   "abc",
			ban:       []string{"X", "\x1b["},
			wantInAlt: true,
		},
		{
			name:      "跨批的非切换 CSI 归影子",
			batches:   []string{"abc\x1b[?25", "lDEF"},
			wantPri:   "abc\x1b[?25lDEF",
			wantInAlt: false,
		},
		{
			name:      "多批进出",
			batches:   []string{SW_IN, "abc", SW_OUT, "def"},
			wantPri:   "def",
			ban:       []string{"abc", SW_IN},
			wantInAlt: false,
		},
		{
			name:    "OSC 标题整段归影子（字符串体不扫描）",
			batches: []string{"\x1b]0;标题\a正文"},
			wantPri: "\x1b]0;标题\a正文",
		},
		{
			name:    "1049 的前缀参数（10499）不是切换",
			batches: []string{"x\x1b[?10499h后"},
			wantPri: "x\x1b[?10499h后",
		},
		{
			name:      "legacy 1047 与 47 变体",
			batches:   []string{"a\x1b[?1047h备1047\x1b[?1047l", "b\x1b[?47h备47\x1b[?47lC"},
			wantPri:   "abC",
			ban:       []string{"备1047", "备47"},
			wantInAlt: false,
		},
		{
			name:      "组合参数 1049;25 保留光标模式",
			batches:   []string{"p\x1b[?1049;25h在备用屏"},
			wantPri:   "p\x1b[?25h",
			ban:       []string{"在备用屏"},
			wantInAlt: true,
		},
		{
			name:    "参数 470 与 147 均不匹配备用屏模式",
			batches: []string{"x\x1b[?25;470h\x1b[?147;25ly"},
			wantPri: "x\x1b[?25;470h\x1b[?147;25ly",
		},
		{
			name:    "组合 DECSET 和 DECRST 保留其余模式",
			batches: []string{"x\x1b[?2004;1049hALT\x1b[?25;1049ly"},
			wantPri: "x\x1b[?2004h\x1b[?25ly",
		},
		{
			name:    "ESC 跨批中止 CSI 后开始新序列",
			batches: []string{"x\x1b[?25", SW_IN + "ALT" + SW_OUT + "y"},
			wantPri: "xy",
		},
		{
			name:    "RIS 复位备用屏与影子主屏",
			batches: []string{"x" + SW_IN + "ALT\x1b", "cy"},
			wantPri: "x\x1bcy",
		},
		{
			name:      "DECSTR 保持活动备用屏",
			batches:   []string{"x\x1b[!p" + SW_IN + "ALT\x1b[!pALT2"},
			wantPri:   "x\x1b[!p",
			wantInAlt: true,
		},
		{
			name:    "非 OSC 字符串内 BEL 不终止，ESC 可开始新的 CSI",
			batches: []string{"x\x1bPqABC\aDEF\x1b", "[?1049hALT" + SW_OUT + "y"},
			wantPri: "x\x1bPqABC\aDEF\x1b\\y",
		},
		{
			name:    "普通 CSI（清屏/SGR）归影子",
			batches: []string{"\x1b[2J\x1b[H\x1b[31m红\x1b[0m"},
			wantPri: "\x1b[2J\x1b[H\x1b[31m红\x1b[0m",
		},
		{
			name:      "批量输出中夹杂切换（高吞吐切分）",
			batches:   []string{strings.Repeat("主屏内容", 50) + SW_IN + strings.Repeat("备用内容", 50) + SW_OUT + "回到主屏"},
			wantPri:   strings.Repeat("主屏内容", 50) + "回到主屏",
			ban:       []string{"备用内容"},
			wantInAlt: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &altFilter{}
			var shadow []byte
			for _, b := range tc.batches {
				for _, s := range f.process([]byte(b)) {
					if s.shadow {
						shadow = append(shadow, s.data...)
					}
				}
			}
			if !bytes.Equal(shadow, []byte(tc.wantPri)) {
				t.Fatalf("影子流 = %q, 期望 %q", shadow, tc.wantPri)
			}
			for _, banned := range tc.ban {
				if bytes.Contains(shadow, []byte(banned)) {
					t.Fatalf("影子流泄漏了 %q: %q", banned, shadow)
				}
			}
			if f.inAlt != tc.wantInAlt {
				t.Fatalf("inAlt = %v, 期望 %v", f.inAlt, tc.wantInAlt)
			}
		})
	}
}

func TestAltFilterMatchesGhosttyReset(t *testing.T) {
	grid, err := libghostty.NewTerminal(libghostty.WithSize(gridCols, gridRows))
	if err != nil {
		t.Fatal(err)
	}
	defer grid.Close()

	var f altFilter
	for _, tc := range []struct {
		data string
		alt  bool
	}{
		{"\x1b[?1049h", true},
		{"\x1b[!p", true},
		{"\x1bc", false},
	} {
		grid.VTWrite([]byte(tc.data))
		f.process([]byte(tc.data))
		screen, err := grid.ActiveScreen()
		if err != nil || (screen == libghostty.ScreenAlternate) != tc.alt || f.inAlt != tc.alt {
			t.Fatalf("写入 %q 后 screen=%v, inAlt=%v, err=%v", tc.data, screen, f.inAlt, err)
		}
	}
}

func TestAltFilterBELOnlyTerminatesOSC(t *testing.T) {
	for _, introducer := range []byte{']', 'P', '_', '^', 'X'} {
		f := altFilter{}
		f.process([]byte{0x1b, introducer, 'x', 0x07})
		if (f.state == stGround) != (introducer == ']') {
			t.Errorf("字符串 ESC %c 收到 BEL 后 state=%d", introducer, f.state)
		}
		f.process([]byte{0x1b, '\\'})
		if f.state != stGround {
			t.Errorf("字符串 ESC %c 收到 ST 后 state=%d", introducer, f.state)
		}
	}
}
