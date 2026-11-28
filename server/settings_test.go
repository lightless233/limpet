package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.mitchellh.com/libghostty"
)

func putScrollback(m *manager, body string) *httptest.ResponseRecorder {
	reply := httptest.NewRecorder()
	m.handler().ServeHTTP(reply, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(body)))
	return reply
}

func requireScrollbackPut(t *testing.T, m *manager, value int) settingsResult {
	t.Helper()
	reply := putScrollback(m, fmt.Sprintf(`{"scrollback":%d}`, value))
	var result settingsResult
	if err := json.Unmarshal(reply.Body.Bytes(), &result); reply.Code != 200 || err != nil || result.Scrollback != value {
		t.Fatalf("PUT %d: status=%d body=%s err=%v", value, reply.Code, reply.Body, err)
	}
	return result
}

func TestScrollbackSettingsPersistence(t *testing.T) {
	m := testManager(t)
	get := func(want int) {
		t.Helper()
		resp := httptest.NewRecorder()
		m.handler().ServeHTTP(resp, httptest.NewRequest("GET", "/api/settings", nil))
		var body scrollbackSetting
		if err := json.Unmarshal(resp.Body.Bytes(), &body); resp.Code != 200 || err != nil || body.Scrollback != want {
			t.Fatalf("GET: status=%d body=%s want=%d", resp.Code, resp.Body, want)
		}
	}
	get(defaultScrollback)
	for _, invalid := range []string{
		`{}`, `null`, `{"scrollback":null}`, `{"scrollback":true}`, `{"scrollback":"10"}`,
		`{"scrollback":1.5}`, `{"scrollback":-1}`, `{"scrollback":1000001}`, `{"scrollback":2} []`, strings.Repeat(" ", 4097),
	} {
		if resp := putScrollback(m, invalid); resp.Code != 400 {
			t.Fatalf("invalid PUT %q returned %d", invalid[:min(len(invalid), 45)], resp.Code)
		}
	}
	get(defaultScrollback)
	if _, err := os.Stat(m.settingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid PUT changed disk: %v", err)
	}

	requireScrollbackPut(t, m, 0)
	file, err := os.Stat(m.settingsPath)
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", file, err)
	}
	restarted := &manager{settingsPath: m.settingsPath}
	if err := restarted.loadSettings(); err != nil || restarted.currentScrollback() != 0 {
		t.Fatalf("restart lost explicit zero: %v", err)
	}

	m.settingsPath = filepath.Join(t.TempDir(), "missing-directory", "settings.json")
	if resp := putScrollback(m, `{"scrollback":2}`); resp.Code != 500 {
		t.Fatalf("write failure returned %d: %s", resp.Code, resp.Body)
	}
	get(0)

	for _, invalid := range []string{`{}`, `{"scrollback":null}`, `{"scrollback":1.5}`, `{"scrollback":-1}`, `{"scrollback":1000001}`, `{"scrollback":0}x`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readScrollback(path); err == nil {
			t.Fatalf("worker accepted corrupt settings %q", invalid)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != invalid {
			t.Fatalf("worker changed corrupt file: %q, %v", data, err)
		}
		restarted := &manager{settingsPath: path}
		if err := restarted.loadSettings(); err != nil || restarted.currentScrollback() != defaultScrollback {
			t.Fatalf("server did not recover default: %v", err)
		}
		backups, err := filepath.Glob(path + ".corrupt-*")
		if err != nil || len(backups) != 1 {
			t.Fatalf("corrupt backups = %v, %v", backups, err)
		}
		if data, err := os.ReadFile(backups[0]); err != nil || string(data) != invalid {
			t.Fatalf("corrupt backup = %q, %v", data, err)
		}
	}
	if err := (&manager{settingsPath: t.TempDir()}).loadSettings(); err == nil {
		t.Fatal("server ignored a settings I/O error")
	}
	// A valid source name too long for the added backup suffix forces rename to fail.
	path := filepath.Join(t.TempDir(), strings.Repeat("x", 250))
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&manager{settingsPath: path}).loadSettings(); err == nil {
		t.Fatal("server ignored a settings backup error")
	}
}

