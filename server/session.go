package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	gridCols, gridRows     = 80, 24
	gridScrollback         = 10000 // 与重接深度对齐（spec 003）；更深的历史由录制承担
	recFlushBytes          = 32 << 10
	promptCwdSampleThrottle = 200 * time.Millisecond
)

// session 是一个活会话：PTY、libghostty 网格、录制与当前消费者。
//
// 并发模型（刻意的简单）：grid 不是并发安全的，全部 grid 访问经 gridMu 串行——
// 读循环喂字节、接入方重建与取切点、resize，都是短临界区，吞吐实测远超需要。
type session struct {
	id, name  string
	createdAt time.Time
	m         *manager

	cmd *exec.Cmd
	ptmx *os.File

	grid   *libghostty.Terminal
	gridMu sync.Mutex

	// 录制：log 是纯原始字节（只追加）；meta 是事件行，偏移是 log 的文件偏移。
	logPath, metaPath string
	logF, metaF       *os.File
	logW              *bufio.Writer
	metaW             *bufio.Writer
	logBytes          int64 // 已写入 log 的字节数（= 事件偏移的共同坐标）
	recBroken         bool
	lastCwd           string
	lastCwdAt         time.Time

	fedBytes atomic.Int64 // 已喂入网格的字节数（读循环维护；写回回调读取）

	consumer atomic.Pointer[consumer]

	done      chan struct{}
	closeOnce sync.Once
}

// startSession 启动 Shell（PTY）、网格与录制；PTY 就绪才返回（spec：创建即启动 Shell）。
func startSession(m *manager, id, name string) (*session, error) {
	home, _ := os.UserHomeDir()
	s := &session{
		id:        id,
		name:      name,
		createdAt: time.Now(),
		m:         m,
		logPath:   filepath.Join(m.histDir, id+".log"),
		metaPath:  filepath.Join(m.histDir, id+".meta"),
		done:      make(chan struct{}),
	}

	// 录制文件先行：PTY 启动即开始，首条事件是偏移 0 的启动时刻与初始尺寸。
	lf, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	mf, err := os.OpenFile(s.metaPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		lf.Close()
		return nil, err
	}
	s.logF, s.metaF = lf, mf
	s.logW = bufio.NewWriterSize(lf, recFlushBytes)
	s.metaW = bufio.NewWriterSize(mf, 4096)

	if err := s.recordMeta("T", 0, fmt.Sprint(s.createdAt.UnixMilli())); err != nil {
		s.closeRecording()
		return nil, err
	}
	_ = s.recordMeta("S", 0, fmt.Sprintf("%dx%d", gridCols, gridRows))
	if cwd, _ := os.Readlink("/proc/self/cwd"); cwd != "" {
		_ = s.recordMeta("C", 0, cwd) // 初始目录记录；Shell 起来后再按 /proc 采样
	}

	// 网格。写回回调：应答路由（spec 003「终端查询由谁应答」）——查询在切点之前
	// （不会进入实时流）时由本进程写入 PTY；在切点之后交由浏览器应答，网格的
	// 应答丢弃。无消费者时 cutOffset 视为无穷大，全部代答。
	s.grid, err = libghostty.NewTerminal(
		libghostty.WithSize(gridCols, gridRows),
		libghostty.WithMaxScrollbackLines(gridScrollback),
		libghostty.WithContinuationMaxBytes(1<<20),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			s.onQueryReply(data)
		}),
	)
	if err != nil {
		s.closeRecording()
		return nil, err
	}

	// Shell：$SHELL 优先，交互模式，起始目录 $HOME，TERM 显式设置（不信任默认环境）。
	// pty.Start 强制 Setsid+Setctty：Shell 自成会话与进程组——server（前台运行时）
	// 的 Ctrl+C 不会到达它（spec 003「运行方式与生命周期边界」），关闭时按 -pid
	// 杀组即可。不能预设 Setpgid：creack/pty 合并而非替换 SysProcAttr，子进程先成
	// 组长再 setsid 会 EPERM（实测踩过）。
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-i")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	ptmx, err := pty.Start(cmd) // Start 内部把 std* 接到同一个 PTY
	if err != nil {
		s.grid.Close()
		s.closeRecording()
		return nil, fmt.Errorf("启动 Shell 失败: %w", err)
	}
	s.cmd, s.ptmx = cmd, ptmx
	s.recordMeta("S", s.logBytes, fmt.Sprintf("%dx%d", gridCols, gridRows))
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid)); err == nil {
		s.lastCwd = cwd
		s.recordMeta("C", s.logBytes, cwd)
	}

	go s.readLoop()
	return s, nil
}

