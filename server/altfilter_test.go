package main

import (
	"bytes"
	"strings"
	"testing"
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
