package main

// worker socket 的私有帧协议（spec 003：worker 与 server 同二进制同版本一起升级；
// 版本握手用于兼容「升级后仍在运行的旧 worker」——加法演进，不破坏性变更）。
//
// 帧格式：[type:1][len:4 BE][payload]。握手帧是连接上的第一帧，双方各发一帧。

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

// protocolVersion 是 socket 协议版本。字节序列不兼容时必须递增。
const protocolVersion = 1

const (
	wsReadLimit = 1 << 20 // 大粘贴兜底；前端按 64KiB 主动切块
	wsChunk     = 64 << 10
	queueChunks = 256              // 实时流队列上限（背压：满则断开本次接入）
	initTimeout = 3 * time.Second // 首条尺寸的兜底时限（沿用 001 语义）
	spawnWait   = 5 * time.Second // 等待 worker 就绪（socket 可探测）
)

// jsonUnmarshal 语义化包装，统一错误来源。
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type frameType byte

const (
	ftOutput       frameType = 0x01 // PTY 输出字节（worker → server）
	ftInput        frameType = 0x02 // 终端输入字节（server → worker）
	ftResize       frameType = 0x03 // resize 控制 JSON（server → worker）
	ftExit         frameType = 0x04 // 会话结束通知（worker → server）
	ftReady        frameType = 0x05 // 重建完成、实时流开始（worker → server）
	ftDetach       frameType = 0x06 // 消费者被丢弃（慢），带 WS 关闭码（worker → server）
	ftHandshake    frameType = 0x10 // 握手请求（server → worker，连接首帧）
	ftHandshakeAck frameType = 0x11 // 握手应答（worker → server）
)

// handshakeReq 是连接首帧。op：probe（探测，不占位）/ attach（占独占位）/ kill（回收会话）。
type handshakeReq struct {
	Op    string `json:"op"`
	Proto int    `json:"proto"`
	UUID  string `json:"uuid"`
}

// handshakeAck 是握手应答。UUID/PID 用于 server 核对身份（worker 自报 uuid 必须与
// socket 文件名一致，spec 验收 18）；Busy 表示独占位已被占用。
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

// resizeMsg 是 ftResize 的载荷。
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

var errShortFrame = errors.New("帧不完整")

// frameConn 是 net.Conn 上的帧封装：写入互斥、只关一次。读写可并发（写经互斥，
// 读由单一 goroutine 持有连接）。
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
	return fc.c.Close()
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
