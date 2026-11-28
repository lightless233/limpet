package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	paths = sessionPaths{
		histDir: filepath.Join(dir, "history"), socketDir: filepath.Join(dir, "workers"),
		settingsPath: filepath.Join(dir, "settings.json"),
	}
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

// serveViewerTestSession 构造只够 serveViewer 运行的最小 session（不启动 PTY）。
func serveViewerTestSession() *session {
	return &session{
		id:         newID(),
		createdAt:  time.Now(),
		viewers:    make(map[*viewer]struct{}),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
		queryQueue: make(chan []byte, 1),
	}
}

// pumpViewer 把 serveViewer 接到一对 net.Pipe 上跑，返回测试侧连接与泵退出信号。
func pumpViewer(t *testing.T) (client net.Conn, v *viewer, s *session, pumpDone chan struct{}) {
	t.Helper()
	s = serveViewerTestSession()
	server, client := net.Pipe()
	v = newViewer(newFrameConn(server), 0)
	v.dims = viewerDims{Cols: 80, Rows: 24}
	s.viewerWG.Add(1)
	pumpDone = make(chan struct{})
	go func() {
		s.serveViewer(v, handshakeAck{}, false)
		close(pumpDone)
	}()
	t.Cleanup(func() {
		// 先撤测试侧：未读完的帧（如 EXIT）让写侧立即出错返回，泵得以退出。
		_ = client.Close()
		select {
		case <-pumpDone:
		case <-time.After(3 * time.Second):
			_ = server.Close()
			<-pumpDone
			t.Error("serveViewer 未在 3s 内退出")
		}
	})
	return client, v, s, pumpDone
}

// emit 按生产侧（handleOutput）的契约入队输出：先计入积压字节，再入队。
func emit(v *viewer, data string) {
	v.queued.Add(int64(len(data)))
	v.queue <- viewerEvent{data: []byte(data)}
}

func readFrameDeadline(c net.Conn, within time.Duration) (frameType, []byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(within))
	return readFrame(c)
}

// skipPreamble 消费握手应答与 ready 帧，返回首个数据帧。
func skipPreamble(t *testing.T, c net.Conn) (frameType, []byte, error) {
	t.Helper()
	for {
		kind, body, err := readFrameDeadline(c, 3*time.Second)
		if err != nil {
			t.Fatalf("等待数据帧: %v", err)
		}
		if kind != ftHandshakeAck && kind != ftReady {
			return kind, body, err
		}
	}
}

// 验收①：连续高频小包合并成更少的 ftOutput 帧，字节无丢失、顺序一致。
// 先落一帧首包让 lastFlush 生效，随后并发实时读帧（net.Pipe 无缓冲，若等发送
// 完再读，泵会卡在写管道上，旧版无时间窗的 drain 也能把积压合并成极少帧，
// 测试就失去分辨力），主协程以 ~1ms 间隔持续发碎包，使其全部落入 16ms 合批窗口。
func TestServeViewerBatchesBurstOutput(t *testing.T) {
	client, v, _, _ := pumpViewer(t)
	emit(v, "h")
	kind, body, err := skipPreamble(t, client)
	if err != nil || kind != ftOutput || string(body) != "h" {
		t.Fatalf("首帧 = %v %q, %v", kind, body, err)
	}
	const chunks = 300
	var want strings.Builder
	for i := 0; i < chunks; i++ {
		want.WriteString("abcd")
	}
	type burstReading struct {
		out    string
		frames int
		err    error
	}
	resCh := make(chan burstReading, 1)
	go func() {
		var got strings.Builder
		frames := 0
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			k, b, rerr := readFrame(client)
			if rerr != nil {
				if errors.Is(rerr, os.ErrDeadlineExceeded) {
					rerr = nil // 读限超时即输出已安静（合批窗口早已结束）
				}
				resCh <- burstReading{out: got.String(), frames: frames, err: rerr}
				return
			}
			_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			switch k {
			case ftOutput:
				got.Write(b)
				frames++
			default:
				resCh <- burstReading{out: got.String(), frames: frames, err: fmt.Errorf("意外帧 %v: %q", k, b)}
				return
			}
		}
	}()
	for i := 0; i < chunks; i++ {
		emit(v, "abcd")
		time.Sleep(time.Millisecond)
	}
	var res burstReading
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("并发读帧未收尾")
	}
	if res.err != nil {
		t.Fatalf("读帧失败: %v", res.err)
	}
	if res.out != want.String() {
		t.Fatalf("输出字节不一致: got %d bytes, want %d bytes", len(res.out), want.Len())
	}
	// 旧版逐包直发（drain 至多并掉排队中的相邻包）会有近 chunks 帧；
	// 16ms 时间窗合批应远低于 chunks/5。
	if res.frames >= chunks/5 {
		t.Fatalf("%d 个碎包写出了 %d 帧，未有效合批", chunks, res.frames)
	}
	if queued := v.queued.Load(); queued != 0 {
		t.Errorf("合批后 queued 未清零: %d", queued)
	}
}

