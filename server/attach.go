package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	wsReadLimit = 1 << 20 // 大粘贴兜底；前端按 64KiB 主动切块
	wsChunk     = 64 << 10
	queueChunks = 256              // 实时流队列上限（背压：满则断开本次接入）
	initTimeout = 3 * time.Second // 首条尺寸的兜底时限（沿用 001 语义）
)

// wsConn 给 coder/websocket 加写入互斥与只关一次。
type wsConn struct {
	c      *websocket.Conn
	mu     sync.Mutex
	closed atomic.Bool
}

func (w *wsConn) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, typ, data)
}

// close 发关闭帧；之后的写入直接失败，读侧随之退出。
func (w *wsConn) close(code websocket.StatusCode, reason string) {
	if !w.closed.CompareAndSwap(false, true) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.Close(code, reason)
}

// consumer 是一次接入：独占位、有界的实时流队列与切点。
type consumer struct {
	ws        *wsConn
	queue     chan []byte
	cutOffset int64 // 重建覆盖到的流偏移；实时流从此之后接上
	closed    atomic.Bool
}

// closeWith 只关一次；ws 未挂上（升级失败窗口）时仅标记。
func (c *consumer) closeWith(code websocket.StatusCode, reason string) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	if c.ws != nil {
		c.ws.close(code, reason)
	}
}

// handleWS GET /api/ws/term/{id}。接入握手在 WS 升级之前完成（spec 003）：
// 不存在 404，已有消费者 409，都是 HTTP 响应，连接不升级。
func (m *manager) handleWS(w http.ResponseWriter, r *http.Request) {
	s, ok := m.byID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}

	c := &consumer{
		queue:     make(chan []byte, queueChunks),
		cutOffset: math.MaxInt64, // 重建完成前一切查询都由本进程代答
	}
	if !s.consumer.CompareAndSwap(nil, c) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "会话已有接入"})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 开发期经 vite 代理接入，Origin 与 Host 不一致是常态；单用户工具，跳过校验。
		InsecureSkipVerify: true,
	})
	if err != nil { // Accept 已自行写 HTTP 错误响应
		s.consumer.CompareAndSwap(c, nil)
		return
	}
	conn.SetReadLimit(wsReadLimit)
	c.ws = &wsConn{c: conn}

	s.serveAttach(r, c)
}

// serveAttach 升级后的接入流程：等首条尺寸（3s 兜底 query）→ 调整尺寸 →
// ready → 重建帧 → 切点后的实时流。帧序见 spec 003「帧协议」。
func (s *session) serveAttach(r *http.Request, c *consumer) {
	wsc := c.ws
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := func() { s.consumer.CompareAndSwap(c, nil) }

	// 读侧：文本帧 = resize 控制 JSON；二进制帧 = 终端输入，直写 PTY。
	firstSize := make(chan [2]uint16, 1)
	go func() {
		defer cancel()
		for {
			mt, data, err := wsc.c.Read(ctx)
			if err != nil {
				return
			}
			if mt == websocket.MessageText {
				var ctl struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if json.Unmarshal(data, &ctl) != nil || ctl.Type != "resize" ||
					ctl.Cols < 2 || ctl.Cols > 500 || ctl.Rows < 2 || ctl.Rows > 200 {
					continue
				}
				select {
				case firstSize <- [2]uint16{ctl.Cols, ctl.Rows}:
				default:
					_ = s.resize(ctl.Cols, ctl.Rows)
				}
				continue
			}
			if s.ptmx != nil && len(data) > 0 {
				_, _ = s.ptmx.Write(data)
			}
		}
	}()

	// 首条尺寸：3s 未到按 query 兜底（计时归属 server，沿用 001 语义）。
	cols, rows := s.waitFirstSize(ctx, firstSize, r)
	if err := s.resize(cols, rows); err != nil {
		c.closeWith(1011, "resize failed")
		return
	}

	// ready 先于任何输出帧。
	if err := wsc.write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
		release()
		return
	}

	// 重建：滚动历史 + 活动屏一次 formatter 调用（U0-2 定案），经现有二进制帧
	// 进入 wterm 的 write 入口。
	out, cont, err := s.rebuild()
	if err != nil {
		c.closeWith(1011, "rebuild failed")
		return
	}
	for i := 0; i < len(out); i += wsChunk {
		end := min(i+wsChunk, len(out))
		if err := wsc.write(ctx, websocket.MessageBinary, out[i:end]); err != nil {
			release()
			return
		}
	}
	// 切点落在序列中间时，先补未完成序列的前缀（U0-8a 验证的兜底），
	// 再开始实时流——实时流从切点起无缝衔接。
	if len(cont) > 0 {
		if err := wsc.write(ctx, websocket.MessageBinary, cont); err != nil {
			release()
			return
		}
	}

	// 泵：把切点之后的实时输出送入浏览器。写失败（含对端断开、慢消费者触发
	// 的 4001 关闭）即退出并释放独占位。
	for {
		select {
		case chunk := <-c.queue:
			if err := wsc.write(ctx, websocket.MessageBinary, chunk); err != nil {
				release()
				return
			}
		case <-ctx.Done():
			release()
			return
		}
	}
}

// waitFirstSize 等首条有效尺寸消息；3s 未到按 URL query 兜底（缺省 80×24）。
func (s *session) waitFirstSize(ctx context.Context, firstSize chan [2]uint16, r *http.Request) (uint16, uint16) {
	cols, rows := uint16(gridCols), uint16(gridRows)
	if v, err := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16); err == nil && v >= 2 && v <= 500 {
		cols = uint16(v)
	}
	if v, err := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16); err == nil && v >= 2 && v <= 200 {
		rows = uint16(v)
	}
	select {
	case sz := <-firstSize:
		return sz[0], sz[1]
	case <-time.After(initTimeout):
		return cols, rows
	case <-ctx.Done():
		return cols, rows
	}
}
