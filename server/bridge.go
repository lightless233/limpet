package main

// server 侧：WS 接入 ↔ worker socket 的中继。
//
// 接入握手发生在 WS 升级之前（spec 003）：不存在 404、独占位被占 409、握手失败
// 500 + 原因，都是 HTTP 响应，连接不升级。升级后帧序 ready → 重建帧 → 实时流。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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

// probeWorker 探测一台 worker：拨号 → 握手 → 断开。不占独占位。
func probeWorker(path string, uuid string) (handshakeAck, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return handshakeAck{}, err
	}
	defer conn.Close()
	fc := newFrameConn(conn)
	_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: "probe", Proto: protocolVersion, UUID: uuid}))
	t, payload, err := fc.readFrame()
	if err != nil {
		return handshakeAck{}, err
	}
	if t != ftHandshakeAck {
		return handshakeAck{}, errors.New("握手应答类型异常")
	}
	var ack handshakeAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		return handshakeAck{}, err
	}
	return ack, nil
}

// killWorker 令 worker 回收会话并退出。返回前不等待回收完成（worker 回复 ok 后
// 自行 TERM → 兜底 KILL 整棵进程树、移除 socket、退出；最长 closeGrace 秒级）。
func killWorker(path string, uuid string) (handshakeAck, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return handshakeAck{}, err
	}
	defer conn.Close()
	fc := newFrameConn(conn)
	_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: "kill", Proto: protocolVersion, UUID: uuid}))
	t, payload, err := fc.readFrame()
	if err != nil {
		return handshakeAck{}, err
	}
	if t != ftHandshakeAck {
		return handshakeAck{}, errors.New("握手应答类型异常")
	}
	var ack handshakeAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		return handshakeAck{}, err
	}
	return ack, nil
}

// handleWS GET /api/ws/term/{id}。握手在升级之前完成（spec 003）。
func (m *manager) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	h := m.byIDLocked(id)
	m.mu.Unlock()
	if h == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	if h.busy.Load() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "会话已有接入"})
		return
	}

	// 与 worker 握手（占独占位）——成功之后才升级 WS。
	conn, err := net.DialTimeout("unix", h.socketPath, 2*time.Second)
	if err != nil {
		// socket 拨不通：worker 已死（stale），清理并按不存在处理。
		m.dropStale(id, h)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	fc := newFrameConn(conn)
	defer fc.close()
	_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: "attach", Proto: protocolVersion, UUID: id}))
	t, payload, err := fc.readFrame()
	if err != nil {
		m.dropStale(id, h)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	if t != ftHandshakeAck {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 握手应答异常"})
		return
	}
	var ack handshakeAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 握手应答解析失败"})
		return
	}
	if !ack.OK {
		if ack.Busy {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "会话已有接入"})
		} else {
			reason := ack.Reason
			if reason == "" {
				reason = "worker 拒绝接入"
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": reason})
		}
		return
	}
	if ack.UUID != id {
		// 身份不符（spec 验收 18）：不删除、不终止，如实报错。
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 身份与请求不符"})
		return
	}

	h.busy.Store(true)
	defer h.busy.Store(false)

	// 兜底尺寸（沿用 001 语义：URL query，缺省 80×24）。首条尺寸计时归属 server：
	// 3s 内客户端发来 resize 就用客户端的，否则按 query 发（spec 003 帧协议）。
	fbCols, fbRows := uint16(gridCols), uint16(gridRows)
	if v, err := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16); err == nil && v >= 2 && v <= 500 {
		fbCols = uint16(v)
	}
	if v, err := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16); err == nil && v >= 2 && v <= 200 {
		fbRows = uint16(v)
	}

	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 开发期经 vite 代理接入，Origin 与 Host 不一致是常态；单用户工具，跳过校验。
		InsecureSkipVerify: true,
	})
	if err != nil { // Accept 已自行写 HTTP 错误响应
		return
	}
	wsc.SetReadLimit(wsReadLimit)
	ww := &wsConn{c: wsc}

	relay(ww, fc, fbCols, fbRows)
}

// relay 把 WS 与 worker socket 双向中继。socket → WS 的翻译：
// OUTPUT → 二进制帧；READY → 文本帧 `{"type":"ready"}`；EXIT → 关闭码 1000。
// WS → socket：二进制 → INPUT；文本（resize）→ RESIZE。
// 慢消费者判定在 server：WS 写超时 5s → 关闭码 4001，worker 的 socket 随之关闭。
func relay(w *wsConn, fc *frameConn, fbCols, fbRows uint16) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 首条尺寸：客户端 resize 与 query 兜底竞争，CAS 保证只发一次。
	firstSent := atomic.Bool{}
	sendFirstSize := func(c, r uint16) {
		if firstSent.CompareAndSwap(false, true) {
			_ = fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: c, Rows: r}))
		}
	}
	go func() {
		time.Sleep(initTimeout)
		sendFirstSize(fbCols, fbRows)
	}()
	// 任一侧退出都必须关掉 socket：否则独占位随旧连接悬挂，重连会被 busy 挡住
	// （实测踩过：WS 关闭只取消了 ctx，socket 读仍阻塞，worker 槽位不释放）。
	defer fc.close()
	go func() {
		<-ctx.Done()
		fc.close()
	}()

	// WS 写：有界超时——对端停止读取时在此暴露为超时，而不是无限阻塞。
	writeWS := func(typ websocket.MessageType, data []byte) error {
		c2, cl := context.WithTimeout(ctx, 5*time.Second)
		defer cl()
		return w.write(c2, typ, data)
	}

	// WS 读侧。
	go func() {
		defer cancel()
		for {
			mt, data, err := w.c.Read(ctx)
			if err != nil {
				return
			}
			if mt == websocket.MessageText {
				var ctl struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if jsonUnmarshal(data, &ctl) != nil || ctl.Type != "resize" ||
					ctl.Cols < 2 || ctl.Cols > 500 || ctl.Rows < 2 || ctl.Rows > 200 {
					continue
				}
				sendFirstSize(ctl.Cols, ctl.Rows)
				continue
			}
			if len(data) > 0 {
				if err := fc.writeFrame(ftInput, data); err != nil {
					return
				}
			}
		}
	}()

	// socket 读侧（主循环）：把 worker 的帧翻译成 WS 侧动作。
	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			// worker 连接断开：有 EXIT 帧在先的已按 1000 处理；走到这里说明
			// 是异常断开（worker 崩溃/被杀）→ 1011。
			w.close(websocket.StatusCode(1011), "worker connection lost")
			break
		}
		switch t {
		case ftReady:
			if err := writeWS(websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
				cancel()
				return
			}
		case ftOutput:
			if err := writeWS(websocket.MessageBinary, payload); err != nil {
				// 写超时（对端未消费）→ 4001；其他（对端断开/取消）→ 静默退出。
				if errors.Is(err, context.DeadlineExceeded) {
					w.close(websocket.StatusCode(4001), "too slow")
				}
				return
			}
		case ftDetach:
			var dm detachMsg
			_ = jsonUnmarshal(payload, &dm)
			code := websocket.StatusCode(dm.Code)
			if dm.Code < 4000 || dm.Code > 4999 {
				code = 4001
			}
			w.close(code, dm.Reason)
			return
		case ftExit:
			var em exitMsg
			_ = jsonUnmarshal(payload, &em)
			w.close(websocket.StatusCode(1000), "session ended")
			return
		}
	}
}