func TestSessionScrollbackGridsAndRPC(t *testing.T) {
	dir := t.TempDir()
	paths := sessionPaths{histDir: dir, settingsPath: filepath.Join(dir, "settings.json")}
	config := &workerConfig{Argv: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: dir, Dims: viewerDims{Cols: 80, Rows: 24}}
	s, err := startSession(paths, newID(), config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.shutdown)

	host := &workerHost{id: s.id, paths: paths, session: s}
	rpc := func(lines *int) handshakeAck {
		t.Helper()
		server, client := net.Pipe()
		defer client.Close()
		go host.handleConn(newFrameConn(server))
		c := newFrameConn(client)
		if err := c.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: scrollbackOp, UUID: s.id, Scrollback: lines})); err != nil {
			t.Fatal(err)
		}
		kind, body, err := readFrameDeadline(client, frameBudget)
		var ack handshakeAck
		if err != nil || kind != ftHandshakeAck || json.Unmarshal(body, &ack) != nil || ack.UUID != s.id {
			t.Fatalf("RPC ack: %v %s %v", kind, body, err)
		}
		return ack
	}
	writeLines := func(start, count int) {
		t.Helper()
		var output bytes.Buffer
		for i := start; i < start+count; i++ {
			fmt.Fprintf(&output, "history-%06d %s\r\n", i, strings.Repeat("x", 64))
		}
		s.handleOutput(output.Bytes())
	}
	check := func(lines int, minRows, maxRows uint, present, absent string) {
		t.Helper()
		s.gridMu.Lock()
		defer s.gridMu.Unlock()
		for name, term := range map[string]*libghostty.Terminal{"grid": s.grid, "shadow": s.shadow} {
			limit, err := term.ScrollbackMaxLines()
			if err != nil || (lines == 0 && limit != nil) || (lines != 0 && (limit == nil || *limit != uint(lines))) {
				t.Fatalf("%s line limit = %v err=%v want=%d", name, limit, err, lines)
			}
			byteLimit, err := term.ScrollbackMaxBytes()
			if err != nil || byteLimit != nil {
				t.Fatalf("%s byte limit = %v err=%v", name, byteLimit, err)
			}
			rows, err := term.ScrollbackRows()
			if err != nil || rows < minRows || rows > maxRows {
				t.Fatalf("%s history = %d rows, want [%d,%d], err=%v", name, rows, minRows, maxRows, err)
			}
			snapshot, err := formatVT(term)
			if err != nil || !bytes.Contains(snapshot, []byte(present)) || (absent != "" && bytes.Contains(snapshot, []byte(absent))) {
				t.Fatalf("%s snapshot expected %q and excluded %q: %v", name, present, absent, err)
			}
		}
	}
	apply := func(lines int) {
		t.Helper()
		if err := atomicWrite(paths.settingsPath, mustJSON(scrollbackSetting{Scrollback: lines})); err != nil {
			t.Fatal(err)
		}
		if ack := rpc(&lines); !ack.OK {
			t.Fatalf("set %d: %+v", lines, ack)
		}
	}

	writeLines(1, 2000)
	check(10000, 1900, 2010, "history-000001", "")
	belowRange, aboveRange := -1, maxScrollback+1
	for _, lines := range []*int{nil, &belowRange, &aboveRange} {
		if ack := rpc(lines); ack.OK || ack.Reason == "" {
			t.Fatalf("invalid parameters accepted: %+v", ack)
		}
	}
	missing := 200
	if ack := rpc(&missing); ack.OK || ack.Reason != "配置已过期" {
		t.Fatalf("missing-file default did not reject stale RPC: %+v", ack)
	}
	defaultValue := defaultScrollback
	if ack := rpc(&defaultValue); !ack.OK {
		t.Fatalf("missing-file default rejected matching RPC: %+v", ack)
	}

	apply(200)
	check(200, 1, 1000, "history-002000", "history-000001")
	writeLines(2001, 1000)
	check(200, 1, 1000, "history-003000", "history-002000")
	apply(5000)
	check(5000, 1, 1000, "history-003000", "history-000001")
	apply(0)
	if ack := rpc(&missing); ack.OK || ack.Reason != "配置已过期" {
		t.Fatalf("late RPC accepted: %+v", ack)
	}
	if err := os.WriteFile(paths.settingsPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ack := rpc(&missing); ack.OK || !strings.Contains(ack.Reason, "读取回滚设置失败") {
		t.Fatalf("corrupt settings permitted an update: %+v", ack)
	}
	if err := os.Remove(paths.settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.settingsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if ack := rpc(&missing); ack.OK || !strings.Contains(ack.Reason, "读取回滚设置失败") {
		t.Fatalf("settings I/O failure permitted an update: %+v", ack)
	}
	check(0, 1, 1000, "history-003000", "history-000001")
	writeLines(3001, 12500)
	check(0, 12500, 13500, "history-015500", "history-000001")
}

func TestScrollbackBroadcastClassificationAndOrdering(t *testing.T) {
	m := testManager(t)
	for _, reason := range []string{"未知 worker 操作", "会话正在结束", "bad identity"} {
		id := newID()
		socket := filepath.Join(m.workersDir, id+".sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		h := &workerHandle{uuid: id, socketPath: socket}
		m.sessions[id] = h
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				fc := newFrameConn(conn)
				kind, payload, err := readFrameDeadline(conn, frameBudget)
				var req handshakeReq
				if err == nil && kind == ftHandshake && json.Unmarshal(payload, &req) == nil {
					ack := handshakeAck{Op: req.Op, UUID: id, Reason: reason}
					if reason == "bad identity" {
						ack.UUID = "other"
					}
					_ = fc.writeFrame(ftHandshakeAck, mustJSON(ack))
				}
				_ = fc.close()
			}
		}()
		t.Cleanup(func() {
			_ = listener.Close()
			<-done
			delete(m.sessions, id)
		})
	}
	h, err := createTestShell(m, "broadcast")
	if err != nil {
		t.Fatal(err)
	}
	// Both requests may win the lock first. The resulting disk value and worker
	// must agree, and both broadcasts must have observed all four handles.
	var concurrent sync.WaitGroup
	results := make(chan settingsResult, 2)
	for _, lines := range []int{200, 0} {
		concurrent.Go(func() {
			reply := putScrollback(m, fmt.Sprintf(`{"scrollback":%d}`, lines))
			var result settingsResult
			if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &result) != nil {
				t.Errorf("concurrent PUT %d: %d %s", lines, reply.Code, reply.Body)
			}
			results <- result
		})
	}
	concurrent.Wait()
	close(results)
	for result := range results {
		if result.Applied != 1 || result.Unsupported != 1 || result.Failed != 2 {
			t.Fatalf("broadcast counts across four handles: %+v", result)
		}
	}
	if len(m.sessions) != 4 {
		t.Fatalf("rejected replies dropped handles: %d/4 left", len(m.sessions))
	}
	lines, err := readScrollback(m.settingsPath)
	if err != nil || lines != m.currentScrollback() {
		t.Fatalf("disk and manager differ: %d %d %v", lines, m.currentScrollback(), err)
	}
	assertWorkerHistory(t, h.socketPath, h.uuid, nil, lines == 0)
}

