package main

// worker 侧的会话：PTY、libghostty 双网格、录制与 N 个 viewer（多视图，spec 004）。
// 本文件运行在 worker 进程里（`limpet-server worker <uuid>`），进程退出即会话终结。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	gridCols, gridRows = 80, 24
	queryQueueSize     = 64
)

// viewer 是一个接入的渲染器：连接、有界实时流队列、申报窗口尺寸与切点。
type viewer struct {
	conn      *frameConn
	queue     chan viewerEvent
	dims      viewerDims   // 申报的窗口尺寸（跟随输入者的晋升依据）
	locked    bool         // 只发送输入，不参与尺寸晋升（inputMu）
	cutOffset atomic.Int64 // 重建覆盖到的流偏移；实时流从此之后接上
	queued    atomic.Int64 // 队列里积压的字节数
	closed    atomic.Bool
	done      chan struct{} // 置位 closed 的同时关闭：通知输出泵收尾
	// 关闭原因（仅在赢得 closed 置位的那次写入，输出泵在 done 之后读取）。code 为 0 = 不发 DETACH。
	dropCode   int
	dropReason string
}

// 尺寸和 VT 字节共享一个队列，以网格应用顺序发送。只有尺寸事件的 data 为空。
type viewerEvent struct {
	data []byte
	size resizeMsg
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
	histDir      string
	socketDir    string
	settingsPath string
}

// session 是 worker 进程的全部状态：PTY、双网格、录制与 viewer 集合。
type session struct {
	id        string
	createdAt time.Time
	cli       bool

	cmd  *exec.Cmd
	ptmx *os.File

	grid       *libghostty.Terminal // 真实网格：吃全量原始字节（备用屏也进）
	shadow     *libghostty.Terminal // 影子网格：只吃主屏阶段字节，供全屏程序运行中重接还原主屏
	gridMu     sync.Mutex           // 同时保护 grid/shadow 的访问与 fedBytes 的更新
	altf       altFilter
	scrollback int // guarded by gridMu

	curCols, curRows uint16 // 网格当前尺寸（跟随输入者）

	rec        *recording
	queryQueue chan []byte
	inputMu    sync.Mutex
	owner      *viewer // guarded by inputMu

	fedBytes atomic.Int64 // 已喂入真实网格的字节数（读循环在网格临界区内维护）

	viewersMu sync.Mutex
	viewers   map[*viewer]struct{} // 全部接入中的 viewer
	viewerWG  sync.WaitGroup
	ending    atomic.Bool
	end       exitMsg // published by closing each viewer queue

	done      chan struct{}
	closeOnce sync.Once
	stop      chan struct{}
}

