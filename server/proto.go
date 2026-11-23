package main

// worker socket 的私有帧协议（spec 003/004）。
//
// 帧格式：[type:1][len:4 BE][payload]。握手帧是连接上的第一帧，双方各发一帧。
//
// 版本（spec 004）：protocolVersion 是 server/新 worker 使用的版本（2）。
// ftResize 在 v2 中语义为「窗口尺寸申报」——不直接触发 resize；网格尺寸跟随
// 输入者（输入批次触发晋升）。server 按握手应答里 worker 自报的版本适配：
// 对 v1 旧 worker 维持旧语义（首帧 resize 触发其重建、申报照发即尺寸变更）。

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// protocolVersion 是 server 与新 worker 使用的协议版本。worker 握手对 ≤ 本版本
// 的请求向下兼容；旧 worker 只认自己的版本，由 server 端回退适配。
const protocolVersion = 2

const (
	wsReadLimit = 1 << 20 // 大粘贴兜底；前端按 64KiB 主动切块
	wsChunk     = 64 << 10
	queueChunks = 256              // 每个 viewer 的实时流队列上限（背压：满则丢弃该 viewer）
	spawnWait   = 5 * time.Second  // 等待 worker 就绪（socket 可探测）
	frameBudget = 2 * time.Second  // 单帧写入的兜底超时：任何慢消费者都不能钉住写锁
	killGrace   = 3 * time.Second  // 关闭回收的 TERM 宽限
)

// jsonUnmarshal 语义化包装，统一错误来源。
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type frameType byte

const (
	ftOutput       frameType = 0x01 // PTY 输出字节（worker → server）
	ftInput        frameType = 0x02 // 终端输入字节（server → worker）
	ftResize       frameType = 0x03 // v2：窗口尺寸申报（server → worker）；v1：尺寸变更
	ftExit         frameType = 0x04 // 会话结束通知（worker → server）
	ftReady        frameType = 0x05 // 重建完成、实时流开始（worker → server）
	ftDetach       frameType = 0x06 // 消费者被丢弃（慢），带 WS 关闭码（worker → server）
	ftHandshake    frameType = 0x10 // 握手请求（server → worker，连接首帧）
	ftHandshakeAck frameType = 0x11 // 握手应答（worker → server）
)

// handshakeReq 是连接首帧。op：probe（探测，不占位）/ attach（成为 viewer）/ kill（回收会话）。
type handshakeReq struct {
	Op    string `json:"op"`
	Proto int    `json:"proto"`
	UUID  string `json:"uuid"`
}

// handshakeAck 是握手应答。UUID/PID 用于 server 核对身份（worker 自报 uuid 必须与
// socket 文件名一致，spec 验收 18）；Busy 表示存在接入中的 viewer。
type handshakeAck struct {
	Op     string `json:"op"`
	OK     bool   `json:"ok"`
	Proto  int    `json:"proto"`
	UUID   string `json:"uuid"`
	PID    int    `json:"pid"`
	Busy   bool   `json:"busy"`
	Alive  bool   `json:"alive"`
	Reason string `json:"reason,omitempty"`
}

// resizeMsg 是 ftResize 的载荷。v2 语义：窗口尺寸申报。
type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// exitMsg 是 ftExit 的载荷。
type exitMsg struct {
	Reason string `json:"reason"`
}

// detachMsg 是 ftDetach 的载荷，Code 是期望的 WS 关闭码（如 4001）。
type detachMsg struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// writeFrame 在 w 上写一帧。同一连接同一时刻只允许一个写者（调用方保证）。
func writeFrame(w io.Writer, t frameType, payload []byte) error {
	head := [5]byte{byte(t)}
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// readFrame 读一帧。payload 上限 8 MiB（防御性，输入切块远小于此）。
func readFrame(r io.Reader) (frameType, []byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > 8<<20 {
		return 0, nil, fmt.Errorf("帧过大: %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return frameType(head[0]), payload, nil
}

// writeJSONFrame 写一帧 JSON 载荷。
func writeJSONFrame(w io.Writer, t frameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, t, b)
}

// frameConn 是 net.Conn 上的帧封装：写入互斥（每帧 2s 兜底超时——任何慢消费者
// 都不能钉住写锁，spec 004 背压）、只关一次。读写可并发。
type frameConn struct {
	c      net.Conn
	mu     sync.Mutex
	closed atomic.Bool
}

func newFrameConn(c net.Conn) *frameConn {
	return &frameConn{c: c}
}

func (fc *frameConn) writeFrame(t frameType, payload []byte) error {
	if fc.closed.Load() {
		return errors.New("连接已关闭")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	_ = fc.c.SetWriteDeadline(time.Now().Add(frameBudget))
	head := [5]byte{byte(t)}
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := fc.c.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := fc.c.Write(payload)
	return err
}

// writeFrameTimeout 带写超时的帧写入（用于向可能停滞的对端发通知：尽力而为，
// 超时即失败，由调用方决定后续——通常直接关闭连接）。
func (fc *frameConn) writeFrameTimeout(t frameType, payload []byte, budget time.Duration) error {
	if fc.closed.Load() {
		return errors.New("连接已关闭")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	_ = fc.c.SetWriteDeadline(time.Now().Add(budget))
	head := [5]byte{byte(t)}
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := fc.c.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := fc.c.Write(payload)
	return err
}

func (fc *frameConn) readFrame() (frameType, []byte, error) {
	return readFrame(fc.c)
}

func (fc *frameConn) close() error {
	if !fc.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = fc.c.SetWriteDeadline(time.Time{})
	return fc.c.Close()
}

// viewerDims 是一个 viewer 申报的窗口尺寸。
type viewerDims struct {
	Cols uint16
	Rows uint16
}

func (d viewerDims) valid() bool { return d.Cols >= 2 && d.Cols <= 500 && d.Rows >= 2 && d.Rows <= 200 }
