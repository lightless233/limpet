package main

// worker 侧的会话：PTY、libghostty 双网格、录制与 N 个 viewer（多视图，spec 004）。
// 本文件运行在 worker 进程里（`limpet-server worker <uuid>`），进程退出即会话终结。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	gridCols, gridRows = 80, 24
	gridScrollback     = 10000 // 与重接深度对齐（spec 003）；更深的历史由录制承担
	recFlushBytes      = 32 << 10
	cwdSampleThrottle  = 200 * time.Millisecond
)

// viewer 是一个接入的渲染器：连接、有界实时流队列、申报窗口尺寸与切点。
type viewer struct {
	conn      *frameConn
	queue     chan []byte
	dims      viewerDims   // 申报的窗口尺寸（跟随输入者的晋升依据）
	cutOffset atomic.Int64 // 重建覆盖到的流偏移；实时流从此之后接上
	queued    atomic.Int64 // 队列里积压的字节数
	closed    atomic.Bool
	done      chan struct{} // 置位 closed 的同时关闭：通知输出泵收尾
	// 关闭原因（仅在赢得 closed 置位的那次写入，输出泵在 done 之后读取）。code 为 0 = 不发 DETACH。
	dropCode   int
	dropReason string
}

// closeWith 丢弃 viewer（积压超限）。它只置位并通知输出泵——DETACH 与关闭连接都由泵来做：
// 泵是这条连接在重建之后唯一的写者，可能正持着写锁阻塞在写上（最长 2s）；在这里等锁会把
// PTY 读循环卡住，Shell 的输出随之停摆。
func (v *viewer) closeWith(code int, reason string) {
	if !v.closed.CompareAndSwap(false, true) {
		return
	}
	v.dropCode, v.dropReason = code, reason
	if code != 0 {
		log.Printf("viewer 被丢弃（%s，积压 %d 字节）", reason, v.queued.Load())
	}
	close(v.done)
}

// sessionPaths 是 worker 需要的文件系统位置。
type sessionPaths struct {
	histDir   string
	socketDir string
}

// session 是 worker 进程的全部状态：PTY、双网格、录制与 viewer 集合。
type session struct {
	id        string
	createdAt time.Time
	paths     sessionPaths

	cmd  *exec.Cmd
	ptmx *os.File

	grid   *libghostty.Terminal // 真实网格：吃全量原始字节（备用屏也进）
	shadow *libghostty.Terminal // 影子网格：只吃主屏阶段字节，供全屏程序运行中重接还原主屏
	gridMu sync.Mutex           // 同时保护 grid/shadow 的访问与 fedBytes 的更新
	altf   altFilter

	curCols, curRows uint16 // 网格当前尺寸（跟随输入者）

	logPath, metaPath string
	logF, metaF       *os.File
	logW              *bufio.Writer
	metaW             *bufio.Writer
	logBytes          int64 // 已写入 log 的字节数（= 事件偏移的共同坐标）
	recBroken         bool
	lastCwd           string
	lastCwdAt         time.Time

	fedBytes atomic.Int64 // 已喂入真实网格的字节数（读循环在网格临界区内维护）

	viewersMu sync.Mutex
	viewers   map[*viewer]viewerDims // 全部接入中的 viewer 及其申报尺寸

	done      chan struct{}
	closeOnce sync.Once
}

// startSession 启动 Shell（PTY）、双网格与录制；PTY 就绪才返回（创建即启动 Shell）。
func startSession(paths sessionPaths, id string) (*session, error) {
	home, _ := os.UserHomeDir()
	s := &session{
		id:        id,
		createdAt: time.Now(),
		paths:     paths,
		logPath:   filepath.Join(paths.histDir, id+".log"),
		metaPath:  filepath.Join(paths.histDir, id+".meta"),
		viewers:   map[*viewer]viewerDims{},
		done:      make(chan struct{}),
	}
	s.curCols, s.curRows = gridCols, gridRows

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
	_ = s.recordMeta("T", 0, fmt.Sprint(s.createdAt.UnixMilli()))
	_ = s.recordMeta("S", 0, fmt.Sprintf("%dx%d", gridCols, gridRows))

	// 网格。写回回调：查询应答路由（spec 003/004）——多视图下程序查询随字节流
	// 到达所有 viewer，v1 已知限制是应答可能重复；worker 只在「没有任何 viewer
	// 持有实时流」时代答（全离线，程序不因无人接入而卡等）。
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
	s.shadow, err = libghostty.NewTerminal(
		libghostty.WithSize(gridCols, gridRows),
		libghostty.WithMaxScrollbackLines(gridScrollback),
	)
	if err != nil {
		s.grid.Close()
		s.closeRecording()
		return nil, err
	}

	// Shell：$SHELL 优先，交互模式，起始目录 $HOME，TERM 显式设置（不信任默认环境）。
	// pty.Start 强制 Setsid+Setctty：Shell 自成会话与进程组，随 worker 存活；
	// 不能预设 Setpgid：creack/pty 合并而非替换 SysProcAttr，子进程先成组长再
	// setsid 会 EPERM（实测踩过）。
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
	ptmx, err := pty.Start(cmd)
	if err != nil {
		s.grid.Close()
		s.shadow.Close()
		s.closeRecording()
		return nil, fmt.Errorf("启动 Shell 失败: %w", err)
	}
	s.cmd, s.ptmx = cmd, ptmx
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
	if s.hasLiveViewer() {
		return // 有 viewer 在看实时流：查询已随流转发，由浏览器应答
	}
	_, _ = s.ptmx.Write(data)
}

