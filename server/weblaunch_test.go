package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func requestTerm(m *manager, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/term", strings.NewReader(body)))
	return w
}

func createWeb(t *testing.T, m *manager, body string) map[string]string {
	t.Helper()
	w := requestTerm(m, body)
	var reply map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || !uuidRe.MatchString(reply["id"]) {
		t.Fatalf("POST /api/term = %d %s", w.Code, w.Body.Bytes())
	}
	return reply
}

func writeLaunchFixture(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func loginFixture(t *testing.T) string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SHELL", zsh)
	writeLaunchFixture(t, filepath.Join(home, ".zshrc"), `export PATH="$HOME/bin:$PATH"
codex() { printf '%s\n' codex-function >> "$HOME/preset-trap"; return 1; }
agy() { printf '%s\n' agy-function >> "$HOME/preset-trap"; return 1; }
alias codex='printf "%s\n" codex-alias >> "$HOME/preset-trap"; false'
alias agy='printf "%s\n" agy-alias >> "$HOME/preset-trap"; false'
export LIMPET_TEST_RC=loaded
`, 0o600)
	return bin
}

func TestWebPresetLaunchAndRecover(t *testing.T) {
	m := testManager(t)
	bin := loginFixture(t)
	cwd := filepath.Join(filepath.Dir(bin), "project ; name")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ preset, flag string }{
		{"codex", "--yolo"}, {"agy", "--dangerously-skip-permissions"},
	} {
		shim := "#!/bin/sh\nprintf '%s\\n' \"$PWD|$1|${LIMPET_WEB_LAUNCH-unset}|${LIMPET_TEST_RC-unset}\" > preset-result\nexec sleep 30\n"
		writeLaunchFixture(t, filepath.Join(bin, entry.preset), shim, 0o700)
		reply := createWeb(t, m, fmt.Sprintf("{\"cwd\":%q,\"preset\":%q}", "~/project ; name", entry.preset))
		id := reply["id"]
		want := map[string]string{"id": id, "name": "project ; name-" + entry.preset, "cwd": "~/project ; name", "cmd": entry.preset}
		if !reflect.DeepEqual(reply, want) {
			t.Fatalf("POST = %v, want %v", reply, want)
		}
		data, err := os.ReadFile(filepath.Join(cwd, "preset-result"))
		if want := cwd + "|" + entry.flag + "|unset|loaded\n"; err != nil || string(data) != want {
			t.Fatalf("PTY startup = %q, %v, want %q", data, err, want)
		}
		if trap, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "preset-trap")); !os.IsNotExist(err) {
			t.Fatalf("interactive preset trap = %q, %v", trap, err)
		}
		if err := os.Remove(filepath.Join(cwd, "preset-result")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(m.workersDir, id+".launch.json")); !os.IsNotExist(err) {
			t.Fatalf("launch file not consumed: %v", err)
		}
		// Simulate startup with a registry missing this live worker.
		recovered := &manager{
			workersDir: m.workersDir, regPath: filepath.Join(filepath.Dir(m.regPath), "recovered.json"),
			registry: map[string]regEntry{}, sessions: map[string]*workerHandle{},
		}
		recovered.recoverWorkers()
		got := recovered.registry[id]
		if got.Name != want["name"] || got.Cwd != cwd || got.Cmd != entry.preset || got.CreatedAt.IsZero() {
			t.Fatalf("recover from probe = %+v", got)
		}
		if err := recovered.renameSession(id, "renamed"); err != nil {
			t.Fatal(err)
		}
		clear(recovered.sessions)
		recovered.recoverWorkers()
		if got := recovered.registry[id]; got.Name != "renamed" || got.Cwd != cwd || got.Cmd != entry.preset {
			t.Fatalf("recover renamed session = %+v", got)
		}
	}
}

func TestWebPresetRejectedAndEarlyExit(t *testing.T) {
	m := testManager(t)
	bin := loginFixture(t)
	check := func(body string, code int, contains string) {
		t.Helper()
		w := requestTerm(m, body)
		if w.Code != code || !strings.Contains(w.Body.String(), contains) {
			t.Fatalf("POST = %d %s, want %d containing %q", w.Code, w.Body, code, contains)
		}
		if len(m.registry) != 0 || len(m.list()) != 0 || len(m.pendingIDs) != 0 {
			t.Fatal("failed create left a registration")
		}
	}
	check("{\"preset\":\"other\"}", 400, "预设")
	t.Run("unsupported", func(t *testing.T) {
		t.Setenv("SHELL", "/usr/bin/fish")
		check("{\"preset\":\"codex\"}", 500, "fish")
	})
	check("{\"preset\":\"codex\"}", 500, "找不到可执行的")
	if files, err := os.ReadDir(m.workersDir); err != nil || len(files) != 0 {
		t.Fatalf("preflight started a worker: %v, %v", files, err)
	}
	writeLaunchFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\n: > \"$HOME/preset-ran\"\nexit 0\n", 0o700)
	check("{\"preset\":\"codex\"}", 500, "worker")
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "preset-ran")); err != nil {
		t.Fatalf("early-exit shim did not run: %v", err)
	}
}

