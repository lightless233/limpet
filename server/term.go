// 终端会话：PTY 的创建、WS 桥接与生命周期管理。
// 本阶段为直接 PTY（无 tmux），WS 断开即结束进程；
// 换设备接续能力由后续接入 tmux 提供。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

type termSession struct {
	id   string
	cmd  *exec.Cmd
	ptmx *os.File

	mu      sync.Mutex
	claimed bool
	// closing 在处理器退出清理时置位，阻止超时兜底再启动 Shell。
	closing bool
	// done 由回收 goroutine 在进程退出后关闭；spawn 前永不关闭。
	done chan struct{}
	// ptyReady 在 spawn 成功时投递 PTY，供转发 goroutine 等待。
	ptyReady chan *os.File
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*termSession{}
)

// pendingSessionTTL 是预留会话（未接入）的存活时间。
const pendingSessionTTL = 30 * time.Second

// initTimeout 是接入后等待首条尺寸消息的时限。
// URL query 里的尺寸在握手期间可能已经过期（窗口变化发生在连接建立
// 前后之间），因此真正的启动尺寸以连接建立后的首条 resize 消息为准；
// 超时未收到才按 query 兜底尺寸启动。
const initTimeout = 3 * time.Second

// maxInputMessageBytes 是单条 WS 输入消息的读取上限。
// coder/websocket 默认 32 KiB：大段粘贴会作为一条消息到达，
// 超限会让连接进入错误态断开，并连带结束本阶段的 Shell。
// 前端会按 64 KiB 主动切块，1 MiB 只是兜底。
const maxInputMessageBytes = 1 << 20

func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shellPath() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/bash"
}

// terminalEnviron 在服务进程环境的基础上覆盖终端相关变量。
// 服务自身的 TERM 不可信（可能是 dumb 或缺失）：PTY 子进程需要真实的
// 终端类型，否则 zsh 的光标位置检测（u7）等能力失效。
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

// POST /api/term —— 预留终端会话，返回终端 ID。
// Shell 不在此刻 spawn：接入后也先等首条尺寸消息再启动（见 handleTermWS），
// 保证 Shell 从第一个字节起就按连接建立后的真实尺寸排版。
func handleTermCreate(w http.ResponseWriter, r *http.Request) {
	id := newSessionID()
	s := &termSession{
		id:       id,
		done:     make(chan struct{}),
		ptyReady: make(chan *os.File, 1),
	}

	sessionsMu.Lock()
	sessions[id] = s
	sessionsMu.Unlock()

	// 无人接入的预留会话定时回收，防止 map 泄漏。
	// 与接入占用同锁（sessionsMu）检查 claimed，避免删掉刚好在接入中的会话。
	time.AfterFunc(pendingSessionTTL, func() {
		sessionsMu.Lock()
		if s, ok := sessions[id]; ok {
			s.mu.Lock()
			claimed := s.claimed
			s.mu.Unlock()
			if !claimed {
				delete(sessions, id)
			}
		}
		sessionsMu.Unlock()
	})

	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// spawnShell 创建 PTY 并启动 Shell，size 来自首条尺寸消息或兜底值。
// 调用方需持有 s.mu。
func spawnShell(s *termSession, size pty.Winsize) error {
	cmd := exec.Command(shellPath())
	cmd.Env = terminalEnviron()

	ptmx, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return err
	}

	s.cmd = cmd
	s.ptmx = ptmx
	s.ptyReady <- ptmx

	// 进程退出后回收会话与 PTY。
	go func() {
		_ = cmd.Wait()
		_ = ptmx.Close()
		sessionsMu.Lock()
		delete(sessions, s.id)
		sessionsMu.Unlock()
		close(s.done)
	}()
	return nil
}

