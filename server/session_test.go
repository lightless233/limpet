package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		_, _, _ = s.rebuildFor(new(viewer))
		_, _ = s.currentDims()
	})
	for range cap(ready) {
		<-ready
	}
	s.finish(exitMsg{Reason: "test"})
	close(stop)
	routines.Wait()

	if _, _, err := s.rebuildFor(new(viewer)); err == nil {
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