// waitSettingsReader opens a FIFO writer only once a real worker is reading it.
// Renaming a new settings file over the path then leaves that exact read pending.
func waitSettingsReader(t *testing.T, path string) *os.File {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err == nil {
			writer := os.NewFile(uintptr(fd), path)
			t.Cleanup(func() { _ = writer.Close() })
			return writer
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker never opened %s", path)
	return nil
}

const historyCommand = "i=1; while [ \"$i\" -le 2500 ]; do printf 'SCROLL-LINE-%06d\\n' \"$i\"; i=$((i+1)); done; printf 'SCROLL-FINISHED\\n'"

func assertWorkerHistory(t *testing.T, socket, id string, first *frameConn, keepOldest bool) {
	t.Helper()
	if first == nil {
		c, ack, err := attachWorker(socket, id)
		if err != nil || !ack.OK {
			t.Fatalf("attach for history output: %+v %v", ack, err)
		}
		defer c.close()
		first = c
	}
	if err := first.writeFrame(ftInput, []byte(historyCommand+"\r")); err != nil {
		t.Fatal(err)
	}
	readOutput := func(c *frameConn) string {
		t.Helper()
		_ = c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var all bytes.Buffer
		for !(bytes.Contains(all.Bytes(), []byte("SCROLL-LINE-002500")) && bytes.Contains(all.Bytes(), []byte("SCROLL-FINISHED"))) {
			kind, body, err := c.readFrame()
			if err != nil {
				t.Fatalf("waiting for numbered history after %d bytes: %v", all.Len(), err)
			}
			if kind == ftOutput {
				all.Write(body)
			}
		}
		return all.String()
	}
	readOutput(first)
	replay, ack, err := attachWorker(socket, id)
	if err != nil || !ack.OK {
		t.Fatalf("attach for replay: %+v %v", ack, err)
	}
	defer replay.close()
	output := readOutput(replay)
	if strings.Contains(output, "SCROLL-LINE-000001") != keepOldest {
		t.Fatalf("oldest line retention: got %t want %t, %d replay bytes",
			!keepOldest, keepOldest, len(output))
	}
}

func TestWebCreateScrollbackABA(t *testing.T) {
	m := testManager(t)
	requireScrollbackPut(t, m, 200)
	if err := os.Remove(m.settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(m.settingsPath, 0o600); err != nil {
		t.Fatal(err)
	}
	type created struct {
		handle *workerHandle
		err    error
	}
	createdWorker := make(chan created, 1)
	go func() {
		h, err := createTestShell(m, "aba")
		createdWorker <- created{h, err}
	}()
	writer := waitSettingsReader(t, m.settingsPath)
	// Both broadcasts finish while create is still pending. Its blocked file
	// descriptor observes the middle value even after both atomic replacements.
	for _, lines := range []int{0, 200} {
		result := requireScrollbackPut(t, m, lines)
		if result.Applied+result.Unsupported+result.Failed != 0 {
			t.Fatalf("pending worker was in broadcast: %+v", result)
		}
	}
	if _, err := writer.WriteString(`{"scrollback":0}`); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	result := <-createdWorker
	if result.err != nil {
		t.Fatal(result.err)
	}
	assertWorkerHistory(t, result.handle.socketPath, result.handle.uuid, nil, false)
}

func TestCLIStartRechecksScrollbackUnderGridLock(t *testing.T) {
	m := testManager(t)
	dir := filepath.Dir(m.settingsPath)
	paths, id, socket := startTestCLIWorker(t, dir, dir, "/bin/sh", "-i")
	lines := defaultScrollback
	if ack, err := workerOp(socket, handshakeReq{Op: scrollbackOp, UUID: id, Scrollback: &lines}, frameBudget); err != nil || ack.OK || ack.Reason != "会话尚未就绪" {
		t.Fatalf("pre-start ack: %+v %v", ack, err)
	}
	if err := syscall.Mkfifo(paths.settingsPath, 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	first := newFrameConn(conn)
	defer first.close()
	if err := first.writeFrame(ftHandshake, mustJSON(handshakeReq{Op: "start", UUID: id})); err != nil {
		t.Fatal(err)
	}

	initial := waitSettingsReader(t, paths.settingsPath)
	next := paths.settingsPath + ".next"
	if err := syscall.Mkfifo(next, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, paths.settingsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := initial.WriteString(`{"scrollback":200}`); err != nil {
		t.Fatal(err)
	}
	_ = initial.Close()
	recheck := waitSettingsReader(t, paths.settingsPath)
	defer recheck.Close()
	if ack, err := probeWorker(socket, id); err != nil || !ack.readyFor(id) {
		t.Fatalf("CLI recheck happened before publication: %+v %v", ack, err)
	}
	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() { saved <- putScrollback(m, `{"scrollback":200}`) }()
	for deadline := time.Now().Add(frameBudget); time.Now().Before(deadline); {
		stat, err := os.Stat(paths.settingsPath)
		if err == nil && stat.Mode().IsRegular() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case reply := <-saved:
		t.Fatalf("PUT broadcast bypassed the recheck lock: %s", reply.Body)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := recheck.WriteString(`{"scrollback":0}`); err != nil {
		t.Fatal(err)
	}
	_ = recheck.Close()
	reply := <-saved
	var result settingsResult
	if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &result) != nil || result.Applied != 1 {
		t.Fatalf("PUT after recheck: %d %s", reply.Code, reply.Body)
	}
	for _, want := range []frameType{ftHandshakeAck, ftReady} {
		if kind, body, err := readFrameDeadline(conn, frameBudget); kind != want || err != nil {
			t.Fatalf("CLI preamble after recheck: %v %s %v", kind, body, err)
		}
	}
	assertWorkerHistory(t, socket, id, first, false)
}
