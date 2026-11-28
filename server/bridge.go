package main

// server 侧：WS 接入 ↔ worker socket 的中继（多视图，spec 004）。
//
// 接入握手发生在 WS 升级之前（spec 003）：不存在 404、握手失败 500，都是 HTTP
// 响应，连接不升级；**没有 409**——多视图下每条连接都成为独立 viewer。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// wsConn 只让一个中继协程发关闭帧，读写与关闭由 websocket.Conn 协调。
type wsConn struct {
	c          *websocket.Conn
	closed     atomic.Bool
	lastSlowAt atomic.Int64
}

func (w *wsConn) close(code websocket.StatusCode, reason string) {
	if w.closed.CompareAndSwap(false, true) {
		_ = w.c.Close(code, reason)
	}
}

// probeWorker 探测一台 worker，不占 viewer。
func probeWorker(path, uuid string) (handshakeAck, error) {
	return workerOp(path, handshakeReq{Op: "probe", UUID: uuid}, 2*time.Second)
}

// killWorker 请求回收会话，worker 应答后自行回收与清理 socket。
func killWorker(path, uuid string) (handshakeAck, error) {
	return workerOp(path, handshakeReq{Op: "kill", UUID: uuid}, 5*time.Second)
}

// attachWorker 建立 viewer 连接，握手完成之后才开始中继。
func attachWorker(path, uuid string) (*frameConn, handshakeAck, error) {
	return openWorker(path, handshakeReq{Op: "attach", UUID: uuid}, 2*time.Second)
}

func workerOp(path string, req handshakeReq, readBudget time.Duration) (handshakeAck, error) {
	fc, ack, err := openWorker(path, req, readBudget)
	if err == nil {
		_ = fc.close()
	}
	return ack, err
}

func openWorker(path string, req handshakeReq, readBudget time.Duration) (*frameConn, handshakeAck, error) {
	return openWorkerContext(context.Background(), path, req, readBudget)
}

func openWorkerContext(ctx context.Context, path string, req handshakeReq, readBudget time.Duration) (_ *frameConn, ack handshakeAck, err error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, ack, err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	fc := newFrameConn(conn)
	stop := context.AfterFunc(ctx, func() { _ = fc.close() })
	defer stop()
	if err := fc.writeFrame(ftHandshake, mustJSON(req)); err != nil {
		return nil, ack, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(readBudget)); err != nil {
		return nil, ack, err
	}
	t, payload, err := fc.readFrame()
	if err != nil {
		return nil, ack, err
	}
	if t != ftHandshakeAck {
		return nil, ack, errors.New("握手应答类型异常")
	}
	if err := jsonUnmarshal(payload, &ack); err != nil {
		return nil, ack, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, ack, err
	}
	return fc, ack, nil
}

// handleWS GET /api/ws/term/{id}。多视图：不做独占判定（spec 004 修订一）。
func (m *manager) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h := m.get(id)
	if h == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}

	// 与 worker 握手成功后才升级 WS。
	fc, ack, err := attachWorker(h.socketPath, id)
	if err != nil {
		if staleSocket(err) {
			m.dropStale(id, h)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 握手失败"})
		}
		return
	}
	defer fc.close()
	if ack.Op != "attach" || ack.UUID != id {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 握手操作或身份与请求不符"})
		return
	}
	if !ack.OK {
		reason := ack.Reason
		if reason == "" {
			reason = "worker 拒绝接入"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": reason})
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept 已写 HTTP 错误响应。
	}
	ws.SetReadLimit(wsReadLimit)
	ww := &wsConn{c: ws}

	relay(ww, fc)
}