// 验收②：交错的 ftOutput + ftSize + ftOutput 严格保序，尺寸不能越过前后的输出字节。
func TestServeViewerOrdersSizeBetweenOutput(t *testing.T) {
	client, v, s, _ := pumpViewer(t)
	code := 0
	s.end = exitMsg{Reason: "command-exit", ExitCode: &code}
	// 先落一帧 head，使后续输出进入合批等待；尺寸事件在等待期间到达时
	// 必须先刷出已累积字节（A），再写尺寸，随后才是 B。
	emit(v, "head")
	kind, body, err := skipPreamble(t, client)
	if err != nil || kind != ftOutput || string(body) != "head" {
		t.Fatalf("首帧 = %v %q, %v", kind, body, err)
	}
	emit(v, "A")
	v.queue <- viewerEvent{size: resizeMsg{Cols: 90, Rows: 30}}
	emit(v, "B")
	close(v.queue)

	var seg strings.Builder
	sized := false
	for {
		kind, body, err = readFrameDeadline(client, 3*time.Second)
		if err != nil {
			t.Fatalf("读帧失败: %v", err)
		}
		switch kind {
		case ftOutput:
			seg.Write(body)
			if !sized {
				// head 之后到尺寸之前只允许 A（可整段或碎段到达）。
				if !strings.HasPrefix("A", seg.String()) {
					t.Fatalf("尺寸帧前的输出字节越序: %q", seg.String())
				}
			} else if !strings.HasPrefix("B", seg.String()) {
				t.Fatalf("尺寸帧后的输出字节越序: %q", seg.String())
			}
		case ftSize:
			_, dims, ok := decodeResize(body)
			if !ok || dims.Cols != 90 || dims.Rows != 30 {
				t.Fatalf("尺寸帧载荷 = %q", body)
			}
			if sized {
				t.Fatal("意外的第二个尺寸帧")
			}
			if seg.String() != "A" {
				t.Fatalf("尺寸帧前的输出字节 = %q, want A", seg.String())
			}
			sized = true
			seg.Reset()
		case ftExit:
			if !sized || seg.String() != "B" {
				t.Fatalf("退出前状态不符: sized=%v seg=%q", sized, seg.String())
			}
			return
		default:
			t.Fatalf("意外帧 %v: %q", kind, body)
		}
	}
}

// 验收③：输出后立刻退出时，尾部字节与 ftExit 完整送达且不被合批窗口拖延。
func TestServeViewerFlushesTailBeforeExit(t *testing.T) {
	client, v, s, _ := pumpViewer(t)
	code := 7
	s.end = exitMsg{Reason: "command-exit", ExitCode: &code}
	// 先落一帧，使尾部输出落在合批窗口内：退出不得干等定时器。
	emit(v, "head")
	kind, body, err := skipPreamble(t, client)
	if err != nil || kind != ftOutput || string(body) != "head" {
		t.Fatalf("首帧 = %v %q, %v", kind, body, err)
	}
	emit(v, "tail")
	started := time.Now()
	close(v.queue)

	var got strings.Builder
	var exit exitMsg
	for {
		kind, body, err = readFrameDeadline(client, 2*time.Second)
		if err != nil {
			t.Fatalf("等待尾部帧与退出: %v", err)
		}
		if kind == ftExit {
			if err := json.Unmarshal(body, &exit); err != nil {
				t.Fatalf("退出帧载荷 = %q, %v", body, err)
			}
			break
		}
		if kind != ftOutput {
			t.Fatalf("意外帧 %v: %q", kind, body)
		}
		got.Write(body)
	}
	if got.String() != "tail" {
		t.Fatalf("尾部字节 = %q, want tail", got.String())
	}
	if exit.Reason != "command-exit" || exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Fatalf("退出帧 = %+v", exit)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("退出被拖延: %v", elapsed)
	}
}

