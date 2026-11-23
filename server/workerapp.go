package main

// worker 子命令的入口：`limpet-server worker <uuid>`。
//
// worker 进程持有 PTY 与双网格，独立于 server 进程存活——server 被杀（含 Ctrl+C
// 与 kill -9）时它继续运行，PTY 不随 server 关闭（spec 003 验收 9 的机制，
// 与 systemd 无关）。启动方式有两种（spec 005，见 launch.go）：前台开发时由 server
// 以 Setsid 直接 spawn，天然脱离 server 的进程组与会话；服务部署时经 systemd-run
// 进入自己的临时单元，不在 server 的控制组里——server 单元停止或重启时不被一并清理。
//
// 多视图（spec 004）：每条 attach 连接 = 一个 viewer，各自有界队列与切点；
// PTY 输出广播给全部 viewer，输入来自任意 viewer。

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func runWorker(uuid string, paths sessionPaths) error {
	if !uuidRe.MatchString(uuid) {
		return fmt.Errorf("uuid 不合法: %q", uuid)
	}
	// 子进程收割者：Shell 死后脱离进程组的后台作业收养到 worker，显式关闭时按树回收。
	if err := setSubreaper(); err != nil {
		log.Printf("setSubreaper 失败（回收将只覆盖进程组）: %v", err)
	}
	if err := os.MkdirAll(paths.socketDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.histDir, 0o755); err != nil {
		return err
	}

	s, err := startSession(paths, uuid)
	if err != nil {
		return err
	}

	sockPath := filepath.Join(paths.socketDir, uuid+".sock")
	_ = os.Remove(sockPath) // 同名 socket 残留只可能是 stale（worker 只在新建时被 spawn）
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		s.finish("listen-failed")
		return err
	}
	_ = os.Chmod(sockPath, 0o600)

	// SIGTERM（server 关闭的兜底信号）：按显式关闭回收。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() {
		<-sig
		s.shutdown("closed")
	}()

	log.Printf("worker %s 就绪 (pid %d, socket %s)", uuid, os.Getpid(), sockPath)

	// accept 循环：探测/kill 连接各自短生命周期处理；attach 连接成为 viewer。
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.handleWorkerConn(newFrameConn(c))
		}
	}()

	// 会话结束（Shell 退出 / 显式关闭）→ 清理 socket → 进程退出。
	<-s.done
	_ = l.Close()
	_ = os.Remove(sockPath)
	log.Printf("worker %s 退出", uuid)
	return nil
}

// handleWorkerConn 处理一条到 worker 的连接：握手分流 probe / kill / attach。
func (s *session) handleWorkerConn(fc *frameConn) {
	defer fc.close()
	log.Printf("[调试] conn 已接受")

	t, payload, err := fc.readFrame()
	if err != nil || t != ftHandshake {
		log.Printf("[调试] 握手读取失败/类型异常: err=%v t=%v", err, t)
		return
	}
	log.Printf("[调试] 握手已读: %s", payload[:min(len(payload), 90)])
	var req handshakeReq
	if err := jsonUnmarshal(payload, &req); err != nil {
		return
	}
	// 版本协商：worker 对 ≤ 自身版本的请求向下兼容；超出则拒绝并自报版本。
	if req.Proto > protocolVersion {
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: req.Op, OK: false, Proto: protocolVersion, UUID: s.id,
			Reason: fmt.Sprintf("协议版本不兼容: 请求 %d，worker %d", req.Proto, protocolVersion),
		}))
		return
	}
	// 身份核对（spec 验收 18）：请求的 uuid 必须与 worker 自报一致。
	if req.UUID != s.id {
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: req.Op, OK: false, Proto: protocolVersion, UUID: s.id,
			Reason: fmt.Sprintf("uuid 不符: 请求 %q，worker %q", req.UUID, s.id),
		}))
		return
	}

	switch req.Op {
	case "probe":
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "probe", OK: true, Proto: protocolVersion, UUID: s.id,
			PID: os.Getpid(), Busy: s.attached(), Alive: true,
		}))

	case "kill":
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "kill", OK: true, Proto: protocolVersion, UUID: s.id, PID: os.Getpid(),
		}))
		// 回收整棵会话进程树（subreaper），然后收尾退出（done 关闭 → 主函数清理）。
		s.shutdown("closed")

	case "attach":
		// 多视图：每条连接独立成为 viewer，互不排斥（spec 004 修订一）。
		v := &viewer{
			conn: fc,
			queue: make(chan []byte, queueChunks),
			// 切点初值 = 无穷大：重建完成前一切字节只进重建画面、不进实时流，
			// 否则接入到重建之间的输出会被「实时发一遍 + 重建再发一遍」
			// （验收 15 实测抓到过整段双份）。
		}
		v.cutOffset.Store(1<<62) // 接近无穷大的安全初值（atomic.Int64 零值是 0）
		s.viewersMu.Lock()
		s.viewers[v] = viewerDims{}
		s.viewersMu.Unlock()
		log.Printf("[调试] attach ack 已写，进入 serveViewerConn")
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "attach", OK: true, Proto: protocolVersion, UUID: s.id, PID: os.Getpid(),
		}))
		log.Printf("[调试] serveViewerConn 开始（重建+读循环）")
		s.serveViewerConn(fc, v)
	}
}