// relay 把 WS 与 worker socket 双向中继，resize 作为窗口申报转发。
//
// socket → WS：OUTPUT → 二进制帧；READY / SIZE → 文本控制帧；EXIT →
// 关闭码 1000；DETACH → 关闭码取载荷（4001=慢消费者）。
// WS 写均有 5s 有界超时：对端停止消费 → 关闭码 4001（spec 003 背压）。
//
// 心跳（spec 005）：每 pingInterval 发文本帧 `{"type":"ping"}`；连续 pingTimeout 没有收到
// 对端的任何消息（pong、输入、尺寸申报都算）→ 关闭码 4002。ping/pong 在此终结，不进 worker。
func relay(w *wsConn, fc *frameConn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer w.c.CloseNow()
	// 任一侧退出都必须关掉 worker socket。
	defer fc.close()
	go func() {
		<-ctx.Done()
		fc.close()
	}()

	// WS 写：有界超时——对端停止读取时在此暴露为超时 → 4001。没超时但阻塞了很久，
	// 说明对端一直在慢慢消费：记下来，之后 worker 断开时按 4001 报（见主循环）。
	writeWS := func(typ websocket.MessageType, data []byte) error {
		started := time.Now()
		c2, cl := context.WithTimeout(ctx, 5*time.Second)
		defer cl()
		err := w.c.Write(c2, typ, data)
		if err == nil && time.Since(started) >= slowWrite {
			w.lastSlowAt.Store(time.Now().UnixNano())
		}
		return err
	}

	// 最近一次听到对端的时刻（任何入站消息都算），心跳超时判定用。
	var lastHeard atomic.Int64
	lastHeard.Store(time.Now().UnixNano())

	// WS 读侧。
	go func() {
		defer cancel()
		for {
			mt, data, err := w.c.Read(ctx)
			if err != nil {
				return
			}
			lastHeard.Store(time.Now().UnixNano())
			if mt == websocket.MessageText {
				typ, dims, ok := decodeResize(data)
				if typ != "resize" || !ok {
					continue
				}
				if err := fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: dims.Cols, Rows: dims.Rows})); err != nil {
					return
				}
				continue
			}
			if len(data) > 0 {
				if err := fc.writeFrame(ftInput, data); err != nil {
					return
				}
			}
		}
	}()

	// 心跳：保活 + 判对端失联。无声死亡的连接（手机锁屏/切网）靠它收敛，
	// 不必等输出积压或 TCP 超时才发现。
	go func() {
		defer cancel()
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastHeard.Load())) >= pingTimeout {
					w.close(websocket.StatusCode(4002), "heartbeat timeout")
					return
				}
				if err := writeWS(websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
					// 对端停止消费时尽量发送 4001。任何写失败都结束中继。
					if errors.Is(err, context.DeadlineExceeded) {
						w.close(websocket.StatusCode(4001), "too slow")
					}
					return
				}
			}
		}
	}()

	// socket 读侧（主循环）：把 worker 的帧翻译成 WS 侧动作。
	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			// worker 连接断开：有 EXIT 帧在先的已按 1000 处理。对端一直在慢慢消费时，
			// 这是 worker 丢弃了慢 viewer——它的 DETACH 送不到（此刻正阻塞写给对端）→ 4001；
			// 其余情况才是异常断开（worker 崩溃/被杀）→ 1011。
			if last := w.lastSlowAt.Load(); last != 0 && time.Since(time.Unix(0, last)) <= slowWriteWindow {
				w.close(websocket.StatusCode(4001), "too slow")
			} else {
				w.close(websocket.StatusCode(1011), "worker connection lost")
			}
			break
		}
		switch t {
		case ftReady, ftSize, ftOutput:
			kind := websocket.MessageBinary
			if t != ftOutput {
				kind = websocket.MessageText
				_, dims, ok := decodeResize(payload)
				valid := ok && dims.valid()
				typ := "ready"
				if t == ftSize {
					if !valid {
						continue
					}
					typ = "size"
				}
				ctl := struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols,omitempty"`
					Rows uint16 `json:"rows,omitempty"`
				}{Type: typ}
				if valid {
					ctl.Cols, ctl.Rows = dims.Cols, dims.Rows
				}
				payload = mustJSON(ctl)
			}
			if err := writeWS(kind, payload); err != nil {
				// 对端停止消费 → 4001；其他（对端断开/取消）→ 静默退出。
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