// startSession prepares both grids and registers the launching viewer before
// starting the command. Only that viewer starts at byte zero without replay.
func startSession(paths sessionPaths, id string, config *workerConfig, first *viewer,
	reportStartFailure func(error)) (_ *session, err error) {
	cwd, _ := os.UserHomeDir()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = loginShell()
	}
	argv := []string{shell, "-i"}
	env := append(os.Environ(), "TERM=xterm-256color")
	dims := viewerDims{Cols: gridCols, Rows: gridRows}
	if config != nil {
		argv, cwd, dims = config.Argv, config.Cwd, config.Dims
		env = cliEnv()
		if os.Getenv("TERM") == "" {
			env = append(env, "TERM=xterm-256color")
		}
	}
	s := &session{
		id: id, createdAt: time.Now(), cli: config != nil,
		curCols: dims.Cols, curRows: dims.Rows, owner: first,
		queryQueue: make(chan []byte, queryQueueSize),
		viewers:    make(map[*viewer]struct{}), done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	if first != nil {
		s.viewers[first] = struct{}{}
		s.viewerWG.Add(1)
	}

	var recordingFiles []*os.File
	defer func() {
		if err == nil {
			return
		}
		if reportStartFailure != nil {
			reportStartFailure(err)
		}
		for _, file := range recordingFiles {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	lf, err := os.OpenFile(filepath.Join(paths.histDir, id+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	recordingFiles = append(recordingFiles, lf)
	mf, err := os.OpenFile(filepath.Join(paths.histDir, id+".meta"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	recordingFiles = append(recordingFiles, mf)

	s.scrollback, err = readScrollback(paths.settingsPath)
	if err != nil {
		log.Printf("读取 worker %s 的回滚设置失败，采用默认值: %v", id, err)
		s.scrollback, err = defaultScrollback, nil
	}
	options := []libghostty.TerminalOption{
		libghostty.WithSize(dims.Cols, dims.Rows),
		libghostty.WithMaxScrollbackBytes(math.MaxUint),
	}
	if s.scrollback != 0 {
		options = append(options, libghostty.WithMaxScrollbackLines(uint(s.scrollback)))
	}
	s.grid, err = libghostty.NewTerminal(append(options,
		libghostty.WithContinuationMaxBytes(1<<20),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			s.onQueryReply(data)
		}),
	)...)
	if err != nil {
		return nil, err
	}
	s.shadow, err = libghostty.NewTerminal(options...)
	if err != nil {
		s.grid.Close()
		return nil, err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = cwd, env
	ptmx, err := startSessionPTY(cmd, dims)
	if err != nil {
		s.grid.Close()
		s.shadow.Close()
		return nil, fmt.Errorf("启动 %s 失败: %w", filepath.Base(argv[0]), err)
	}
	s.cmd, s.ptmx = cmd, ptmx
	s.rec = &recording{
		queue: make(chan recordingEvent, recordingQueueSize),
		done:  make(chan struct{}), finalDims: dims,
	}
	go s.rec.write(lf, mf, s.createdAt, dims, cwd, fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid))
	go s.writeQueryReplies()
	go s.run()
	return s, nil
}

// syncScrollback serializes the disk check and update with CLI startup and live RPCs.
// A nil expected value is used only by CLI startup to adopt the current setting.
func (s *session) syncScrollback(path string, expected *int) error {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.ending.Load() || s.grid == nil || s.shadow == nil {
		return errors.New("会话正在结束")
	}
	lines, err := readScrollback(path)
	if err != nil {
		return fmt.Errorf("读取回滚设置失败: %w", err)
	}
	if expected != nil && lines != *expected {
		return errors.New("配置已过期")
	}
	if lines == s.scrollback {
		return nil
	}
	unlimited := uint(math.MaxUint)
	var limit *uint
	if lines != 0 {
		value := uint(lines)
		limit = &value
	}
	for _, terminal := range []*libghostty.Terminal{s.grid, s.shadow} {
		if err := terminal.SetScrollbackMaxBytes(&unlimited); err != nil {
			return err
		}
		if err := terminal.SetScrollbackMaxLines(limit); err != nil {
			return err
		}
	}
	s.scrollback = lines
	return nil
}

func startSessionPTY(cmd *exec.Cmd, dims viewerDims) (*os.File, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer slave.Close()
	defer func() {
		if err != nil {
			_ = master.Close()
		}
	}()
	// creack/pty uses File.Fd during Open, which puts the master back in
	// blocking mode. Restore nonblocking I/O before starting the child.
	fd, err := master.SyscallConn()
	if err != nil {
		return nil, err
	}
	var setErr error
	if err = fd.Control(func(fd uintptr) { setErr = syscall.SetNonblock(int(fd), true) }); err != nil {
		return nil, err
	}
	if err = setErr; err != nil {
		return nil, err
	}
	if err = master.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err = setSessionPTYSize(master, dims.Cols, dims.Rows); err != nil {
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return master, nil
}

func setSessionPTYSize(file *os.File, cols, rows uint16) error {
	size := pty.Winsize{Cols: cols, Rows: rows}
	fd, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	err = fd.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	})
	if errno != 0 {
		return errno
	}
	return err
}

func loginShell() string {
	passwd, err := os.ReadFile("/etc/passwd")
	if err == nil {
		uid := strconv.Itoa(os.Getuid())
		for _, line := range strings.Split(string(passwd), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) == 7 && fields[2] == uid && fields[6] != "" {
				return fields[6]
			}
		}
	}
	return "/bin/bash"
}

// 查询回调在 VTWrite 期间同步执行，不能等待 PTY 的写入或输入锁。
func (s *session) onQueryReply(data []byte) {
	if s.hasLiveViewer() {
		return // 有 viewer 在看实时流：查询已随流转发，由浏览器应答
	}
	// 回调数据由 libghostty 提供，离开回调前拷贝。
	select {
	case s.queryQueue <- append([]byte(nil), data...):
	default:
	}
}

func (s *session) writeQueryReplies() {
	for reply := range s.queryQueue {
		s.inputMu.Lock()
		_, err := s.ptmx.Write(reply)
		s.inputMu.Unlock()
		if err != nil {
			return
		}
	}
}

// hasLiveViewer 是否存在已拿到实时流切点的 viewer。
func (s *session) hasLiveViewer() bool {
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	fed := s.fedBytes.Load()
	for v := range s.viewers {
		if !v.closed.Load() && v.cutOffset.Load() <= fed {
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
}

// handleOutput：喂双网格 → 录制 → 广播给所有 viewer（各自有界队列，满则仅丢弃该 viewer）。
func (s *session) handleOutput(batch []byte) {
	at := time.Now().UnixMilli()
	// 真实网格吃全量；影子网格只吃主屏段（过滤器切分）。
	// fedBytes 必须在临界区内更新：重建的切点取自它，若在解锁后写，
	// 重建可能读到旧值——刚喂入的字节「在重建画面里 + 又被实时流重发」
	// （验收 15 实测抓到过：5 行重复）。
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.grid == nil {
		return
	}
	start := s.fedBytes.Load()
	feedGround(s.grid, batch)
	for _, sg := range s.altf.process(batch) {
		if !sg.shadow {
			continue
		}
		feedGround(s.shadow, sg.data)
	}
	s.fedBytes.Store(start + int64(len(batch)))
	s.rec.enqueue(viewerDims{Cols: s.curCols, Rows: s.curRows}, batch, at)

	// 广播：只送各 viewer 切点之后的字节（切点之前的已在各自重建画面里）。
	// 积压按字节计（queueBytes），槽数只是碎块突发的余量：PTY 的一批可能只有几十字节
	// （Shell 回显长行时逐段重绘），按块计会把几 KB 的积压误判成慢消费者。
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	if s.end.Reason != "" {
		return
	}
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
		case v.queue <- viewerEvent{data: tail}:
		default:
			v.closeWith(4001, "too slow")
		}
	}
}

// resize 串行应用 PTY 和双网格尺寸。调用方持 inputMu 时，晋升与对应输入不会被其他 viewer 打断。
func (s *session) resize(cols, rows uint16) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.ending.Load() || s.grid == nil || (s.curCols == cols && s.curRows == rows) {
		return
	}
	if err := setSessionPTYSize(s.ptmx, cols, rows); err != nil {
		log.Printf("设置 PTY 尺寸失败: %v", err)
		return
	}
	_ = s.grid.Resize(cols, rows, 0, 0)
	_ = s.shadow.Resize(cols, rows, 0, 0)
	s.curCols, s.curRows = cols, rows
	s.rec.enqueue(viewerDims{Cols: cols, Rows: rows}, nil, 0)
	// 播放切点与 VT 入队同受 gridMu 保护，保证前后两侧的字节不会越过尺寸事件。
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	if s.end.Reason != "" {
		return
	}
	cut := s.fedBytes.Load()
	for v := range s.viewers {
		if v.closed.Load() || v.cutOffset.Load() > cut {
			continue
		}
		select {
		case v.queue <- viewerEvent{size: resizeMsg{Cols: cols, Rows: rows}}:
		default:
			v.closeWith(4001, "too slow")
		}
	}
}

// currentDims 返回网格当前尺寸；会话结束后返回零值。
func (s *session) currentDims() (uint16, uint16) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.grid == nil {
		return 0, 0
	}
	return s.curCols, s.curRows
}