// serveViewerConn 服务一个 viewer：接入即重建（按网格当前尺寸，不等尺寸——
// spec 004 修订三），此后窗口申报存档、输入晋升尺寸并直写 PTY、实时流广播。
func (s *session) serveViewerConn(fc *frameConn, v *viewer) {
	defer func() {
		s.removeViewer(v)
		fc.close()
	}()

	// 接入即重建。
	out, cont, err := s.rebuildFor(v)
	if err != nil {
		// 重建失败：断开连接（server 侧映射为 WS 1011 + 原因）。
		fc.close()
		return
	}
	// ready 先于任何输出帧（spec 003 帧协议）。
	if err := fc.writeFrame(ftReady, nil); err != nil {
		return
	}
	for i := 0; i < len(out); i += wsChunk {
		end := min(i+wsChunk, len(out))
		if err := fc.writeFrame(ftOutput, out[i:end]); err != nil {
			return
		}
	}
	if len(cont) > 0 {
		// 切点落在序列中间：未完成序列的字节作为实时流前缀（U0-8a 兜底）。
		if err := fc.writeFrame(ftOutput, cont); err != nil {
			return
		}
	}

	// 输出泵：切点之后的实时流。写失败（含 2s 写超时 = server 端停止消费）→
	// 补发 DETACH(4001) 再关连接：此时上一次写已超时返回、锁已空闲，DETACH 能
	// 在兜底超时内送达。会话本身不受影响（PTY 读取与录制照常，spec 003 背压）。
	go func() {
		for {
			select {
			case chunk := <-v.queue:
				if err := fc.writeFrame(ftOutput, chunk); err != nil {
					if v.closed.Load() {
						return
					}
					_ = fc.writeFrame(ftDetach, mustJSON(detachMsg{Code: 4001, Reason: "too slow"}))
					fc.close()
					return
				}
			}
		}
	}()

	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			return
		}
		switch t {
		case ftResize:
			// v2 语义：窗口尺寸申报，不触发 resize。
			var rm resizeMsg
			dims := viewerDims{Cols: rm.Cols, Rows: rm.Rows}
			if err := jsonUnmarshal(payload, &rm); err == nil {
				dims = viewerDims{Cols: rm.Cols, Rows: rm.Rows}
			}
			if !dims.valid() {
				continue
			}
			s.viewersMu.Lock()
			s.viewers[v] = dims
			s.viewersMu.Unlock()
			v.dims = dims
		case ftInput:
			if s.ptmx == nil || len(payload) == 0 {
				continue
			}
			// 尺寸跟随输入者（spec 004 修订二）：打字者的窗口尺寸成为网格尺寸。
			// 每批输入至多一次调整；尺寸相同则跳过（无 resize 风暴）。
			if v.dims.valid() {
				curCols, curRows := s.currentDims()
				if v.dims.Cols != curCols || v.dims.Rows != curRows {
					s.resize(v.dims.Cols, v.dims.Rows)
				}
			}
			_, _ = s.ptmx.Write(payload)
		default:
			// 未知帧忽略（加法演进的前向兼容）。
		}
	}
}