// hasLiveViewer 是否存在已拿到实时流切点的 viewer。
func (s *session) hasLiveViewer() bool {
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	fed := s.fedBytes.Load()
	for v := range s.viewers {
		if v.cutOffset.Load() <= fed {
			return true
		}
	}
	return false
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

// handleOutput：喂双网格 → 录制 → 广播给所有 viewer（各自有界队列，满则仅丢弃该 viewer）。
func (s *session) handleOutput(batch []byte) {
	// 真实网格吃全量；影子网格只吃主屏段（过滤器切分）。
	// fedBytes 必须在临界区内更新：重建的切点取自它，若在解锁后写，
	// 重建可能读到旧值——刚喂入的字节「在重建画面里 + 又被实时流重发」
	// （验收 15 实测抓到过：5 行重复）。
	s.gridMu.Lock()
	start := s.fedBytes.Load()
	feedGround(s.grid, batch)
	for _, sg := range s.altf.process(batch) {
		if !sg.shadow {
			continue
		}
		feedGround(s.shadow, sg.data)
	}
	s.fedBytes.Store(start + int64(len(batch)))
	s.gridMu.Unlock()

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

	// 广播：只送各 viewer 切点之后的字节（切点之前的已在各自重建画面里）。
	// 积压按字节计（queueBytes），槽数只是碎块突发的余量：PTY 的一批可能只有几十字节
	// （Shell 回显长行时逐段重绘），按块计会把几 KB 的积压误判成慢消费者。
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	for v := range s.viewers {
		if v.closed.Load() {
			continue
		}
		cut := v.cutOffset.Load()
		var tail []byte
		switch {
		case start >= cut:
			tail = batch
		case start+int64(len(batch)) > cut:
			tail = batch[cut-start:]
		default:
			tail = nil
		}
		if len(tail) == 0 {
			continue
		}
		if v.queued.Add(int64(len(tail))) > queueBytes {
			v.closeWith(4001, "too slow")
			continue
		}
		select {
		case v.queue <- tail:
		default:
			v.closeWith(4001, "too slow")
		}
	}
}

// sampleCwd 采样 Shell 工作目录：/proc/<pid>/cwd，不依赖 shell 集成。
// 目录变化没有内核通知，靠「切目录必产生提示符输出」在每批输出后采样（节流）。
func (s *session) sampleCwd() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	if time.Since(s.lastCwdAt) < cwdSampleThrottle {
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

// resize 应用尺寸：PTY 与双网格同源（尺寸跟随输入者，spec 004）。
func (s *session) resize(cols, rows uint16) {
	s.gridMu.Lock()
	_ = s.grid.Resize(cols, rows, 0, 0)
	_ = s.shadow.Resize(cols, rows, 0, 0)
	s.curCols, s.curRows = cols, rows
	s.gridMu.Unlock()
	if s.ptmx != nil {
		_ = pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
	if !s.recBroken {
		s.recordMeta("S", s.logBytes, fmt.Sprintf("%dx%d", cols, rows))
	}
}

// currentDims 返回网格当前尺寸（调用方多在 gridMu 临界区内）。
func (s *session) currentDims() (uint16, uint16) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	return s.curCols, s.curRows
}

// rebuildFor 在当前切点为指定 viewer 生成重建画面（U0 定案）。备用屏激活时用
// 影子网格还原主屏，再进入备用屏还原活动屏。返回 (画面, 未完成序列前缀, 错误)。
func (s *session) rebuildFor(v *viewer) ([]byte, []byte, error) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()

	v.cutOffset.Store(s.fedBytes.Load())

	screen, err := s.grid.ActiveScreen()
	if err != nil {
		return nil, nil, err
	}
	var out []byte
	if screen == libghostty.ScreenAlternate {
		sh, err := formatVT(s.shadow)
		if err != nil {
			return nil, nil, err
		}
		rt, err := formatVT(s.grid)
		if err != nil {
			return nil, nil, err
		}
		out = append(append(sh, "\x1b[?1049h"...), rt...)
	} else {
		out, err = formatVT(s.grid)
		if err != nil {
			return nil, nil, err
		}
	}

	var cont []byte
	if g, err := s.grid.VTGround(); err == nil && !g {
		// 切点落在序列中间：把未完成序列的字节作为实时流前缀补发
		//（U0-8a 验证过，浏览器端与 worker 端状态一致）。
		cont, _ = s.grid.Continuation()
	}
	return out, cont, nil
}

// attached 是否存在接入中的 viewer（已被丢弃、尚未摘除的不算）。
func (s *session) attached() bool {
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	for v := range s.viewers {
		if !v.closed.Load() {
			return true
		}
	}
	return false
}

// shutdown 显式关闭：worker 是子进程收割者（subreaper），按会话进程树回收——
// 带作业控制的 Shell 给每个后台作业单开进程组，只杀 Shell 组杀不到它们
// （bash/zsh 均实测复现）。TERM → 兜底 KILL，随后移除 socket、结束进程。
func (s *session) shutdown(reason string) {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			reapTree(s.cmd.Process.Pid)
		}
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
	})
	<-s.done
}