// onQueryReply 见 startSession 中的注释。回调在 gridMu 临界区内（VTWrite 期间）同步执行，
// 直接写 PTY 是允许的（不得触碰网格）。
func (s *session) onQueryReply(data []byte) {
	c := s.consumer.Load()
	// 判断依据是这条查询会不会进入实时流（spec 003）：切点之后的输出已交给
	// 浏览器，由浏览器应答；切点之前的由本进程写入 PTY。fedBytes 在回调时刻
	// 是当前批次的起始偏移。
	live := c != nil && s.fedBytes.Load() >= c.cutOffset
	if live {
		return
	}
	if !s.recBroken { // 与 PTY 同侧，写入失败不致命
		_, _ = s.ptmx.Write(data)
	}
}

// readLoop：PTY 的持续读取是会话独立运行的前提，任何外部通道都不得阻塞它。
func (s *session) readLoop() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.handleOutput(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	s.finish("shell-exit")
}

// handleOutput：喂网格 → 录制 → 交给当前消费者（有界队列，满了断开本次接入）。
func (s *session) handleOutput(batch []byte) {
	start := s.fedBytes.Load()

	// 喂网格：ground 时整批直写（快路径）；停在序列中间时用 VTWriteUntil 喂到
	// 回到 ground 为止（U0-8a 定案；已在 ground 时 VTWriteUntil 消耗 0 字节，
	// 不能用它推进普通文本）。
	s.gridMu.Lock()
	rest := batch
	for len(rest) > 0 {
		if g, err := s.grid.VTGround(); err == nil && g {
			s.grid.VTWrite(rest)
			break
		}
		stop, err := s.grid.VTWriteUntil(rest, libghostty.TerminalBoundaryGround)
		if err != nil {
			break
		}
		if stop.Consumed == 0 {
			s.grid.VTWrite(rest)
			break
		}
		rest = rest[stop.Consumed:]
	}
	s.gridMu.Unlock()
	s.fedBytes.Store(start + int64(len(batch)))

	// 录制：纯原始字节 + 事件（时间、目录）。写失败降级为缺口标记，不拖住读取。
	if !s.recBroken {
		if _, err := s.logW.Write(batch); err == nil {
			s.logBytes += int64(len(batch))
			_ = s.recordMeta("T", s.logBytes-int64(len(batch)), fmt.Sprint(time.Now().UnixMilli()))
			s.sampleCwd()
			if err := s.logW.Flush(); err != nil {
				s.breakRecording()
			}
		} else {
			s.breakRecording()
		}
	} else {
		s.logBytes += int64(len(batch))
	}

	// 消费者：有界队列，满则断开本次接入（4001）——会话不受影响（spec 003 背压）。
	if c := s.consumer.Load(); c != nil {
		select {
		case c.queue <- batch:
		default:
			c.closeWith(4001, "too slow")
		}
	}
}

// sampleCwd 采样 Shell 工作目录：/proc/<pid>/cwd，不依赖 shell 集成。
// 目录变化没有内核通知，靠「切目录必产生提示符输出」在每批输出后采样（节流）。
func (s *session) sampleCwd() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if time.Since(s.lastCwdAt) < promptCwdSampleThrottle {
		return
	}
	s.lastCwdAt = time.Now()
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", s.cmd.Process.Pid))
	if err != nil || cwd == s.lastCwd {
		return
	}
	s.lastCwd = cwd
	_ = s.recordMeta("C", s.logBytes, cwd)
}

