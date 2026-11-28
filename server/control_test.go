package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestControlDirectoryOwnership(t *testing.T) {
	dir, err := os.MkdirTemp("", "limpet-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	lock, err := lockServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if second, err := lockServer(dir); err == nil {
		_ = second.Close()
		t.Fatal("第二个 server 取得同一数据目录锁")
	}

	path := filepath.Join(dir, "server.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	l, err := listenControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("控制 socket 权限: %v, %v", info, err)
	}
	if other, err := listenControl(dir); err == nil {
		_ = other.Close()
		t.Fatal("活控制 socket 被替换")
	}
	_ = l.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("退出后控制 socket 未清理: %v", err)
	}
}

func testControl(t *testing.T, m *manager, req controlRequest) controlReply {
	t.Helper()
	c, peer := net.Pipe()
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(controlBudget))
	go m.handleControl(peer)
	if err := json.NewEncoder(c).Encode(req); err != nil {
		t.Fatal(err)
	}
	var r controlReply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func testProbeSocket(t *testing.T, m *manager, id string, respond func(handshakeReq) handshakeAck) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(m.workersDir, id+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(controlBudget))
				_, body, err := readFrame(c)
				var req handshakeReq
				if err == nil && json.Unmarshal(body, &req) == nil {
					_ = writeJSONFrame(c, ftHandshakeAck, respond(req))
				}
			}()
		}
	}()
}

func TestControlRegisterAndDiscover(t *testing.T) {
	m := testManager(t)
	name := strings.Repeat("名", 100)
	id, id2 := newID(), newID()
	var ready atomic.Bool
	testProbeSocket(t, m, id, func(req handshakeReq) handshakeAck {
		value := ready.Load()
		return handshakeAck{UUID: id, Op: req.Op, OK: true, Ready: &value, Name: name,
			Cwd: "/example", Cmd: "codex", CreatedAt: 1700000000123}
	})
	testProbeSocket(t, m, id2, func(req handshakeReq) handshakeAck {
		return handshakeAck{UUID: id2, Op: req.Op, OK: true, Name: name}
	})
	req := controlRequest{Op: "register", UUID: id, Name: name, ExplicitName: true}
	if r := testControl(t, m, req); r.Error == "" {
		t.Fatal("未就绪 worker 被注册")
	}
	if h := m.get(id2); h == nil || h.name != name {
		t.Fatalf("缺 ready 字段的旧应答应正常纳管: %+v", h)
	}
	ready.Store(true)
	r := testControl(t, m, req)
	if r.Error != "" || !strings.HasSuffix(r.Name, "-2") || utf8.RuneCountInString(r.Name) != 100 || !nameValid(r.Name) {
		t.Fatalf("重名注册 = %+v", r)
	}
	if err := m.renameSession(id, "renamed"); err != nil {
		t.Fatal(err)
	}
	if r := testControl(t, m, req); r.Error != "" || r.Name != "renamed" {
		t.Fatalf("重复注册覆盖名称: %+v", r)
	}
	r = testControl(t, m, controlRequest{Op: "list"})
	for _, row := range r.Sessions {
		if row.ID == id && (row.Cwd != "/example" || row.Cmd != "codex" || row.Name != "renamed") {
			t.Errorf("控制列表: %+v", row)
		}
	}
	if len(r.Sessions) != 2 {
		t.Fatalf("控制列表丢失会话: %+v", r)
	}
	m.mu.Lock()
	delete(m.sessions, id)
	delete(m.sessions, id2)
	m.mu.Unlock()
}

func TestHTTPListMetadata(t *testing.T) {
	m := testManager(t)
	home := t.TempDir()
	cwd := filepath.Join(home, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	web, err := m.create("web")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "test-limpet-private-argv-marker"
	_, id, socket := startTestCLIWorker(t, filepath.Dir(m.workersDir), cwd, "/bin/sh", "-c", "read line", "sh", secret)
	fc, ack, err := openWorker(socket, handshakeReq{Op: "start", UUID: id}, frameBudget)
	if err != nil || !ack.OK {
		t.Fatalf("start = %+v, %v", ack, err)
	}
	defer fc.close()
	if r := testControl(t, m, controlRequest{Op: "register", UUID: id, Name: "cli", ExplicitName: true}); r.Error != "" {
		t.Fatal(r.Error)
	}
	if kind, _, err := fc.readFrame(); err != nil || kind != ftReady {
		t.Fatalf("start 后首帧 = %v, %v", kind, err)
	}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/api/term")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK || strings.Contains(string(body), secret) || strings.Contains(string(body), home) {
		t.Fatalf("HTTP 列表泄露启动元数据或请求失败，HTTP %d: %s, %v", res.StatusCode, body, err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("HTTP 列表 = %s, %v", body, err)
	}
	for _, row := range rows {
		want := map[string]any{"id": row["id"], "created": row["created"]}
		switch row["id"] {
		case id:
			want["name"], want["attached"], want["cmd"], want["cwd"] = "cli", true, "sh", "~/workspace"
		case web.uuid:
			want["name"], want["attached"] = "web", false
		default:
			t.Fatalf("意外会话 %v", row)
		}
		if _, err := time.Parse(time.RFC3339, row["created"].(string)); err != nil || !reflect.DeepEqual(row, want) {
			t.Errorf("HTTP 列表项 = %v，期望 %v", row, want)
		}
	}
}

func TestDiscoverCannotReviveEndedSession(t *testing.T) {
	m := testManager(t)
	id := newID()
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	testProbeSocket(t, m, id, func(req handshakeReq) handshakeAck {
		close(probeStarted)
		<-releaseProbe
		return handshakeAck{UUID: id, Op: req.Op, OK: true, Name: "closed"}
	})
	m.registry[id] = regEntry{Name: "closed", CreatedAt: time.Now()}
	result := make(chan error, 1)
	go func() {
		_, err := m.discover(id, "", false)
		result <- err
	}()
	<-probeStarted
	m.mu.Lock()
	m.registryEndLocked(id)
	m.mu.Unlock()
	close(releaseProbe)
	if err := <-result; !errors.Is(err, errNotFound) || m.get(id) != nil {
		t.Fatalf("结束后复活: %v", err)
	}
}

func TestLegacyRegistryEntry(t *testing.T) {
	var e regEntry
	const old = `{"name":"legacy","createdAt":"2026-11-23T00:00:00Z"}`
	if err := json.Unmarshal([]byte(old), &e); err != nil || e.Name != "legacy" || e.CreatedAt.IsZero() ||
		e.EndedAt != nil || e.Cmd != "" || e.Cwd != "" {
		t.Fatalf("旧注册表解码: %+v, %v", e, err)
	}
}
