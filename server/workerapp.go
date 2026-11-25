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
	"time"
)

func runWorker(uuid string, paths sessionPaths) error {
	if !uuidRe.MatchString(uuid) {
		return fmt.Errorf("uuid 不合法: %q", uuid)
	}
	// 子进程收割者：Shell 死后脱离进程组的后台作业收养到 worker，显式关闭时按树回收。
	if err := setSubreaper(); err != nil {
		log.Printf("setSubreaper 失败（已孤儿化的任务可能无法回收）: %v", err)
	}
	if err := os.MkdirAll(paths.socketDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.histDir, 0o700); err != nil {
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
		s.shutdown()
		return err
	}
	_ = os.Chmod(sockPath, 0o600)

	// SIGTERM（server 关闭的兜底信号）：按显式关闭回收。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() {
		<-sig
		s.shutdown()
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

	t, payload, err := fc.readFrame()
	if err != nil || t != ftHandshake {
		return
	}
	var req handshakeReq
	if err := jsonUnmarshal(payload, &req); err != nil {
		return
	}
	// 身份核对（spec 验收 18）：请求的 uuid 必须与 worker 自报一致。
	if req.UUID != s.id {
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: req.Op, OK: false, UUID: s.id,
			Reason: fmt.Sprintf("uuid 不符: 请求 %q，worker %q", req.UUID, s.id),
		}))
		return
	}

	switch req.Op {
	case "probe":
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "probe", OK: true, UUID: s.id,
			PID: os.Getpid(), Busy: s.attached(), Alive: true,
		}))

	case "kill":
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "kill", OK: true, UUID: s.id, PID: os.Getpid(),
		}))
		// 回收整棵会话进程树（subreaper），然后收尾退出（done 关闭 → 主函数清理）。
		s.shutdown()

	case "attach":
		// 多视图：每条连接独立成为 viewer，互不排斥（spec 004 修订一）。
		v := &viewer{
			conn:  fc,
			queue: make(chan []byte, queueChunks),
			done:  make(chan struct{}),
			// 切点初值 = 无穷大：重建完成前一切字节只进重建画面、不进实时流，
			// 否则接入到重建之间的输出会被「实时发一遍 + 重建再发一遍」
			// （验收 15 实测抓到过整段双份）。
		}
		v.cutOffset.Store(1 << 62) // 接近无穷大的安全初值（atomic.Int64 零值是 0）
		s.viewersMu.Lock()
		s.viewers[v] = struct{}{}
		s.viewersMu.Unlock()
		if err := fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "attach", OK: true, UUID: s.id, PID: os.Getpid(),
		})); err != nil {
			s.removeViewer(v)
			return
		}
		s.serveViewerConn(fc, v)

	default:
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: req.Op, UUID: s.id, Reason: "未知 worker 操作",
		}))
	}
}

// serveViewerConn 服务一个 viewer：接入即重建（按网格当前尺寸，不等尺寸——
// spec 004 修订三），此后窗口申报存档、输入晋升尺寸并直写 PTY、实时流广播。
func (s *session) serveViewerConn(fc *frameConn, v *viewer) {
	defer func() {
		s.removeViewer(v)
		v.closeWith(0, "") // 让输出泵退出（不发 DETACH）
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

	// 输出泵：切点之后的实时流。它是这条连接在重建之后唯一的写者——丢弃 viewer 时的
	// DETACH 与关闭也在这里做（见 closeWith）。
	// 已经排队的块合并成一帧发出：下游按帧处理，逐块发的单块开销在碎块突发时跟不上
	// PTY 读取，会把队列顶满。
	go func() {
		defer fc.close()
		// 先尽力通知被丢弃的原因，再关闭 socket。
		defer func() {
			<-v.done
			if v.dropCode != 0 {
				_ = fc.writeFrameTimeout(ftDetach, mustJSON(detachMsg{Code: v.dropCode, Reason: v.dropReason}), 300*time.Millisecond)
			}
		}()
		buf := make([]byte, 0, wsChunk)
		take := func(c []byte) {
			buf = append(buf, c...)
			v.queued.Add(-int64(len(c)))
		}
		for {
			select {
			case <-v.done:
				return
			default:
			}
			var chunk []byte
			select {
			case chunk = <-v.queue:
			case <-v.done:
				return
			}
			buf = buf[:0]
			take(chunk)
		drain:
			for len(buf) < wsChunk {
				select {
				case c := <-v.queue:
					take(c)
				default:
					break drain
				}
			}
			if v.closed.Load() {
				return
			}
			if err := fc.writeFrame(ftOutput, buf); err != nil {
				// 写失败只终止这条 viewer 连接。
				v.closeWith(4001, "too slow")
				return
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
			var rm resizeMsg
			if jsonUnmarshal(payload, &rm) != nil {
				continue
			}
			dims := viewerDims{Cols: rm.Cols, Rows: rm.Rows}
			if dims.valid() {
				v.dims = dims
			}
		case ftInput:
			if len(payload) == 0 {
				continue
			}
			// 将尺寸晋升和该批输入一起串行化，查询应答也使用同一个写锁。
			s.inputMu.Lock()
			if v.dims.valid() {
				s.resize(v.dims.Cols, v.dims.Rows)
			}
			_, err := s.ptmx.Write(payload)
			s.inputMu.Unlock()
			if err != nil {
				return
			}

		default:
			// 未知帧忽略（加法演进的前向兼容）。
		}
	}
}
