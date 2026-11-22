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
	done    chan struct{}
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*termSession{}
)

// pendingSessionTTL 是预留会话（未接入）的存活时间。
const pendingSessionTTL = 30 * time.Second

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
// Shell 不在此刻 spawn：PTY 必须以接入方的真实尺寸创建，否则 Shell 在
// 旧尺寸下写出的首屏（提示符、PROMPT_SP 标记行）不会回流。真正的
// spawn 在首次 WS 接入时按 query/控制消息给出的尺寸进行。
func handleTermCreate(w http.ResponseWriter, r *http.Request) {
	id := newSessionID()
	s := &termSession{id: id, done: make(chan struct{})}

	sessionsMu.Lock()
	sessions[id] = s
	sessionsMu.Unlock()

	// 无人接入的预留会话定时回收，防止 map 泄漏。
	time.AfterFunc(pendingSessionTTL, func() {
		s.mu.Lock()
		claimed := s.claimed
		s.mu.Unlock()
		if !claimed {
			sessionsMu.Lock()
			delete(sessions, id)
			sessionsMu.Unlock()
		}
	})

	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// spawnShell 创建 PTY 并启动 Shell，size 来自接入请求。
func spawnShell(s *termSession, size pty.Winsize) error {
	cmd := exec.Command(shellPath())
	cmd.Env = terminalEnviron()

	ptmx, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return err
	}

	s.cmd = cmd
	s.ptmx = ptmx

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
// 帧约定：二进制帧 = 终端输入输出原样转发；文本帧 = 控制 JSON（目前仅 resize）。
// 本阶段一个会话同一时间只允许一个连接。
func handleTermWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sessionsMu.Lock()
	s := sessions[id]
	sessionsMu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	if s.claimed {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session already attached"})
		return
	}
	s.claimed = true
	s.mu.Unlock()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
		return
	}
	c.SetReadLimit(maxInputMessageBytes)
	log.Printf("term %s attached", id)

	// 初始窗口尺寸（缺省 80x24）。Shell 以该尺寸 spawn，保证首屏排版正确。
	size := pty.Winsize{Rows: 24, Cols: 80}
	if v := queryUint(r, "cols"); v > 0 {
		size.Cols = v
	}
	if v := queryUint(r, "rows"); v > 0 {
		size.Rows = v
	}
	if err := spawnShell(s, size); err != nil {
		log.Printf("term %s spawn: %v", id, err)
		c.Close(websocket.StatusInternalError, "spawn failed")
		return
	}

	ctx := r.Context()

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

	// PTY → WS
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := s.ptmx.Read(buf)
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

	// WS → PTY（并处理控制消息）
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
				_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
			}
			continue
		}
		if _, err := s.ptmx.Write(data); err != nil {
			break
		}
	}

	// 本阶段无持久化：WS 断开即结束会话。
	c.Close(websocket.StatusNormalClosure, "session ended")
	_ = s.cmd.Process.Kill()
	<-s.done
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
