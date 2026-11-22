// 终端会话：tmux 宿主之上的 WS 桥接与生命周期管理。
// 一个 Limpet 会话 = 一个 tmux session（1:1，会话 ID 即 tmux 会话名）。
// tmux（独立 socket -L limpet）是常驻状态的唯一事实来源：
// WS 断开只结束接入视图（tmux 客户端进程），会话与 Shell 常驻；
// 显式关闭（close）或 Shell 内退出才终结会话。tmux 的重绘承担换设备接续。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

type termSession struct {
	// id 即 tmux 会话名；rename 时在 s.mu 下更新。
	id   string
	cmd  *exec.Cmd
	ptmx *os.File

	mu      sync.Mutex
	claimed bool
	// ptyReady 在客户端 spawn 成功时投递 PTY，供转发 goroutine 等待。
	ptyReady chan *os.File
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*termSession{}
)

// initTimeout 是接入后等待首条尺寸消息的时限。
// URL query 里的尺寸在握手期间可能已经过期（窗口变化发生在连接建立
// 前后之间），因此真正的启动尺寸以连接建立后的首条 resize 消息为准；
// 超时未收到才按 query 兜底尺寸启动。
const initTimeout = 3 * time.Second

// maxInputMessageBytes 是单条 WS 输入消息的读取上限。
// coder/websocket 默认 32 KiB：大段粘贴会作为一条消息到达，
// 超限会让连接进入错误态断开。前端按 64 KiB 主动切块，1 MiB 只是兜底。
const maxInputMessageBytes = 1 << 20

var (
	errConnectionClosed = errors.New("connection closed")
	errSessionClosed    = errors.New("session closed")
)

func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// terminalEnviron 为 tmux 客户端进程覆盖终端相关变量。
// 服务自身的 TERM 不可信（可能是 dumb 或缺失）。
func terminalEnviron() []string {
	overrides := map[string]string{
		"TERM":       "xterm-256color",
		"COLORTERM":  "truecolor",
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[k]; ok {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// tmuxUnavailableResponse 统一处理 tmux 环境不可用（未安装）。
func tmuxUnavailableResponse(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "tmux 不可用：请确认 tmux 已安装",
	})
}

// tmuxError 把 tmux 命令失败转成 HTTP 响应：环境级失败 503，其余 500。
func tmuxError(w http.ResponseWriter, err error) {
	msg := err.Error()
	status := http.StatusInternalServerError
	if strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting") ||
		strings.Contains(msg, "No such file") {
		status = http.StatusServiceUnavailable
	}
	log.Printf("tmux error: %v", err)
	writeJSON(w, status, map[string]string{"error": msg})
}

// POST /api/term —— 创建会话：tmux 会话与 Shell 立即启动，出现在列表。
// body（可选）: {"name": "..."}；缺省自动生成。重名 409，非法字符 400。
func handleTermCreate(w http.ResponseWriter, r *http.Request) {
	if tmuxMissing {
		tmuxUnavailableResponse(w)
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if b, err := io.ReadAll(io.LimitReader(r.Body, 4096)); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		_ = json.Unmarshal(b, &body) // 解析失败按无名称处理
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = newSessionID()
	}
	if !tmuxSessionNameValid(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "名称不合法：不能包含 : 或 .，长度不超过 100 字符",
		})
		return
	}
	if tmuxSessionExists(tmuxConfPath, name) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "同名会话已存在"})
		return
	}
	if err := tmuxCreate(tmuxConfPath, name); err != nil {
		tmuxError(w, err)
		return
	}

	s := &termSession{id: name, ptyReady: make(chan *os.File, 1)}
	sessionsMu.Lock()
	sessions[name] = s
	sessionsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"id": name})
}

