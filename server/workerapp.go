package main

// worker 子命令的入口：`limpet-server worker <uuid>`。
//
// worker 进程持有 PTY 与双网格，独立于 server 进程存活——server 被杀（含 Ctrl+C
// 与 kill -9）时它成为孤儿继续运行，PTY 不随 server 关闭（这就是 spec 003 验收 9
// 的机制，与 systemd 无关）。worker 由 server 以 Setsid 启动，天然脱离 server 的
// 进程组与会话；正式部署接 systemd-run 时只换这一条启动路径（spec「延后」小节）。

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

	// accept 循环：探测/kill 连接各自短生命周期处理；attach 连接占独占位。
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
	if req.Proto != protocolVersion {
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
		c := &consumer{conn: fc, queue: make(chan []byte, queueChunks)}
		if !s.consumer.CompareAndSwap(nil, c) {
			_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
				Op: "attach", OK: false, Proto: protocolVersion, UUID: s.id, Busy: true,
				Reason: "会话已有接入",
			}))
			return
		}
		_ = fc.writeFrame(ftHandshakeAck, mustJSON(handshakeAck{
			Op: "attach", OK: true, Proto: protocolVersion, UUID: s.id, PID: os.Getpid(),
		}))
		s.serveAttachConn(fc, c)
	}
}

// serveAttachConn 服务一条 attach 连接：首帧 resize 触发尺寸调整与重建，
// 之后输入直写 PTY、resize 直调、输出帧与实时队列发往 server。
func (s *session) serveAttachConn(fc *frameConn, c *consumer) {
	rebuilt := false
	defer func() {
		s.consumer.CompareAndSwap(c, nil)
		fc.close()
	}()

	// 输出泵：把实时队列的帧发往 server（重建帧写完之后才启动，顺序由
	// 「先直写重建、后开泵」保证）。写失败（含 4001 丢弃触发的关闭）即退出。
	go func() {
		for {
			select {
			case chunk := <-c.queue:
				if err := fc.writeFrame(ftOutput, chunk); err != nil {
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
			var rm resizeMsg
			if err := jsonUnmarshal(payload, &rm); err != nil ||
				rm.Cols < 2 || rm.Cols > 500 || rm.Rows < 2 || rm.Rows > 200 {
				continue
			}
			if !rebuilt {
				rebuilt = true
				s.resize(rm.Cols, rm.Rows)
				out, cont, err := s.rebuild()
				if err != nil {
					_ = fc.writeFrame(ftDetach, mustJSON(detachMsg{Code: 1011, Reason: "rebuild failed: " + err.Error()}))
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
			} else {
				s.resize(rm.Cols, rm.Rows)
			}
		case ftInput:
			if s.ptmx != nil && len(payload) > 0 {
				_, _ = s.ptmx.Write(payload)
			}
		default:
			// 未知帧忽略（加法演进的前向兼容）。
		}
	}
}