func TestWebCreateCwdAndConcurrentNames(t *testing.T) {
	m := testManager(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, " a.b ")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLaunchFixture(t, filepath.Join(home, "file"), "", 0o600)
	for _, invalid := range []string{"relative", "~other", "~/missing", "~/file", "~/a\x00b"} {
		payload, _ := json.Marshal(map[string]string{"cwd": invalid})
		if w := requestTerm(m, string(payload)); w.Code != 400 {
			t.Fatalf("cwd %q = %d %s", invalid, w.Code, w.Body)
		}
	}
	for _, body := range []string{"", "{}", "{\"cwd\":\"  \", \"name\":\"  \"}"} {
		reply := createWeb(t, m, body)
		if reply["name"] != reply["id"] || reply["cwd"] != "~" {
			t.Fatalf("legacy POST = %v", reply)
		}
	}
	if reply := createWeb(t, m, "{\"preset\":\"shell\", \"cwd\":\"~\"}"); reply["name"] != "sh" {
		t.Fatalf("home name = %v", reply)
	}
	longLeaf := strings.Repeat("长", 75) + strings.Repeat("a", 24)
	if err := os.Mkdir(filepath.Join(home, longLeaf), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"cwd": "~/" + longLeaf, "preset": "shell"})
	if reply := createWeb(t, m, string(payload)); len([]rune(reply["name"])) != 96 {
		t.Fatalf("long name = %v", reply)
	}
	var tasks sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 3)
	for range 3 {
		tasks.Go(func() {
			results <- requestTerm(m, "{\"cwd\":\"~/x/../ a.b \",\"preset\":\"shell\"}")
		})
	}
	tasks.Wait()
	close(results)
	var names []string
	for w := range results {
		var reply map[string]string
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply["cwd"] != "~/ a.b " {
			t.Fatalf("concurrent POST = %d %s", w.Code, w.Body)
		}
		names = append(names, reply["name"])
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{" a-b -sh", " a-b -sh-2", " a-b -sh-3"}) {
		t.Fatalf("names = %q", names)
	}
	createWeb(t, m, "{\"name\":\"explicit\"}")
	if w := requestTerm(m, "{\"name\":\"explicit\"}"); w.Code != 409 {
		t.Fatalf("explicit duplicate = %d %s", w.Code, w.Body)
	}
}

func TestWebLaunchInvalidSidecar(t *testing.T) {
	m := testManager(t)
	for _, entry := range []struct{ kind, data string }{
		{"missing", ""}, {"malformed", "{"}, {"invalid", "{}"},
	} {
		t.Run(entry.kind, func(t *testing.T) {
			id := newID()
			path := filepath.Join(m.workersDir, id+".launch.json")
			if entry.data != "" {
				writeLaunchFixture(t, path, entry.data, 0o600)
			}
			cmd := exec.Command(m.exePath, "worker", id)
			cmd.Env = append(os.Environ(), webLaunchEnv+"=1")
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "Web 启动配置") {
				t.Fatalf("worker startup = %v %s", err, out)
			}
			for _, suffix := range []string{".launch.json", ".sock"} {
				if _, err := os.Stat(filepath.Join(m.workersDir, id+suffix)); !os.IsNotExist(err) {
					t.Fatalf("%s remains: %v", suffix, err)
				}
			}
		})
	}
}

