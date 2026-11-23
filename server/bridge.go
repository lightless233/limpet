package main

// server 侧：WS 接入 ↔ worker socket 的中继（多视图，spec 004）。
//
// 接入握手发生在 WS 升级之前（spec 003）：不存在 404、握手失败 500，都是 HTTP
// 响应，连接不升级；**没有 409**——多视图下每条连接都成为独立 viewer。
// server 按握手应答里 worker 自报的协议版本适配语义（spec 004 单元 2）：
//   v2 worker：resize 帧 = 窗口尺寸申报（worker 内部做输入晋升）；
//   v1 worker（升级窗口）：首帧 resize 触发其重建（合成），后续照发（旧语义）。

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
	c          *websocket.Conn
	mu         sync.Mutex
	closed     atomic.Bool
	resizeSeen atomic.Bool // 客户端是否已发过 resize（v1 worker 兜底定时器用）
	slow       atomic.Bool // 对端消费得慢（WS 写曾长时间阻塞）——worker 断开时据此按「太慢」而不是「异常」报
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

// probeWorker 探测一台 worker：拨号 → 握手 → 断开。不占任何位。
// 版本适配：先以 server 支持的最高版本握手；worker 自报更低版本且拒绝时，
// 以 worker 版本重试一次（升级窗口内的旧 worker）。
func probeWorker(path string, uuid string) (handshakeAck, int, error) {
	return workerOp(path, uuid, "probe", 2*time.Second, nil)
}

// killWorker 令 worker 回收会话并退出。返回前不等待回收完成（worker 回复 ok 后
// 自行按树 TERM → 兜底 KILL、移除 socket、退出）。
func killWorker(path string, uuid string) (handshakeAck, int, error) {
	return workerOp(path, uuid, "kill", 5*time.Second, nil)
}

// attachWorker 在 worker 上登记一个 viewer（多视图：不做独占判定）。
func attachWorker(path string, uuid string, proto int) (*frameConn, handshakeAck, int, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, handshakeAck{}, 0, err
	}
	fc := newFrameConn(conn)
	_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: "attach", Proto: proto, UUID: uuid}))
	t, payload, err := fc.readFrame()
	if err != nil {
		fc.close()
		return nil, handshakeAck{}, 0, err
	}
	if t != ftHandshakeAck {
		fc.close()
		return nil, handshakeAck{}, 0, errors.New("握手应答类型异常")
	}
	var ack handshakeAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		fc.close()
		return nil, handshakeAck{}, 0, err
	}
	return fc, ack, ack.Proto, nil
}

// workerOp 通用操作（probe/kill）：带版本回退（server 最高版本 → worker 自报版本）。
func workerOp(path string, uuid string, op string, readBudget time.Duration, _ any) (handshakeAck, int, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return handshakeAck{}, 0, err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(readBudget))
	fc := newFrameConn(conn)
	_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: op, Proto: protocolVersion, UUID: uuid}))
	t, payload, err := fc.readFrame()
	if err != nil {
		return handshakeAck{}, 0, err
	}
	if t != ftHandshakeAck {
		return handshakeAck{}, 0, errors.New("握手应答类型异常")
	}
	var ack handshakeAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		return handshakeAck{}, 0, err
	}
	if ack.OK {
		return ack, protocolVersion, nil
	}
	// 版本不符：以 worker 自报版本重试一次（旧 worker）。
	if ack.Proto > 0 && ack.Proto != protocolVersion {
		_ = conn.SetReadDeadline(time.Now().Add(readBudget))
		_ = fc.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: op, Proto: ack.Proto, UUID: uuid}))
		t, payload, err = fc.readFrame()
		if err != nil {
			return handshakeAck{}, 0, err
		}
		if t != ftHandshakeAck {
			return handshakeAck{}, 0, errors.New("握手应答类型异常")
		}
		if err := jsonUnmarshal(payload, &ack); err != nil {
			return handshakeAck{}, 0, err
		}
		return ack, ack.Proto, nil
	}
	return ack, protocolVersion, nil
}

