package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(tests *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "worker" {
		if dir := os.Getenv("LIMPET_TEST_WORKER_HOME"); dir != "" {
			paths := sessionPaths{
				histDir: filepath.Join(dir, "history"), socketDir: filepath.Join(dir, "workers"),
				settingsPath: filepath.Join(dir, "settings.json"),
			}
			if err := runWorker(os.Args[2], paths); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	os.Exit(tests.Run())
}

func testManager(t *testing.T) *manager {
	t.Helper()
	// Unix socket 的文件名只允许 108 字节，测试目录名保持简短。
	dir, err := os.MkdirTemp("", "limpet-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("LIMPET_TEST_WORKER_HOME", dir)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	m := &manager{
		sessions:     make(map[string]*workerHandle),
		creating:     make(map[string]struct{}),
		pendingIDs:   make(map[string]struct{}),
		registry:     make(map[string]regEntry),
		workersDir:   filepath.Join(dir, "workers"),
		regPath:      filepath.Join(dir, "sessions.json"),
		settingsPath: filepath.Join(dir, "settings.json"),
		scrollback:   defaultScrollback,
		exePath:      os.Args[0],
		launch:       launchDirect,
	}
	if err := os.Mkdir(m.workersDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for id := range m.sessions {
			if err := m.closeSession(id); err != nil {
				t.Errorf("清理测试会话 %s: %v", id, err)
			}
		}
	})
	return m
}

func createTestShell(m *manager, name string) (*workerHandle, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	config, err := webPreset("shell", home)
	if err != nil {
		return nil, err
	}
	return m.create(name, &config, false)
}

func TestManagerConcurrentCreateRenameAndList(t *testing.T) {
	m := testManager(t)
	first, err := createTestShell(m, "first")
	if err != nil {
		t.Fatal(err)
	}

	const name = "contended"
	ready := make(chan struct{})
	results := make(chan error, 3)
	var workers sync.WaitGroup
	for _, op := range []func() error{
		func() error { _, err := createTestShell(m, name); return err },
		func() error { _, err := createTestShell(m, name); return err },
		func() error { return m.renameSession(first.uuid, name) },
	} {
		workers.Go(func() {
			<-ready
			results <- op()
		})
	}

	workers.Go(func() {
		<-ready
		for range 20 {
			names := map[string]bool{}
			for _, item := range m.list() {
				n := item["name"].(string)
				if names[n] {
					t.Errorf("列表出现重名 %q", n)
					return
				}
				names[n] = true
			}
		}
	})
	close(ready)
	workers.Wait()
	close(results)
	var succeeded int
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, errDuplicate) {
			t.Errorf("并发操作返回了非重名错误: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("并发争用同一名称时成功 %d/3 次，期望 1 次", succeeded)
	}

	second, err := createTestShell(m, "second")
	if err != nil {
		t.Fatal(err)
	}
	assertOrder := func(want []string) {
		t.Helper()
		var ids []string
		for _, item := range m.list() {
			ids = append(ids, item["id"].(string))
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("列表顺序 = %v，期望 %v", ids, want)
		}
	}
	items := m.list()
	previous := time.Time{}
	var insertionOrder []string
	for _, item := range items {
		h := m.get(item["id"].(string))
		if h.createdAt.Before(previous) {
			t.Fatal("未按创建时间升序排列")
		}
		previous = h.createdAt
		insertionOrder = append(insertionOrder, h.uuid)
	}
	if len(insertionOrder) < 2 || insertionOrder[0] != first.uuid || insertionOrder[len(insertionOrder)-1] != second.uuid {
		t.Fatalf("创建顺序 = %v", insertionOrder)
	}
	for range 5 {
		assertOrder(insertionOrder)
	}

	m.mu.Lock()
	for _, h := range m.sessions {
		h.createdAt = first.createdAt
	}
	m.mu.Unlock()
	slices.Sort(insertionOrder)
	assertOrder(insertionOrder)
}

func TestDropStalePreservesUnrelatedProcess(t *testing.T) {
	m := testManager(t)
	cmd := exec.Command("sleep", "15")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	const id = "0123456789abcdef"
	path := filepath.Join(m.workersDir, id+".sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	defer l.Close()
	if err := l.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if kind, _, readErr := readFrame(c); readErr != nil || kind != ftHandshake {
				err = fmt.Errorf("probe frame = %v, err=%v", kind, readErr)
			} else {
				err = writeJSONFrame(c, ftHandshakeAck, handshakeAck{Op: "probe", UUID: id, OK: true, PID: cmd.Process.Pid})
			}
		}
		peer <- err
	}()
	m.recoverWorkers()
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	if m.get(id) == nil {
		t.Fatal("未从握手恢复测试会话")
	}
	_ = l.Close()
	if list := m.list(); len(list) != 0 {
		t.Fatalf("失效 socket 仍在列表: %v", list)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("失效 socket 未被删除，stat=%v", err)
	}
	select {
	case err := <-exited:
		t.Fatalf("与会话无关的进程被终止: %v", err)
	case <-time.After(100 * time.Millisecond):
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("无关进程无法探测: %v", err)
		}
	}
}