// GET /api/term —— 会话列表（实时来自 tmux，创建时间倒序）。
// 对账：tmux 中已消失的条目（Shell 内退出 / 外部 kill）顺带移除。
func handleTermList(w http.ResponseWriter, r *http.Request) {
	if tmuxMissing {
		tmuxUnavailableResponse(w)
		return
	}
	list, err := tmuxList(tmuxConfPath)
	if err != nil {
		tmuxError(w, err)
		return
	}

	sessionsMu.Lock()
	live := make(map[string]bool, len(list))
	for _, s := range list {
		live[s.Name] = true
	}
	for id := range sessions {
		if !live[id] {
			delete(sessions, id)
		}
	}
	sessionsMu.Unlock()

	type item struct {
		ID       string `json:"id"`
		Created  string `json:"created"`
		Attached bool   `json:"attached"`
	}
	items := make([]item, 0, len(list))
	for _, s := range list {
		items = append(items, item{
			ID:       s.Name,
			Created:  time.Unix(s.Created, 0).UTC().Format(time.RFC3339),
			Attached: s.Clients > 0,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created > items[j].Created })
	writeJSON(w, http.StatusOK, items)
}

// POST /api/term/{id}/close —— 显式关闭：kill-session，Shell 终止，列表移除。
// 有客户端接入时，客户端随会话结束退出并由回收协程删除条目。
func handleTermClose(w http.ResponseWriter, r *http.Request) {
	if tmuxMissing {
		tmuxUnavailableResponse(w)
		return
	}
	id := r.PathValue("id")
	sessionsMu.Lock()
	s := sessions[id]
	sessionsMu.Unlock()
	if s == nil && !tmuxSessionExists(tmuxConfPath, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	if err := tmuxKill(tmuxConfPath, id); err != nil {
		tmuxError(w, err)
		return
	}
	sessionsMu.Lock()
	delete(sessions, id)
	sessionsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// POST /api/term/{id}/rename —— 重命名。对活跃客户端透明（tmux 内部完成）。
func handleTermRename(w http.ResponseWriter, r *http.Request) {
	if tmuxMissing {
		tmuxUnavailableResponse(w)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if b, err := io.ReadAll(io.LimitReader(r.Body, 4096)); err == nil {
		_ = json.Unmarshal(b, &body)
	}
	name := strings.TrimSpace(body.Name)
	if !tmuxSessionNameValid(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "名称不合法：不能包含 : 或 .，长度不超过 100 字符",
		})
		return
	}
	if name == id {
		writeJSON(w, http.StatusOK, map[string]string{"id": name})
		return
	}

	sessionsMu.Lock()
	s := sessions[id]
	sessionsMu.Unlock()
	if s == nil && !tmuxSessionExists(tmuxConfPath, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	if tmuxSessionExists(tmuxConfPath, name) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "同名会话已存在"})
		return
	}
	if err := tmuxRename(tmuxConfPath, id, name); err != nil {
		tmuxError(w, err)
		return
	}

	sessionsMu.Lock()
	if entry, ok := sessions[id]; ok {
		entry.mu.Lock()
		entry.id = name
		entry.mu.Unlock()
		delete(sessions, id)
		sessions[name] = entry
	}
	sessionsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"id": name})
}

