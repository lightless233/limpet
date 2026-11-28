package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

func TestSessionConcurrentOutputResizeAndFinish(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	newGrid := func() *libghostty.Terminal {
		grid, err := libghostty.NewTerminal(libghostty.WithSize(gridCols, gridRows))
		if err != nil {
			t.Fatal(err)
		}
		return grid
	}
	dir := t.TempDir()
	raw, err := os.Create(filepath.Join(dir, "recording.log"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.Create(filepath.Join(dir, "recording.meta"))
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	s := &session{
		ptmx: master, grid: newGrid(), shadow: newGrid(),
		curCols: gridCols, curRows: gridRows,
		queryQueue: make(chan []byte, queryQueueSize),
		done:       make(chan struct{}),
		rec: &recording{
			queue:     make(chan recordingEvent, recordingQueueSize),
			done:      make(chan struct{}),
			finalDims: viewerDims{Cols: gridCols, Rows: gridRows},
		},
	}
	go s.rec.write(raw, meta, time.Now(), viewerDims{Cols: gridCols, Rows: gridRows}, "", "")
	s.resize(91, 31)
	size, err := pty.GetsizeFull(master)
	if err != nil || size.Cols != 91 || size.Rows != 31 {
		t.Errorf("PTY 初始 resize = %v, err=%v", size, err)
	}

	var routines sync.WaitGroup
	ready := make(chan struct{}, 4)
	stop := make(chan struct{})
	run := func(op func()) {
		routines.Go(func() {
			op()
			ready <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					op()
				}
			}
		})
	}
	batch := []byte("concurrent output\r\n")
	run(func() { s.handleOutput(batch) })
	run(func() { s.resize(92, 25) })
	run(func() { s.resize(70, 27) })
	run(func() {
		_, _, _, _, _ = s.rebuildFor(new(viewer))
		_, _ = s.currentDims()
	})
	for range cap(ready) {
		<-ready
	}
	s.finish(exitMsg{Reason: "test"})
	close(stop)
	routines.Wait()

	if _, _, _, _, err := s.rebuildFor(new(viewer)); err == nil {
		t.Error("已关闭的网格仍允许重建")
	}
	if cols, rows := s.currentDims(); cols != 0 || rows != 0 {
		t.Errorf("关闭后仍返回网格尺寸 %dx%d", cols, rows)
	}
	s.resize(80, 24)
	s.handleOutput(batch)
	data, err := os.ReadFile(raw.Name())
	if err != nil || !bytes.Contains(data, batch) {
		t.Errorf("录制未保存关闭前的输出，err=%v", err)
	}
}

func TestViewerReplayAndResizeOrder(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	newGrid := func() *libghostty.Terminal {
		t.Helper()
		grid, err := libghostty.NewTerminal(libghostty.WithSize(gridCols, gridRows))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(grid.Close)
		return grid
	}
	s := &session{
		ptmx: master, grid: newGrid(), shadow: newGrid(), curCols: gridCols, curRows: gridRows,
		viewers: make(map[*viewer]struct{}), rec: &recording{queue: make(chan recordingEvent, 8)},
	}
	worker, client := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	v := newViewer(newFrameConn(worker), 1<<62)
	if !s.addViewer(v) {
		t.Fatal("接入失败")
	}
	s.handleOutput([]byte("\x1b[2;3Hsnapshot"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveViewer(v, handshakeAck{OK: true}, true)
	}()
	defer func() {
		_ = client.Close()
		<-done
	}()
	if kind, _, err := readFrame(client); err != nil || kind != ftHandshakeAck {
		t.Fatalf("握手 = %v, %v", kind, err)
	}
	var header [5]byte
	if _, err := io.ReadFull(client, header[:]); err != nil || frameType(header[0]) != ftReady {
		t.Fatalf("ready 帧头 = %v, %v", header, err)
	}
	// Keep the viewer writer blocked in ready while publishing output on both sides of a resize.
	before := "\x1b[4;7Hbefore"
	after := "\x1b[5;111Hafter"
	s.handleOutput([]byte(before))
	s.resize(120, 34)
	s.handleOutput([]byte(after))
	s.ending.Store(true)
	s.viewersMu.Lock()
	s.end = exitMsg{Reason: "test"}
	close(v.queue)
	s.viewersMu.Unlock()
	ready := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(client, ready); err != nil || string(ready) != `{"cols":80,"rows":24}` {
		t.Fatalf("快照尺寸 = %q, %v", ready, err)
	}
	read := func(want frameType) []byte {
		t.Helper()
		kind, body, err := readFrame(client)
		if err != nil || kind != want {
			t.Fatalf("帧 = %v %q (%v)，期望 %v", kind, body, err, want)
		}
		return body
	}
	renderer := newGrid()
	feedGround(renderer, read(ftOutput))
	if body := read(ftOutput); string(body) != before {
		t.Fatalf("resize 前的 VT = %q", body)
	} else {
		feedGround(renderer, body)
	}
	_, dims, ok := decodeResize(read(ftSize))
	if !ok || dims != (viewerDims{Cols: 120, Rows: 34}) {
		t.Fatalf("实时尺寸 = %+v", dims)
	}
	if err := renderer.Resize(dims.Cols, dims.Rows, 0, 0); err != nil {
		t.Fatal(err)
	}
	if body := read(ftOutput); string(body) != after {
		t.Fatalf("resize 后的 VT = %q", body)
	} else {
		feedGround(renderer, body)
	}
	read(ftExit)
	want, err := formatVT(s.grid)
	got, renderErr := formatVT(renderer)
	if err != nil || renderErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("重放网格与 worker 不一致\n渲染: %q\nworker: %q\n错误: %v / %v", got, want, renderErr, err)
	}
}