func TestWebLaunchFailureCleanup(t *testing.T) {
	for _, mode := range []string{"direct-start", "systemd-submit", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t)
			fixtures := t.TempDir()
			t.Setenv("PATH", fixtures+":/usr/bin:/bin")
			t.Setenv("TEST_WEB_WORKERS", m.workersDir)
			if mode == "systemd-submit" {
				m.launch = launchSystemdRun
				writeLaunchFixture(t, filepath.Join(fixtures, "systemd-run"), "#!/bin/sh\nexit 1\n", 0o700)
				writeLaunchFixture(t, filepath.Join(fixtures, "systemctl"), "#!/bin/sh\nunit=${3#limpet-worker-}\nid=${unit%.service}\n[ -f \"$TEST_WEB_WORKERS/$id.launch.json\" ] && : > \"$TEST_WEB_WORKERS/stop-before-remove\"\n", 0o700)
			} else {
				m.exePath = filepath.Join(fixtures, "worker")
				if mode == "timeout" {
					shim := "#!/bin/sh\nfile=\"$TEST_WEB_WORKERS/$2.launch.json\"\n[ \"$(stat -c %a \"$file\")\" = 600 ] && : > \"$TEST_WEB_WORKERS/mode-600\"\ntrap '[ -f \"$file\" ] && : > \"$TEST_WEB_WORKERS/stop-before-remove\"; exit 0' TERM\nwhile :; do sleep 0.1; done\n"
					writeLaunchFixture(t, m.exePath, shim, 0o700)
				}
			}
			if w := requestTerm(m, "{}"); w.Code != 500 {
				t.Fatalf("failed startup = %d %s", w.Code, w.Body)
			}
			files, err := filepath.Glob(filepath.Join(m.workersDir, "*.launch.json"))
			if err != nil || len(files) != 0 {
				t.Fatalf("unconsumed config = %v %v", files, err)
			}
			if mode != "direct-start" {
				if _, err := os.Stat(filepath.Join(m.workersDir, "stop-before-remove")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "timeout" {
				if _, err := os.Stat(filepath.Join(m.workersDir, "mode-600")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWorkspaceHistory(t *testing.T) {
	m := testManager(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"a", "b", "recent"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLaunchFixture(t, filepath.Join(home, "file"), "", 0o600)
	start := time.Now()
	end := start.Add(time.Hour)
	m.registry = map[string]regEntry{
		"legacy": {}, "home": {Cwd: home, CreatedAt: end}, "b": {Cwd: home + "/b", CreatedAt: start},
		"a": {Cwd: home + "/a", CreatedAt: start}, "deleted": {Cwd: home + "/deleted", CreatedAt: end},
		"file": {Cwd: home + "/file", CreatedAt: end}, "old-recent": {Cwd: home + "/recent", CreatedAt: start},
		"recent": {Cwd: home + "/a/../recent", CreatedAt: end, EndedAt: &end},
	}
	w := httptest.NewRecorder()
	m.handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/workspaces", nil))
	if w.Code != 200 || w.Body.String() != "[\"~/recent\",\"~/a\",\"~/b\"]\n" {
		t.Fatalf("GET /api/workspaces = %d %s", w.Code, w.Body)
	}
}

func TestWebCreateRegistryRollback(t *testing.T) {
	m := testManager(t)
	bin := loginFixture(t)
	// Rename of the temporary registry file over a directory fails.
	if err := os.Mkdir(m.regPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLaunchFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\ntrap '' HUP TERM\nexec sleep 30\n", 0o700)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- requestTerm(m, "{\"preset\":\"codex\"}") }()
	var id string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(m.workersDir, "*.sock"))
		if err != nil {
			t.Error(err)
			break
		}
		if len(files) == 1 {
			candidate := strings.TrimSuffix(filepath.Base(files[0]), ".sock")
			ack, err := probeWorker(files[0], candidate)
			if err == nil && ack.Ready != nil && !*ack.Ready {
				id = candidate
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Error("did not observe the worker while rollback was stopping it")
	} else {
		if found := m.get(id); found != nil || len(m.list()) != 0 {
			t.Error("uncommitted worker was published during rollback")
		}
		m.mu.Lock()
		_, pending := m.pendingIDs[id]
		m.mu.Unlock()
		if !pending {
			t.Error("pending ID released during rollback")
		}
	}
	w := <-result
	if w.Code != 500 || !strings.Contains(w.Body.String(), "登记会话失败") {
		t.Fatalf("save failure = %d %s", w.Code, w.Body)
	}
	if len(m.pendingIDs) != 0 || len(m.registry) != 0 || len(m.list()) != 0 {
		t.Fatal("failed create left a registration")
	}
	if files, err := filepath.Glob(filepath.Join(m.workersDir, "*.sock")); err != nil || len(files) != 0 {
		t.Fatalf("socket after rollback = %v, %v", files, err)
	}
}

func TestWebLaunchKeepsWebSessionMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv(webLaunchEnv, "1")
	web, err := webPreset("shell", home)
	if err != nil {
		t.Fatal(err)
	}
	config := &workerConfig{Argv: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: web.Cwd, Dims: viewerDims{Cols: gridCols, Rows: gridRows}}
	s, err := startSession(sessionPaths{histDir: home, settingsPath: filepath.Join(home, "settings.json")}, newID(), config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.shutdown)
	if s.cli {
		t.Fatal("web launch activated CLI resize semantics")
	}
}
