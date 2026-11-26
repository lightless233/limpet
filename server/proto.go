package main

// worker socket 的私有帧协议（spec 003/004）。
//
// 帧格式：[type:1][len:4 BE][payload]。握手帧是连接上的第一帧，双方各发一帧。
//
// ftResize 申报窗口尺寸。CLI 会话的活跃 viewer 可以立即调整尺寸。

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

const (
	wsReadLimit = 1 << 20 // 大粘贴兜底；前端按 64KiB 主动切块
	wsChunk     = 64 << 10
	// 每个 viewer 的实时流队列：槽数只是碎块突发的余量，背压按字节计——PTY 的一批可能只有
	// 几十字节（Shell 回显长行时逐段重绘），按块计会把几 KB 的积压误判成慢消费者。
	queueChunks     = 1024
	queueBytes      = 8 << 20
	spawnWait       = 5 * time.Second // 等待 worker 就绪（socket 可探测）
	frameBudget     = 2 * time.Second // 单帧写入的兜底超时：任何慢消费者都不能钉住写锁
	killGrace       = 3 * time.Second // 关闭回收的 TERM 宽限
	exitWriteBudget = time.Second     // 向 viewer 发送会话结束通知
	// 心跳（spec 005）：Cloudflare 在双向无数据 100s 后关闭 WebSocket，20s 周期留 5 倍余量；
	// 60s 即容忍连续 3 个周期听不到对端。
	pingInterval    = 20 * time.Second
	slowWrite       = 2 * time.Second  // 达到此时长的成功 WS 写入标记为慢写
	slowWriteWindow = 15 * time.Second // 仅用近期慢写推断 worker 断开原因
	pingTimeout     = 60 * time.Second
)

// jsonUnmarshal 语义化包装，统一错误来源。
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type frameType byte

const (
	ftOutput       frameType = 0x01 // PTY 输出字节（worker → server）
	ftInput        frameType = 0x02 // 终端输入字节（server → worker）
	ftResize       frameType = 0x03 // 窗口尺寸申报（server → worker）
	ftExit         frameType = 0x04 // 会话结束通知（worker → server）
	ftReady        frameType = 0x05 // 重建完成、实时流开始（worker → server）
	ftDetach       frameType = 0x06 // 消费者被丢弃（慢），带 WS 关闭码（worker → server）
	ftSize         frameType = 0x07 // PTY 与网格的实时尺寸（worker → server）
	ftHandshake    frameType = 0x10 // 握手请求（server → worker，连接首帧）
	ftHandshakeAck frameType = 0x11 // 握手应答（worker → server）
)

// handshakeReq 是连接首帧。start 由启动 CLI 认领，attach 接入已运行的会话。
type handshakeReq struct {
	Op   string `json:"op"`
	UUID string `json:"uuid"`
}

// handshakeAck 自报操作与 UUID，Busy 表示存在接入中的 viewer。
type handshakeAck struct {
	Op        string `json:"op"`
	OK        bool   `json:"ok"`
	UUID      string `json:"uuid"`
	PID       int    `json:"pid"`
	Busy      bool   `json:"busy"`
	Alive     bool   `json:"alive"`
	Reason    string `json:"reason,omitempty"`
	Name      string `json:"name,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Cmd       string `json:"cmd,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
	Ready     *bool  `json:"ready,omitempty"`
}

func (a handshakeAck) readyFor(id string) bool {
	return a.Op == "probe" && a.OK && a.UUID == id && (a.Ready == nil || *a.Ready)
}

// resizeMsg 是 ftResize 的窗口尺寸申报。
type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// exitMsg 是 ftExit 的载荷。
type exitMsg struct {
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exitCode,omitempty"`
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

// decodeResize 要求两个维度显式存在，区分清空申报与缺失字段。
func decodeResize(payload []byte) (string, viewerDims, bool) {
	var rm struct {
		Type string  `json:"type"`
		Cols *uint16 `json:"cols"`
		Rows *uint16 `json:"rows"`
	}
	if jsonUnmarshal(payload, &rm) != nil || rm.Cols == nil || rm.Rows == nil {
		return "", viewerDims{}, false
	}
	dims := viewerDims{Cols: *rm.Cols, Rows: *rm.Rows}
	return rm.Type, dims, dims == (viewerDims{}) || dims.valid()
}