// 验收③补充：合批等待期间 viewer 被丢弃（done 触发）时立即返回，不阻塞丢弃路径。
func TestServeViewerDroppedDuringBatchWait(t *testing.T) {
	client, v, _, pumpDone := pumpViewer(t)
	// 先落一帧使后续输出进入合批等待，再触发丢弃。
	emit(v, "head")
	kind, body, err := skipPreamble(t, client)
	if err != nil || kind != ftOutput || string(body) != "head" {
		t.Fatalf("首帧 = %v %q, %v", kind, body, err)
	}
	emit(v, "burst")
	v.closeWith(4001, "too slow")
	started := time.Now()
	// DETACH 是尽力而为：readViewer 在 closed 置位后退出并关闭连接，可能与
	// 输出泵的 DETACH 写入竞争。收到则校验，EOF 也算符合契约——关键是泵不阻塞、及时退出。
	for {
		kind, body, err = readFrameDeadline(client, 3*time.Second)
		if err != nil {
			break
		}
		if kind == ftDetach {
			var dm detachMsg
			if err := json.Unmarshal(body, &dm); err != nil || dm.Code != 4001 {
				t.Fatalf("DETACH 载荷 = %q, %v", body, err)
			}
			break
		}
	}
	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("丢弃后 serveViewer 未及时退出")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("丢弃路径被拖延: %v", elapsed)
	}
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
				fc, ack, err := openWorker(socket, handshakeReq{Op: "start", UUID: id}, frameBudget)
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
			fc, ack, err := openWorker(socket, handshakeReq{Op: "start", UUID: id}, frameBudget)
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
	fc, ack, err := openWorker(socket, handshakeReq{Op: "start", UUID: id}, frameBudget)
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

func TestIsTerminalReportOnly(t *testing.T) {
	reports := []string{
		"\x1b[?0u",
		"\x1b[?5u",
		"\x1b[?62;22c",
		"\x1b[>1;10;0c",
		"\x1b[?2026;2$y",
		"\x1b[7;9R",
		"\x1b[0n",
		"\x1b[4;600;800t",
		"\x1b[6;16;8t",
		"\x1b]11;rgb:1212/1414/1717\x07",
		"\x1b]10;rgb:e6e6/eaea/f0f0\x1b\\",
		"\x1b[I",
		"\x1b[O",
		"\x1bP>|ghostty\x1b\\",
		"\x1b_Gi=1;OK\x1b\\",
		"\x1b[?5u\x1b[?2026;2$y",
	}
	for _, in := range reports {
		if !isTerminalReportOnly([]byte(in)) {
			t.Errorf("isTerminalReportOnly(%q) = false, want true", in)
		}
	}

	userInputs := []string{
		"",
		"a",
		"\r",
		"\n",
		"\x1b",
		"\x1b[A",
		"\x1b[13u",
		"\x1b[97;5u",
		"\x1b[<0;10;5M",
		"\x1b[200~hello\x1b[201~",
		"\x1b[?5ua",
	}
	for _, in := range userInputs {
		if isTerminalReportOnly([]byte(in)) {
			t.Errorf("isTerminalReportOnly(%q) = true, want false", in)
		}
	}
}

func TestTerminalReportDoesNotPingPongSize(t *testing.T) {
	// REPORT 与随后的 stty size 之间隔一个合批窗口：输出泵会把窗口内到达的
	// 连续碎包合并成一帧，紧邻的两个输出不再保证分属两帧。
	_, id, socket := cliTestWorker(t, "/bin/sh", "-c",
		"trap 'stty size' WINCH; stty -echo; printf 'READY\\n'; chunk=$(stty -icanon min 1 time 0; dd bs=64 count=1 2>/dev/null | tr '\\033' 'E'); stty icanon; printf 'REPORT:%s\\n' \"$chunk\"; sleep 0.1; stty size; while :; do if IFS= read -r line; then stty size; fi; done")
	fc, ack, err := openWorker(socket, handshakeReq{Op: "start", UUID: id}, frameBudget)
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

	waitOutput("READY")
	peer, ack, err := attachWorker(socket, id)
	if err != nil || !ack.OK {
		t.Fatalf("attach = %+v, %v", ack, err)
	}
	defer peer.close()

	send(peer, ftResize, mustJSON(resizeMsg{Cols: 184, Rows: 62}))
	send(peer, ftInput, []byte("\x1b[?5u"))
	waitOutput("REPORT:E[?5u")
	waitOutput("40 140")

	// fc remains owner without re-sending input; its resize takes effect immediately.
	send(fc, ftResize, mustJSON(resizeMsg{Cols: 110, Rows: 31}))
	waitOutput("31 110")

	// Real user input from peer still promotes peer's size.
	send(peer, ftInput, []byte("x\n"))
	waitOutput("62 184")
}