// handleWS GET /api/ws/term/{id}。多视图：不做独占判定（spec 004 修订一）。
func (m *manager) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	h := m.byIDLocked(id)
	m.mu.Unlock()
	if h == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}

	// 与 worker 握手（成为 viewer）——成功之后才升级 WS。版本适配见单元 2。
	fc, ack, workerProto, err := attachWorker(h.socketPath, id, protocolVersion)
	if err != nil {
		// socket 拨不通：worker 已死（stale），清理并按不存在处理。
		m.dropStale(id, h)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
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
	if ack.UUID != id {
		// 身份不符（spec 验收 18）：不删除、不终止，如实报错。
		fc.close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "worker 身份与请求不符"})
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 开发期经 vite 代理接入，Origin 与 Host 不一致是常态；单用户工具，跳过校验。
		InsecureSkipVerify: true,
	})
	if err != nil { // Accept 已自行写 HTTP 错误响应；关闭 socket，worker 侧自动摘除 viewer
		fc.close()
		return
	}
	ws.SetReadLimit(wsReadLimit)
	ww := &wsConn{c: ws}

	relay(ww, fc, workerProto, r)
}

// relay 把 WS 与 worker socket 双向中继。server 不解释内容，只按 worker 协议
// 版本做必要的适配：
//
//	v2 worker：WS resize → 窗口申报帧（worker 内部做输入晋升）；接入即重建，
//	           ready 在 attach 后立即到达。
//	v1 worker（升级窗口）：客户端 2.5s 内未发 resize 时，以 query 兜底尺寸合成
//	           一帧 resize 触发其重建（旧时序）。
//
// socket → WS：OUTPUT → 二进制帧；READY → 文本帧 `{"type":"ready"}`；EXIT →
// 关闭码 1000；DETACH → 关闭码取载荷（4001=慢消费者）。
// WS 写均有 5s 有界超时：对端停止消费 → 关闭码 4001（spec 003 背压）。
//
// 心跳（spec 005）：每 pingInterval 发文本帧 `{"type":"ping"}`；连续 pingTimeout 没有收到
// 对端的任何消息（pong、输入、尺寸申报都算）→ 关闭码 4002。ping/pong 在此终结，不进 worker。
func relay(w *wsConn, fc *frameConn, workerProto int, r *http.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 任一侧退出都必须关掉 socket（独占位时代的老坑：不关会悬挂状态）。
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
		err := w.write(c2, typ, data)
		if err == nil && time.Since(started) >= slowWrite {
			w.slow.Store(true)
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
				var ctl struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if jsonUnmarshal(data, &ctl) != nil || ctl.Type != "resize" ||
					ctl.Cols < 2 || ctl.Cols > 500 || ctl.Rows < 2 || ctl.Rows > 200 {
					continue
				}
				// 语义按 worker 版本由 worker 解释（v2=申报 / v1=尺寸变更），server 只转发。
				w.resizeSeen.Store(true)
				if err := fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: ctl.Cols, Rows: ctl.Rows})); err != nil {
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
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastHeard.Load())) >= pingTimeout {
					w.close(websocket.StatusCode(4002), "heartbeat timeout")
					cancel()
					return
				}
				if err := writeWS(websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
					// 与输出写超时同样处理：对端停止消费 → 4001；其余原因连接已死，读侧会退出。
					if errors.Is(err, context.DeadlineExceeded) {
						w.close(websocket.StatusCode(4001), "too slow")
						cancel()
					}
					return
				}
			}
		}
	}()

	// v1 worker 的兜底：客户端迟迟不发 resize（query 有值时）→ 合成一帧。
	if workerProto == 1 {
		go func() {
			time.Sleep(2500 * time.Millisecond)
			if w.resizeSeen.Load() {
				return
			}
			cols, rows := uint16(gridCols), uint16(gridRows)
			if v, err := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16); err == nil && v >= 2 && v <= 500 {
				cols = uint16(v)
			}
			if v, err := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16); err == nil && v >= 2 && v <= 200 {
				rows = uint16(v)
			}
			_ = fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: cols, Rows: rows}))
		}()
	}

	// socket 读侧（主循环）：把 worker 的帧翻译成 WS 侧动作。
	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			// worker 连接断开：有 EXIT 帧在先的已按 1000 处理。对端一直在慢慢消费时，
			// 这是 worker 丢弃了慢 viewer——它的 DETACH 送不到（此刻正阻塞写给对端）→ 4001；
			// 其余情况才是异常断开（worker 崩溃/被杀）→ 1011。
			if w.slow.Load() {
				w.close(websocket.StatusCode(4001), "too slow")
			} else {
				w.close(websocket.StatusCode(1011), "worker connection lost")
			}
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
