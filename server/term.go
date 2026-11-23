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
	id string

	mu      sync.Mutex
	claimed bool
}

// connState 是单条 WebSocket 接入的私有资源：attach 客户端进程、其 PTY
// 与退出信号。tmux 会话（termSession）跨连接常驻，绝不持有连接级资源——
// 否则旧连接的关闭握手结束后会从共享字段读到新连接的客户端并将其断开。
type connState struct {
	mu       sync.Mutex // 保护 cmd/ptmx 的发布与读取（启动协程 vs 读循环/清理）
	cmd      *exec.Cmd
	ptmx     *os.File
	ptyReady chan *os.File // spawn 成功后投递 PTY，转发协程在此等待
	done     chan struct{} // 客户端进程退出（任何原因）后关闭
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
		// 非空 body 必须是合法 JSON：静默忽略会让调用方拿到意料之外的
		// 随机名会话且无从察觉。
		if err := json.Unmarshal(b, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
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

	s := &termSession{id: name}
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
		s = &termSession{id: id}
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

	// 本连接的私有资源：客户端进程、PTY、就绪与退出信号。
	// 与会话字段彻底分离——旧连接的清理只会触碰自己的 connState。
	st := &connState{ptyReady: make(chan *os.File, 1), done: make(chan struct{})}

	// 捕获上下文：断线时立即取消 capture-pane，不必等处理器返回。
	captureCtx, cancelCapture := context.WithCancel(context.Background())
	defer cancelCapture()

	// 连接结束原因：正常断开为 Normal；1008/1011 分支先行设置，
	// 统一由清理路径发送关闭帧（避免在分支内先行 c.Close 抢跑）。
	exitCode := websocket.StatusNormalClosure
	exitReason := "session ended"

	// 启动流程：首条尺寸与兜底定时器都只「触发」它（幂等），真正的
	// 检查-捕获-启动在独立 goroutine 里执行——读循环必须保持能读，
	// 否则 capture 期间断线要等捕获结束才能被发现。
	// spawnMu 让启动与断线清理串行化：清理要么阻止启动（锁内复查
	// closing），要么拿到刚启动的客户端并将其收掉。
	// spawn 前捕获 pane 历史（spec 001：接入回放），ready 后先行回放；
	// 捕获失败降级为无回放（交付空结果，不阻塞输出协程）。
	var latestSize atomic.Pointer[pty.Winsize]
	var spawnStarted atomic.Bool
	var spawnMu sync.Mutex
	replayCh := make(chan []byte, 1)

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

	// 启动流程：首条尺寸与兜底定时器都只「触发」它（幂等），真正的
	// 检查-捕获-启动在独立 goroutine 里执行——读循环必须保持能读，
	// 否则 capture 期间断线要等捕获结束才能被发现。
	// spawnMu 让启动与断线清理串行化：清理要么阻止启动（锁内复查
	// closing），要么拿到刚启动的客户端并将其收掉。
	// spawn 前捕获 pane 历史（spec 001：接入回放），ready 后先行回放；
	// 捕获失败降级为无回放（交付空结果，不阻塞输出协程）。
	runSpawnFlow := func() error {
		spawnMu.Lock()
		defer spawnMu.Unlock()
		st.mu.Lock()
		started := st.ptmx != nil
		st.mu.Unlock()
		if closing.Load() || started {
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
		// 历史捕获：先按精确会话名解析活动 pane，再以 pane ID 捕获——
		// capture-pane 的 -t 是 target-pane，会话名恰好等于其他会话的
		// pane ID（%N）时会被解析到别人的 pane。
		paneID, err := tmuxActivePaneID(tmuxConfPath, sessionID)
		if err != nil {
			return err
		}
		out, cerr := runTmuxCtx(captureCtx, tmuxConfPath, "capture-pane", "-p", "-e", "-S", "-10000", "-E", "-1", "-t", paneID)
		if closing.Load() {
			return errConnectionClosed
		}
		// 每次接入必交付一个结果（失败=空历史），输出协程才不会在
		// replayCh 上永久阻塞——历史取不到时实时输出必须继续。
		// capture-pane 以 LF 结行，回放字节绕过 tty 的 onlcr，必须
		// 自行补 CR，否则历史行阶梯缩进。-E -1 只取可见屏之前的历史，
		// 可见屏由接入时的实时重绘覆盖。
		if cerr == nil {
			replayCh <- []byte(strings.ReplaceAll(out, "\n", "\r\n"))
		} else {
			log.Printf("term %s capture: %v", id, cerr)
			replayCh <- nil
		}
		sz := pty.Winsize{Rows: 24, Cols: 80}
		if p := latestSize.Load(); p != nil {
			sz = *p
		}
		return spawnTmuxClient(s, st, sz)
	}

	startSpawn := func() {
		if spawnStarted.Swap(true) {
			return
		}
		go func() {
			if err := runSpawnFlow(); err != nil {
				spawnFailed("spawn", err)
			}
		}()
	}

	// 初始化超时：迟迟等不到尺寸消息时按 URL query 兜底尺寸触发启动。
	initTimer = time.AfterFunc(initTimeout, func() {
		if latestSize.Load() == nil {
			fb := fallback
			latestSize.Store(&fb)
		}
		startSpawn()
	})

	// tmux 会话消失（Shell 内退出 / kill-session）时客户端进程退出，
	// 回收协程关闭 st.done，这里据此结束当前连接。
	go func() {
		select {
		case <-st.done:
			c.Close(websocket.StatusNormalClosure, "session ended")
		case <-ctx.Done():
		}
	}()

	// PTY → WS：PTY 就绪后先发 ready（前端据此显示「已连接」），
	// 再开始转发输出，保证客户端不会把首屏输出当作无效帧丢弃。
	go func() {
		var ptmx *os.File
		select {
		case ptmx = <-st.ptyReady:
		case <-ctx.Done():
			return
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
			return
		}
		// 接入回放：先于实时输出发送 pane 历史（spec 001）。
		// 结果由启动流程保证先于 ptyReady 交付；select 防御性地让
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
				// 记录最新尺寸并触发启动流程（幂等）。启动在异步
				// goroutine 里进行，读循环保持能读，断线即时可见。
				latestSize.Store(&pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				startSpawn()
				// 已启动的客户端直接应用新尺寸；启动中的由启动流程
				// 读取最新尺寸处理。
				st.mu.Lock()
				if st.ptmx != nil {
					_ = pty.Setsize(st.ptmx, &pty.Winsize{Rows: ctl.Rows, Cols: ctl.Cols})
				}
				st.mu.Unlock()
			}
			continue
		}
		// 二进制输入：PTY 未启动属于协议违规（前端以 ready 为输入闸门），
		// 以 1008 明确拒绝，由统一清理路径发送关闭帧并回收条目。
		st.mu.Lock()
		if st.ptmx == nil {
			st.mu.Unlock()
			exitCode = websocket.StatusPolicyViolation
			exitReason = "terminal not ready"
			break
		}
		_, werr := st.ptmx.Write(data)
		st.mu.Unlock()
		if werr != nil {
			break
		}
	}

	// 统一清理：标记关闭 → 停兜底 → 取消捕获 → 发关闭帧 →
	// 等待启动临界区结束 → 回收本连接的客户端资源（若已启动）→
	// 末尾集中释放接入占用。只触碰本连接的 connState：此刻共享字段
	// 上可能是后来接入的新连接，旧清理绝不能断开它。条目永不在此
	// 删除：会话常驻，断开只是 detach。
	endConnection()

	// spawnMu 必不可少：启动协程通过最后一次 closing 检查后、把 PTY
	// 发布进 connState 前有一段窗口（capture 之后的 fork/exec），没有
	// 这把锁清理会读到空资源提前释放占用，随后启动完成留下无主客户端。
	// 锁序保证清理要么阻止启动（启动协程锁内复查 closing 失败），
	// 要么拿到刚启动的客户端并将其收掉。
	spawnMu.Lock()
	st.mu.Lock()
	cmd, ptmx := st.cmd, st.ptmx
	st.mu.Unlock()
	spawnMu.Unlock()

	if ptmx != nil {
		_ = cmd.Process.Kill()
		_ = ptmx.Close()
		<-st.done
	}

	// 占用释放在清理末尾：关闭握手未完成前，新连接得到 409 而不是
	// 被旧清理误杀。cleanup 完成后占用人已不复存在，安全复位。
	s.mu.Lock()
	if s.claimed {
		s.claimed = false
	}
	s.mu.Unlock()
	log.Printf("term %s closed", id)
}


// spawnTmuxClient 在 PTY 中启动 tmux 客户端（attach 到既有会话），
// 资源发布到本次接入的 connState。
// 客户端退出协程只做两件事：报告退出（关闭 st.done，驱动监视器与
// 清理等待）与判定条目去向（tmux 会话已消失 → 删除条目）。
// 接入占用的释放集中在处理器清理末尾——提前释放会让旧连接的关闭
// 握手期间插入的新连接，在握手结束后被旧清理误杀。
func spawnTmuxClient(s *termSession, st *connState, size pty.Winsize) error {
	cmd := exec.Command("tmux", tmuxAttachArgs(tmuxConfPath, s.id)...)
	cmd.Env = terminalEnviron()

	ptmx, err := pty.StartWithSize(cmd, &size)
	if err != nil {
		return err
	}

	// 发布经 st.mu：读循环的 resize/输入与启动协程并发访问。
	st.mu.Lock()
	st.cmd = cmd
	st.ptmx = ptmx
	st.mu.Unlock()
	st.ptyReady <- ptmx

	go func() {
		_ = cmd.Wait()
		_ = ptmx.Close()
		s.mu.Lock()
		id := s.id
		s.mu.Unlock()
		gone := !tmuxSessionExists(tmuxConfPath, id)
		sessionsMu.Lock()
		if gone {
			delete(sessions, id)
		}
		sessionsMu.Unlock()
		close(st.done)
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