func TestReapTreeReclaimsAdoptedOrphan(t *testing.T) {
	if os.Getenv("LIMPET_TEST_REAPER") == "1" {
		checkReaperInChild(t)
		return
	}
	// reapTree 所在的 worker 独占它的子进程。用单独进程复现这个边界。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReapTreeReclaimsAdoptedOrphan$")
	cmd.Env = append(os.Environ(), "LIMPET_TEST_REAPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker 回收测试失败: %v\n%s", err, output)
	}
}

func checkReaperInChild(t *testing.T) {
	if err := setSubreaper(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidFile, readyFile := filepath.Join(dir, "pid"), filepath.Join(dir, "ready")
	cmd := exec.Command("/bin/sh", "-c", `
		setsid /bin/sh -c 'trap "" HUP TERM; printf ready > "$1"; exec sleep 12' orphan "$1" &
		printf '%s\n' "$!" > "$2"
	`, "shell", readyFile, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			reapTree(cmd.Process.Pid)
		}
		_ = cmd.Process.Release()
	}()

	deadline := time.Now().Add(3 * time.Second)
	var orphanPID int
	var adopted bool
	for time.Now().Before(deadline) {
		pid, err := os.ReadFile(pidFile)
		if err == nil {
			orphanPID, _ = strconv.Atoi(strings.TrimSpace(string(pid)))
		}
		if orphanPID > 0 {
			status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", orphanPID))
			_, readyErr := os.Stat(readyFile)
			group, groupErr := syscall.Getpgid(orphanPID)
			adopted = readyErr == nil && groupErr == nil && group != cmd.Process.Pid &&
				strings.Contains(string(status), fmt.Sprintf("\nPPid:\t%d\n", os.Getpid()))
			if adopted {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !adopted {
		t.Fatal("未建立忽略 HUP/TERM 且已过继到 worker 的独立作业")
	}
	reapTree(cmd.Process.Pid)
	reaped = true
	if err := syscall.Kill(orphanPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("回收后孤儿进程 %d 仍存在，signal(0) = %v", orphanPID, err)
	}
	if _, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Errorf("仍有未收割的子进程，Wait4 = %v", err)
	}
}

func TestSessionPTYResizePreservesReadDeadline(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "3")
	master, err := startSessionPTY(cmd, viewerDims{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := setSessionPTYSize(master, 91, 31); err != nil {
		t.Fatal(err)
	}
	if err := master.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := master.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("调整尺寸后的空闲 PTY 读未在期限内返回: %v", err)
	}
}

// 快照序列化不得携带 OSC 4 调色板：它会覆盖浏览器侧深/浅色主题配好的
// ANSI 16 色。正文与样式序列必须照常保留。
func TestFormatVTExcludesPalette(t *testing.T) {
	term, err := libghostty.NewTerminal(libghostty.WithSize(gridCols, gridRows))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.VTWrite([]byte("\x1b[32mgreen\x1b[1;36mcyan\x1b[0m plain"))
	out, err := formatVT(term)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "\x1b]4;") {
		t.Fatalf("快照包含 OSC 4 调色板: %q", s)
	}
	if !strings.Contains(s, "green") || !strings.Contains(s, "cyan") || !strings.Contains(s, "plain") {
		t.Fatalf("快照丢失正文: %q", s)
	}
	if !regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(s) {
		t.Fatalf("快照丢失 SGR 样式序列: %q", s)
	}
}
