package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func cliTestWorker(t *testing.T, argv ...string) (paths sessionPaths, id, socket string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "limpet-worker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return startTestCLIWorker(t, dir, dir, argv...)
}

func startTestCLIWorker(t *testing.T, dir, cwd string, argv ...string) (paths sessionPaths, id, socket string) {
	t.Helper()
	paths = sessionPaths{histDir: filepath.Join(dir, "history"), socketDir: filepath.Join(dir, "workers")}
	id = newID()
	socket = filepath.Join(paths.socketDir, id+".sock")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()
	cmd := exec.Command(os.Args[0], "worker", id)
	cmd.Env = append(os.Environ(), "LIMPET_TEST_WORKER_HOME="+dir, workerConfigEnv+"=1", "TERM=xterm-256color")
	cmd.ExtraFiles = []*os.File{r}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(socket); err == nil {
			_, _ = killWorker(socket, id)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Error("测试 worker 未退出")
		}
	})
	_ = r.Close()
	if err := json.NewEncoder(w).Encode(workerConfig{
		Argv: argv, Cwd: cwd, Dims: viewerDims{Cols: 140, Rows: 40}, Name: "test-cli", WaitFirstViewer: true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ack, err := probeWorker(socket, id)
		if err == nil && ack.OK {
			if ack.Ready == nil || *ack.Ready {
				t.Fatalf("门闩未认领，probe = %+v", ack)
			}
			return paths, id, socket
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker socket 未就绪: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitWorkerSocketGone(t *testing.T, socket string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker socket 未移除: %s", socket)
}

func TestCLIWorkerGate(t *testing.T) {
	for _, op := range []string{"kill", "timeout", "failed-start"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			paths, id, socket := cliTestWorker(t, "/missing-limpet-test-command")
			fc, ack, err := attachWorker(socket, id)
			if err != nil || ack.OK {
				t.Fatalf("未认领的 worker 接受普通 attach: %+v %v", ack, err)
			}
			_ = fc.close()
			switch op {
			case "kill":
				// A partial handshake must not delay a concurrent probe or kill.
				c, err := net.Dial("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				_, _ = c.Write([]byte{byte(ftHandshake)})
				if ack, err := probeWorker(socket, id); err != nil || ack.readyFor(id) {
					t.Fatalf("等待期 probe = %+v, %v", ack, err)
				}
				if ack, err := killWorker(socket, id); err != nil || !ack.OK {
					t.Fatalf("等待期 kill = %+v, %v", ack, err)
				}
			case "failed-start":
				fc, ack, err := openWorker(socket, id, "start", frameBudget)
				if err != nil || ack.OK || !strings.Contains(ack.Reason, "启动") {
					t.Fatalf("失败命令的握手 = %+v, %v", ack, err)
				}
				_ = fc.close()
			}
			waitWorkerSocketGone(t, socket, firstViewerWait+time.Second)
			for _, ext := range []string{".log", ".meta"} {
				if _, err := os.Stat(filepath.Join(paths.histDir, id+ext)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("启动前退出后遗留 %s: %v", ext, err)
				}
			}
		})
	}
}

func TestCLIWorkerOutputAndExit(t *testing.T) {
	for _, tc := range []struct {
		name, script, out string
		code              int
	}{
		{"exit0", "printf done", "done", 0},
		{"exit42", "printf done; exit 42", "done", 42},
		{"signal", "printf done; kill -TERM $$", "done", 128 + int(syscall.SIGTERM)},
		{"orphan", `setsid /bin/sh -c 'trap "" HUP TERM; : > ready; exec sleep 25' & while [ ! -f ready ]; do sleep .01; done; printf done; exit 42`, "done", 42},
		{"query", "stty raw -echo; stty size; printf '\\033[6n'; x=$(dd bs=1 count=6 2>/dev/null); printf '<%s>' \"$x\"", "<\x1b[7;9R>", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			paths, id, socket := cliTestWorker(t, "/bin/sh", "-c", tc.script)
			fc, ack, err := openWorker(socket, id, "start", frameBudget)
			if err != nil || !ack.OK {
				t.Fatalf("start = %+v, %v", ack, err)
			}
			defer fc.close()
			_ = fc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if kind, _, err := fc.readFrame(); err != nil || kind != ftReady {
				t.Fatalf("start 后首帧 = %v, %v", kind, err)
			}

			var output strings.Builder
			replied := false
			for {
				kind, body, err := fc.readFrame()
				if err != nil {
					t.Fatal(err)
				}
				if kind == ftExit {
					var exit exitMsg
					if err := json.Unmarshal(body, &exit); err != nil || exit.Reason != "command-exit" ||
						exit.ExitCode == nil || *exit.ExitCode != tc.code {
						t.Fatalf("退出帧 = %s, %v", body, err)
					}
					break
				}
				if kind != ftOutput {
					t.Fatalf("意外输出帧 %v", kind)
				}
				output.Write(body)
				if tc.name == "query" && !replied && strings.Contains(output.String(), "\x1b[6n") {
					// A distinguishable real viewer response must be the first reply.
					if err := fc.writeFrame(ftInput, []byte("\x1b[7;9R")); err != nil {
						t.Fatal(err)
					}
					replied = true
				}
			}
			if !strings.Contains(output.String(), tc.out) || (tc.name == "query" && !strings.Contains(output.String(), "40 140")) {
				t.Fatalf("输出 = %q，期待 %q", output.String(), tc.out)
			}
			waitWorkerSocketGone(t, socket, 3*time.Second)
			meta, err := os.ReadFile(filepath.Join(paths.histDir, id+".meta"))
			if err != nil || !strings.Contains(string(meta), "S\t0\t140x40\n") {
				t.Fatalf("录制初始尺寸 = %q，%v", meta, err)
			}
			if _, _, err := fc.readFrame(); !errors.Is(err, io.EOF) {
				t.Fatalf("EXIT 后 socket 应关闭，收到 %v", err)
			}
		})
	}
}

func TestCLIWorkerActiveSize(t *testing.T) {
	_, id, socket := cliTestWorker(t, "/bin/sh", "-c",
		"trap 'stty size' WINCH; while :; do if read line; then stty size; fi; done")
	fc, ack, err := openWorker(socket, id, "start", frameBudget)
	if err != nil || !ack.OK {
		t.Fatalf("start = %+v, %v", ack, err)
	}
	defer fc.close()
	_ = fc.c.SetReadDeadline(time.Now().Add(frameBudget))
	if typ, _, err := fc.readFrame(); err != nil || typ != ftReady {
		t.Fatalf("ready = %v, %v", typ, err)
	}
	waitOutput := func(want string) {
		t.Helper()
		var b strings.Builder
		for !strings.Contains(b.String(), want) {
			kind, body, err := fc.readFrame()
			if err != nil {
				t.Fatalf("等待 %q，已见 %q，帧 %v，错误 %v", want, b.String(), kind, err)
			}
			switch kind {
			case ftOutput:
				b.Write(body)
			case ftSize:
				_, dims, ok := decodeResize(body)
				if !ok || !dims.valid() {
					t.Fatalf("无效尺寸通知 %q", body)
				}
			default:
				t.Fatalf("等待 %q 时收到意外帧 %v", want, kind)
			}
		}
	}
	send := func(c *frameConn, typ frameType, data []byte) {
		t.Helper()
		if err := c.writeFrame(typ, data); err != nil {
			t.Fatal(err)
		}
	}
	send(fc, ftInput, []byte("\n"))
	waitOutput("40 140")
	send(fc, ftResize, []byte(`{}`))
	send(fc, ftResize, []byte(`{"cols":0,"rows":null}`))
	send(fc, ftResize, mustJSON(resizeMsg{Cols: 110, Rows: 31}))
	waitOutput("31 110")
	peer, ack, err := attachWorker(socket, id)
	if err != nil || !ack.OK {
		t.Fatalf("attach = %+v, %v", ack, err)
	}
	defer peer.close()
	// An undeclared legacy viewer still becomes the owner as in Spec 006.
	send(peer, ftInput, []byte("\n"))
	waitOutput("31 110")
	send(peer, ftResize, mustJSON(resizeMsg{Cols: 105, Rows: 32}))
	waitOutput("32 105")
	// Clearing the active viewer keeps the current grid. Its later input leaves the CLI owner intact.
	send(peer, ftResize, mustJSON(resizeMsg{}))
	send(peer, ftInput, []byte("\n"))
	waitOutput("32 105")
	send(fc, ftInput, []byte("\n"))
	waitOutput("31 110")
	send(peer, ftInput, []byte("\n"))
	waitOutput("31 110")
	send(fc, ftResize, mustJSON(resizeMsg{Cols: 115, Rows: 33}))
	waitOutput("33 115")

	send(peer, ftResize, mustJSON(resizeMsg{Cols: 51, Rows: 36}))
	send(fc, ftInput, []byte("\n"))
	waitOutput("33 115")
	send(peer, ftInput, []byte("\n"))
	waitOutput("36 51")
}