// resize 应用接入方的尺寸：PTY 与网格同源。
func (s *session) resize(cols, rows uint16) error {
	s.gridMu.Lock()
	err := s.grid.Resize(cols, rows, 0, 0)
	s.gridMu.Unlock()
	if err != nil {
		return err
	}
	if s.ptmx != nil {
		_ = pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
	if !s.recBroken {
		s.recordMeta("S", s.logBytes, fmt.Sprintf("%dx%d", cols, rows))
	}
	return nil
}

// rebuild 在当前切点生成重建画面（U0-2 定案）：formatter 一次输出包含滚动历史 +
// 活动屏，光标经 extras 还原。返回 (画面字节, 未完成序列前缀, 错误)。
func (s *session) rebuild() ([]byte, []byte, error) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()

	c := s.consumer.Load()
	if c == nil {
		return nil, nil, errors.New("无消费者")
	}
	c.cutOffset = s.fedBytes.Load()

	f, err := libghostty.NewFormatter(s.grid,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
		libghostty.WithFormatterExtras(libghostty.FormatterExtrasAll),
	)
	if err != nil {
		return nil, nil, err
	}
	out, err := f.Format()
	f.Close()
	if err != nil {
		return nil, nil, err
	}

	var cont []byte
	if g, err := s.grid.VTGround(); err == nil && !g {
		// 切点落在序列中间：把未完成序列的字节作为实时流前缀补发
		//（U0-8a 验证过，浏览器端与 worker 端状态一致）。
		cont, _ = s.grid.Continuation()
	}
	return out, cont, nil
}

// attached 当前是否有消费者。
func (s *session) attached() bool {
	return s.consumer.Load() != nil
}

// shutdown 显式关闭：回收该会话启动的全部进程（前台任务 + 后台作业），
// TERM → 兜底 KILL。注意后台作业在独立进程组，回收单位是**会话进程树**
// （spec 003）；worker 进程拆分后的 subreaper 方案在生命周期单元落地。
func (s *session) shutdown(reason string) {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			pgid := -s.cmd.Process.Pid // 负值 = 杀整个进程组
			_ = syscall.Kill(pgid, syscall.SIGTERM)
			go func() {
				time.Sleep(3 * time.Second)
				if s.cmd.Process != nil {
					_ = syscall.Kill(pgid, syscall.SIGKILL)
				}
			}()
		}
		// 关闭 PTY 让读循环退出，进而 finish() 收尾。
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
	})
	<-s.done
}

// finish 会话死亡的统一收尾（读循环出口）。
func (s *session) finish(reason string) {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	if c := s.consumer.Swap(nil); c != nil {
		c.closeWith(1000, "session ended")
	}
	s.grid.Close()
	s.closeRecording()
	s.m.remove(s.id)
	close(s.done)
}

func (s *session) closeRecording() {
	if s.logW != nil {
		_ = s.logW.Flush()
	}
	if s.metaW != nil {
		_ = s.metaW.Flush()
	}
	if s.logF != nil {
		_ = s.logF.Close()
		s.logF = nil
	}
	if s.metaF != nil {
		_ = s.metaF.Close()
		s.metaF = nil
	}
}

// breakRecording 写不动：标记缺口，之后不再尝试录制，绝不拖住 PTY 读取。
func (s *session) breakRecording() {
	if s.recBroken {
		return
	}
	s.recBroken = true
	_ = s.recordMeta("G", s.logBytes, "")
}

// recordMeta 追加一条事件（偏移是 log 的文件偏移）。meta 写失败静默——
// 它只服务将来的历史浏览，不影响会话。
func (s *session) recordMeta(kind string, offset int64, val string) error {
	if s.metaW == nil {
		return nil
	}
	line := fmt.Sprintf("%s\t%d\t%s\n", kind, offset, val)
	if _, err := s.metaW.WriteString(line); err != nil {
		return err
	}
	return s.metaW.Flush()
}