// finish 会话死亡的统一收尾（读循环出口）。通知全部 viewer（1000）、移除 socket
// 文件、关闭资源。进程退出由 worker 主函数在 done 上等待后完成。
func (s *session) finish(reason string) {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	s.viewersMu.Lock()
	viewers := make([]*viewer, 0, len(s.viewers))
	for v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.viewers = map[*viewer]viewerDims{}
	s.viewersMu.Unlock()
	for _, v := range viewers {
		if !v.closed.Load() {
			_ = v.conn.writeFrameTimeout(ftExit, mustJSON(exitMsg{Reason: reason}), time.Second)
		}
		v.closeWith(0, "")
		v.conn.close()
	}
	s.grid.Close()
	s.shadow.Close()
	s.closeRecording()
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

// removeViewer 从集合移除一个 viewer。
func (s *session) removeViewer(v *viewer) {
	s.viewersMu.Lock()
	delete(s.viewers, v)
	s.viewersMu.Unlock()
}

// ---- 进程树回收 ----

// setSubreaper 把 worker 设为子进程收割者：孤儿化的后代（后台作业在 Shell 死后
// 被 reparent）改收养到 worker，关闭时按树回收，而不是泄漏给 init。
func setSubreaper() error {
	// prctl(PR_SET_CHILD_SUBREAPER = 36, 1)
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, 36, 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// reapTree 以 base（Shell）为根回收整棵后代进程树：先对 Shell 进程组 TERM
// （快速路径），再对树中脱离了进程组的作业逐个 TERM，宽限后 KILL，最后收割僵尸。
func reapTree(base int) {
	kill := func(pids []int, sig syscall.Signal) {
		for _, p := range pids {
			_ = syscall.Kill(p, sig)
		}
	}
	tree := descendants(base)
	// 快速路径：Shell 进程组整组 TERM
	_ = syscall.Kill(-base, syscall.SIGTERM)
	// 树路径：脱离进程组的后台作业
	kill(tree, syscall.SIGTERM)
	time.Sleep(killGrace)
	tree = descendants(base)
	_ = syscall.Kill(-base, syscall.SIGKILL)
	kill(tree, syscall.SIGKILL)
	// 收割僵尸（worker 是 subreaper，这些孩子归它）
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			break
		}
	}
}

// descendants 返回 base 的全部存活后代 pid（经 /proc 一次扫描 + BFS）。
func descendants(base int) []int {
	type proc struct {
		pid  int
		ppid int
	}
	var procs []proc
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		for _, c := range e.Name() {
			if c < '0' || c > '9' {
				pid = 0
				break
			}
			pid = pid*10 + int(c-'0')
		}
		if pid == 0 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// stat: pid (comm) state ppid ...——comm 可含空格与括号，从最后一个 ')' 后取字段
		idx := strings.LastIndexByte(string(b), ')')
		if idx < 0 {
			continue
		}
		fields := strings.Fields(string(b[idx+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid := 0
		for _, c := range fields[1] {
			if c < '0' || c > '9' {
				ppid = 0
				break
			}
			ppid = ppid*10 + int(c-'0')
		}
		procs = append(procs, proc{pid: pid, ppid: ppid})
	}
	children := map[int][]int{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	var out []int
	stack := []int{base}
	seen := map[int]bool{base: true}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[p] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				stack = append(stack, c)
			}
		}
	}
	return out
}

// ---- 喂法与格式化 ----

// feedGround 把一批字节喂入一台网格：ground 时整批直写（快路径）；停在序列中间
// 时用 VTWriteUntil 喂到回到 ground 为止（U0-8a 定案；已在 ground 时它消耗 0 字节，
// 不能用它推进普通文本）。
func feedGround(t *libghostty.Terminal, batch []byte) {
	rest := batch
	for len(rest) > 0 {
		if g, err := t.VTGround(); err == nil && g {
			t.VTWrite(rest)
			return
		}
		stop, err := t.VTWriteUntil(rest, libghostty.TerminalBoundaryGround)
		if err != nil {
			return
		}
		if stop.Consumed == 0 {
			t.VTWrite(rest)
			return
		}
		rest = rest[stop.Consumed:]
	}
}

// formatVT 用全量 extras 把一台网格的当前屏（含其滚动历史）序列化为 VT。
func formatVT(t *libghostty.Terminal) ([]byte, error) {
	f, err := libghostty.NewFormatter(t,
		libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
		libghostty.WithFormatterExtras(libghostty.FormatterExtrasAll),
	)
	if err != nil {
		return nil, err
	}
	out, err := f.Format()
	f.Close()
	return out, err
}

// mustJSON 序列化协议载荷；失败视为编程错误（panic 合理）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