// rebuildFor 在当前切点为指定 viewer 生成重建画面（U0 定案）。备用屏激活时用
// 影子网格还原主屏，再进入备用屏还原活动屏。切点、画面与尺寸锁内对齐。
func (s *session) rebuildFor(v *viewer) ([]byte, []byte, uint16, uint16, error) {
	s.gridMu.Lock()
	defer s.gridMu.Unlock()
	if s.grid == nil {
		return nil, nil, 0, 0, errors.New("会话已结束")
	}

	v.cutOffset.Store(s.fedBytes.Load())

	screen, err := s.grid.ActiveScreen()
	if err != nil {
		return nil, nil, 0, 0, err
	}
	var out []byte
	if screen == libghostty.ScreenAlternate {
		sh, err := formatVT(s.shadow)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		rt, err := formatVT(s.grid)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		out = append(append(sh, "\x1b[?1049h"...), rt...)
	} else {
		out, err = formatVT(s.grid)
		if err != nil {
			return nil, nil, 0, 0, err
		}
	}

	var cont []byte
	if g, err := s.grid.VTGround(); err == nil && !g {
		// 切点落在序列中间：把未完成序列的字节作为实时流前缀补发
		//（U0-8a 验证过，浏览器端与 worker 端状态一致）。
		cont, _ = s.grid.Continuation()
	}
	return out, cont, s.curCols, s.curRows, nil
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

func (s *session) shutdown() {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
}

func (s *session) run() {
	mainDone := make(chan *int, 1)
	base := s.cmd.Process.Pid
	go func() {
		err := s.cmd.Wait()
		var code *int
		if state := s.cmd.ProcessState; state != nil {
			status := state.Sys().(syscall.WaitStatus)
			switch {
			case status.Exited():
				value := status.ExitStatus()
				code = &value
			case status.Signaled():
				value := 128 + int(status.Signal())
				code = &value
			}
		} else {
			log.Printf("等待目标进程失败: %v", err)
		}
		mainDone <- code
	}()

	outputDone := make(chan struct{})
	go func() {
		s.readLoop()
		close(outputDone)
	}()

	reason := "shell-exit"
	if s.cli {
		reason = "command-exit"
	}
	var code *int
	select {
	case code = <-mainDone:
		if code == nil {
			reason = "worker-error"
		}
	case <-s.stop:
		reason = "killed"
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
		signalDescendants(base, syscall.SIGTERM)
		select {
		case code = <-mainDone:
		case <-time.After(killGrace):
			_ = s.cmd.Process.Kill()
			code = <-mainDone
		}
	}
	s.ending.Store(true)

	// The main child has already been waited for. Any remaining children
	// are adopted descendants, including jobs that still hold the slave PTY.
	select {
	case <-outputDone:
	case <-time.After(100 * time.Millisecond):
	}
	reapTree(base)
	_ = s.ptmx.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	<-outputDone
	_ = s.ptmx.Close()
	s.finish(exitMsg{Reason: reason, ExitCode: code})
}

func (s *session) finish(end exitMsg) {
	s.ending.Store(true)

	s.viewersMu.Lock()
	s.end = end
	for v := range s.viewers {
		close(v.queue)
	}
	s.viewersMu.Unlock()

	flushed := make(chan struct{})
	go func() {
		s.viewerWG.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(frameBudget + exitWriteBudget):
		s.viewersMu.Lock()
		for v := range s.viewers {
			_ = v.conn.close()
		}
		s.viewersMu.Unlock()
		<-flushed
	}

	s.gridMu.Lock()
	s.grid.Close()
	s.shadow.Close()
	s.grid, s.shadow = nil, nil
	close(s.rec.queue)
	close(s.queryQueue)
	s.gridMu.Unlock()
	<-s.rec.done
	close(s.done)
}

func (s *session) removeViewer(v *viewer) {
	s.viewersMu.Lock()
	delete(s.viewers, v)
	s.viewersMu.Unlock()
	// The reader clears the owner after its last input. The output pump
	// never waits on the PTY input lock when dropping a viewer.
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

const (
	reapPoll      = 50 * time.Millisecond
	reapAfterKill = 500 * time.Millisecond
)

// Shell 退出时，过继的作业归 worker。进程组是快速路径，worker 后代才是回收范围。
func signalDescendants(base int, sig syscall.Signal) {
	tree := descendants(os.Getpid())
	for _, pid := range tree {
		if group, err := syscall.Getpgid(pid); err == nil && group == base {
			_ = syscall.Kill(-base, sig)
			break
		}
	}
	for _, pid := range tree {
		_ = syscall.Kill(pid, sig)
	}
}

// reapTree is called after the main waiter returns.
func reapTree(base int) {
	wait := func(deadline time.Time, killing bool) bool {
		for time.Now().Before(deadline) {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			switch {
			case errors.Is(err, syscall.ECHILD):
				return true
			case errors.Is(err, syscall.EINTR), pid > 0:
				continue
			case err != nil:
				log.Printf("回收会话子进程失败: %v", err)
			case killing:
				// 继续覆盖等待期间过继到 worker 的后代。
				signalDescendants(base, syscall.SIGKILL)
			}
			time.Sleep(reapPoll)
		}
		return false
	}

	signalDescendants(base, syscall.SIGTERM)
	if wait(time.Now().Add(killGrace), false) {
		return
	}
	signalDescendants(base, syscall.SIGKILL)
	if !wait(time.Now().Add(reapAfterKill), true) {
		log.Printf("会话子进程未能在 SIGKILL 后 %v 内全部回收", reapAfterKill)
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