// GET /api/ws/term/{id}?cols=&rows= —— 接入终端会话（创建与接续同一入口）。
// 帧约定：二进制帧 = 终端输入输出原样转发；文本帧 = 控制 JSON
// （客户端→resize；服务端→ready）。
//
// 启动时机：Accept 成功后并不立即 spawn，而是等首条有效 resize 消息——
// URL query 里的尺寸在握手期间可能已过期。超时未收到尺寸则按 query
// 兜底尺寸启动；等待期间到达的二进制输入以 1008 拒绝（前端以 ready
// 为输入闸门）。spawn 成功后服务端先发 ready 再转发输出。
// 会话不存在（未创建或 Shell 内退出）→ 404；已有客户端 → 409。
// Go 重启后 map 为空但 tmux 会话仍在：接入时按 tmux 状态重建条目。
func handleTermWS(w http.ResponseWriter, r *http.Request) {
	if tmuxMissing {
		tmuxUnavailableResponse(w)
		return
	}
	id := r.PathValue("id")

	sessionsMu.Lock()
	s := sessions[id]
	sessionsMu.Unlock()
	if s == nil {
		// map 无条目但 tmux 会话仍在（服务重启 / 前端直连旧 ID）→ 重建条目。
		if !tmuxSessionExists(tmuxConfPath, id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
			return
		}
		s = &termSession{id: id, ptyReady: make(chan *os.File, 1)}
		sessionsMu.Lock()
		if existing, ok := sessions[id]; ok {
			s = existing
		} else {
			sessions[id] = s
		}
		sessionsMu.Unlock()
	}

	// 查找与占用必须在 sessionsMu 下原子完成：列表对账持同一锁删除
	// 已消失的条目，分离两步会引入竞态。
	sessionsMu.Lock()
	s = sessions[id]
	if s == nil {
		sessionsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	s.mu.Lock()
	if s.claimed {
		s.mu.Unlock()
		sessionsMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session already attached"})
		return
	}
	s.claimed = true
	s.mu.Unlock()
	sessionsMu.Unlock()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
		return
	}
	c.SetReadLimit(maxInputMessageBytes)
	log.Printf("term %s attached", id)

	// 兜底尺寸：客户端迟迟不发尺寸时的启动值。
	fallback := pty.Winsize{Rows: 24, Cols: 80}
	if v := queryUint(r, "cols"); v > 0 {
		fallback.Cols = v
	}
	if v := queryUint(r, "rows"); v > 0 {
		fallback.Rows = v
	}

	ctx := r.Context()

	// closing 是本连接的局部状态（atomic）：连接结束后阻止兜底定时器
	// 再启动客户端。它不能挂在会话上——会话跨连接常驻，上一个连接的
	// closing 会卡死后续所有接入的启动。
	var closing atomic.Bool

	// done 是本连接的客户端进程生命周期信号：进程退出（任何原因）时由
	// 回收协程关闭，监视器据此结束当前连接。
	done := make(chan struct{})

	// 捕获上下文：断线时立即取消 capture-pane，不必等处理器返回。
	captureCtx, cancelCapture := context.WithCancel(context.Background())
	defer cancelCapture()

	// 连接结束原因：正常断开为 Normal；1008/1011 分支先行设置，
	// 统一由清理路径发送关闭帧（避免在分支内先行 c.Close 抢跑）。
	exitCode := websocket.StatusNormalClosure
	exitReason := "session ended"

	// 启动入口：只在未启动且未关闭时 spawn tmux 客户端，
	// 由主循环（首条尺寸消息）与超时兜底定时器共用。
	// 两个调用方可能并发进入，检查-捕获-复查-启动必须整体串行化
	// （spawnMu），否则双 spawn 会让 replayCh/ptyReady 管道卡死。
	// spawn 前捕获 pane 历史（spec 001：接入回放），ready 后先行回放，
	// 让浏览器滚动缓冲拥有断开前的内容；捕获失败降级为无回放。
	// 捕获绑定连接上下文（可取消），返回后在锁内复查 closing：
	// 清理要么阻止启动，要么拿到刚启动的客户端并将其收掉。
	var spawnMu sync.Mutex
	replayCh := make(chan []byte, 1)
	spawnNow := func(sz pty.Winsize) error {
		spawnMu.Lock()
		defer spawnMu.Unlock()
		if closing.Load() || s.ptmx != nil {
			return nil
		}
		s.mu.Lock()
		sessionID := s.id
		s.mu.Unlock()
		// 会话可能在查找与首条尺寸消息之间被关闭：接入路径绝不复活
		// 会话，存在性消失则清条目并以 1011 结束本次接入。
		if !tmuxSessionExists(tmuxConfPath, sessionID) {
			sessionsMu.Lock()
			delete(sessions, sessionID)
			sessionsMu.Unlock()
			return errSessionClosed
		}
		out, cerr := runTmuxCtx(captureCtx, tmuxConfPath, "capture-pane", "-p", "-e", "-S", "-10000", "-t", sessionID)
		if closing.Load() {
			return errConnectionClosed
		}
		// 每次接入必交付一个结果（失败=空历史），输出协程才不会在
		// replayCh 上永久阻塞——历史取不到时实时输出必须继续。
		if cerr == nil {
			replayCh <- []byte(out)
		} else {
			log.Printf("term %s capture: %v", id, cerr)
			replayCh <- nil
		}
		return spawnTmuxClient(s, sz, done)
	}

	var initTimer *time.Timer
	// endConnection 是所有退出路径的统一收口：标记关闭、停兜底定时器、
	// 取消捕获、发送关闭帧。资源回收在 tail 的 spawnMu 段进行。
	endConnection := func() {
		closing.Store(true)
		if initTimer != nil {
			initTimer.Stop()
		}
		cancelCapture()
		c.Close(exitCode, exitReason)
	}
	spawnFailed := func(where string, err error) {
		if errors.Is(err, errConnectionClosed) {
			return
		}
		log.Printf("term %s spawn(%s): %v", id, where, err)
		exitCode = websocket.StatusInternalError
		exitReason = "spawn failed"
		endConnection()
	}

	// 初始化超时：迟迟等不到尺寸消息时按兜底尺寸启动，避免永久等待。
	// 不要持有 s.mu 调 spawnNow（其内部会再取 s.mu，非重入锁会自死锁）。
	initTimer = time.AfterFunc(initTimeout, func() {
		if err := spawnNow(fallback); err != nil {
			spawnFailed("timeout", err)
		}
	})

	// tmux 会话消失（Shell 内退出 / kill-session）时客户端进程退出，
	// 回收协程关闭 done，这里据此结束当前连接。
	go func() {
		select {
		case <-done:
			c.Close(websocket.StatusNormalClosure, "session ended")
		case <-ctx.Done():
		}
	}()

	// PTY → WS：PTY 就绪后先发 ready（前端据此显示「已连接」），
	// 再开始转发输出，保证客户端不会把首屏输出当作无效帧丢弃。
	go func() {
		var ptmx *os.File
		select {
		case ptmx = <-s.ptyReady:
		case <-ctx.Done():
			return
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
			return
		}
		// 接入回放：先于实时输出发送 pane 历史（spec 001）。
		// 结果由 spawnNow 保证先于 ptyReady 交付；select 防御性地让
		// 等待随连接取消。
		var replay []byte
		select {
		case replay = <-replayCh:
		case <-ctx.Done():
			return
		}
		for i := 0; i < len(replay); i += 64 * 1024 {
			end := i + 64 * 1024
			if end > len(replay) {
				end = len(replay)
			}
			if werr := c.Write(ctx, websocket.MessageBinary, replay[i:end]); werr != nil {
				return
			}
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WS → PTY（控制消息 + 输入）
	for {
		msgType, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if msgType == websocket.MessageText {
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				// spawnNow 绝不能在持有 s.mu 时调用（其内部会再取 s.mu，
				// 非重入锁自死锁）——与上方定时器路径同理。
				err := spawnNow(pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				if err != nil {
					if !errors.Is(err, errConnectionClosed) {
						spawnFailed("resize", err)
					}
					break
				}
				s.mu.Lock()
				if s.ptmx != nil {
					_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				}
				s.mu.Unlock()
			}
			continue
		}
		// 二进制输入：PTY 未启动属于协议违规（前端以 ready 为输入闸门），
		// 以 1008 明确拒绝。
		s.mu.Lock()
		if s.ptmx == nil {
			s.mu.Unlock()
			c.Close(websocket.StatusPolicyViolation, "terminal not ready")
			break
		}
		_, werr := s.ptmx.Write(data)
		s.mu.Unlock()
		if werr != nil {
			break
		}
	}

	// 清理：停掉兜底定时器并关闸，再处理客户端进程。
	// 条目永不在此删除：会话常驻，断开只是 detach——
	// 客户端退出后的条目去向（保留/删除）由回收协程按 tmux 会话存活判定。
	closing.Store(true)
	initTimer.Stop()

	s.mu.Lock()
	cmd, ptmx := s.cmd, s.ptmx
	s.mu.Unlock()

	c.Close(websocket.StatusNormalClosure, "session ended")
	if ptmx != nil {
		_ = cmd.Process.Kill()
		<-done
	} else {
		// 尚未启动客户端：复位接入态，会话等待下一次接入。
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
	}
	log.Printf("term %s closed", id)
}

// spawnTmuxClient 在 PTY 中启动 tmux 客户端（attach 到既有会话）。
// 调用方需持有 s.mu。
// 客户端进程退出后的条目去向由 tmux 会话存活判定：
//   会话仍在（WS 断开的 detach）→ 保留条目、复位接入态，等待重连；
//   会话已消失（Shell 退出 / kill-session）→ 删除条目。
// done 在任何退出原因下都会关闭，驱动监视器结束当前连接。
func spawnTmuxClient(s *termSession, size pty.Winsize, done chan struct{}) error {
	cmd := exec.Command("tmux", tmuxAttachArgs(tmuxConfPath, s.id)...)
	cmd.Env = terminalEnviron()

	ptmx, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return err
	}

	s.cmd = cmd
	s.ptmx = ptmx
	s.ptyReady <- ptmx

	go func() {
		_ = cmd.Wait()
		// 清空客户端引用：会话常驻，下次接入需要重新 spawn；
		// 残留的旧 PTY 会让 spawnNow 误判「已启动」。
		s.mu.Lock()
		id := s.id
		s.cmd = nil
		s.ptmx = nil
		s.mu.Unlock()
		gone := !tmuxSessionExists(tmuxConfPath, id)
		sessionsMu.Lock()
		if gone {
			delete(sessions, id)
		}
		sessionsMu.Unlock()
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
		close(done)
	}()
	return nil
}

func queryUint(r *http.Request, key string) uint16 {
	s := r.URL.Query().Get(key)
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0
	}
	return uint16(n)
}