// GET /api/ws/term/{id}?cols=&rows= —— 接入终端会话。
// 帧约定：二进制帧 = 终端输入输出原样转发；文本帧 = 控制 JSON
// （客户端→resize；服务端→ready）。
//
// 启动时机：Accept 成功后并不立即 spawn，而是等首条有效 resize 消息——
// URL query 里的尺寸在握手期间可能已过期，按它启动会让 Shell 用旧宽度
// 排出首屏（% 伪影）。超时未收到尺寸则按 query 兜底尺寸启动；等待期间
// 到达的二进制输入以 1008 拒绝（前端以 ready 为输入闸门）。
// spawn 成功后服务端先发 ready 再转发输出，客户端据此区分「已连接」
// 与「Shell 启动失败」（1011）。
func handleTermWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 查找与占用必须在 sessionsMu 下原子完成：TTL 回收持同一锁检查
	// claimed，分离两步会让回收删掉一个刚好在接入中的会话。
	sessionsMu.Lock()
	s := sessions[id]
	if s == nil {
		sessionsMu.Unlock()
		http.NotFound(w, r)
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

	// 兜底尺寸：客户端迟迟不发尺寸（如非浏览器客户端）时的启动值。
	fallback := pty.Winsize{Rows: 24, Cols: 80}
	if v := queryUint(r, "cols"); v > 0 {
		fallback.Cols = v
	}
	if v := queryUint(r, "rows"); v > 0 {
		fallback.Rows = v
	}

	ctx := r.Context()

	// 启动入口：只在未启动且未关闭时 spawn，由主循环（首条尺寸消息）
	// 与超时兜底定时器共用，s.mu 串行化。
	spawnNow := func(sz pty.Winsize) error {
		if s.closing || s.ptmx != nil {
			return nil
		}
		return spawnShell(s, sz)
	}
	spawnFailed := func(where string, err error) {
		// 已占用但 spawn 失败：不再有人能接入成功，删除条目，
		// 否则 claimed=true 会让 TTL 回收跳过，条目永久泄漏。
		sessionsMu.Lock()
		delete(sessions, id)
		sessionsMu.Unlock()
		log.Printf("term %s spawn(%s): %v", id, where, err)
		c.Close(websocket.StatusInternalError, "spawn failed")
	}

	// 初始化超时：迟迟等不到尺寸消息时按兜底尺寸启动，避免永久等待。
	time.AfterFunc(initTimeout, func() {
		s.mu.Lock()
		err := spawnNow(fallback)
		s.mu.Unlock()
		if err != nil {
			spawnFailed("timeout", err)
		}
	})

	// Shell 退出（会话结束）时主动关闭 WS。否则主循环会一直堵在
	// c.Read 等客户端输入：会话已从 map 移除、进程已死，连接却还挂着，
	// 客户端永远停在「已连接」。ctx.Done() 保证处理器返回后监视器退出。
	go func() {
		select {
		case <-s.done:
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
				s.mu.Lock()
				err := spawnNow(pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				if err == nil && s.ptmx != nil {
					_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				}
				s.mu.Unlock()
				if err != nil {
					spawnFailed("resize", err)
					return
				}
			}
			continue
		}
		// 二进制输入：PTY 未启动属于协议违规（前端以 ready 为输入闸门），
		// 静默缓冲会让调用方把残缺输入当成完整输入，这里以 1008 明确拒绝，
		// 并走未启动会话的清理路径回收条目。
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

	// 清理：先关闸（同锁串行化，阻止超时兜底再启动），再按是否已启动回收。
	// 已启动：终止进程并等待回收 goroutine 删除条目、关闭 done。
	// 未启动（从未收到尺寸 / 协议违规 / 握手后立即断开）：条目已占用且
	// 无人会再回收，必须在此删除，否则 claimed=true 会让 TTL 跳过它。
	s.mu.Lock()
	s.closing = true
	cmd, ptmx := s.cmd, s.ptmx
	s.mu.Unlock()

	c.Close(websocket.StatusNormalClosure, "session ended")
	if ptmx != nil {
		_ = cmd.Process.Kill()
		<-s.done
	} else {
		sessionsMu.Lock()
		delete(sessions, id)
		sessionsMu.Unlock()
	}
	log.Printf("term %s closed", id)
}

func queryUint(r *http.Request, key string) uint16 {
	s := r.URL.Query().Get(key)
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0
	}
	return uint16(n)
}
