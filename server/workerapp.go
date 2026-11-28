package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	workerConfigEnv = "LIMPET_WORKER_CONFIG"
	webLaunchEnv    = "LIMPET_WEB_LAUNCH"
	firstViewerWait = 10 * time.Second
	// 实时输出的爆发期合批窗口：距上次刷出不足该窗口时，再攒一小段连续碎包
	// 合并成一帧，把高频输出（峰值可达上千帧/秒）压回温和的重绘节奏；
	// 首包与稀疏输出仍零延迟直发。
	viewerBatchWindow = 16 * time.Millisecond
)

type workerConfig struct {
	Argv            []string
	Cwd             string
	Dims            viewerDims
	Name            string
	WaitFirstViewer bool
}

type webLaunchConfig struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Name string   `json:"name"`
	Cmd  string   `json:"cmd"`
}

func readWebLaunch(path string) (webLaunchConfig, error) {
	var config webLaunchConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("读取 Web 启动配置: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return config, fmt.Errorf("删除 Web 启动配置: %w", err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("解析 Web 启动配置: %w", err)
	}
	if len(config.Argv) == 0 || config.Argv[0] == "" || !filepath.IsAbs(config.Cwd) ||
		strings.ContainsRune(config.Cwd, 0) || !nameValid(config.Name) ||
		config.Cmd == "" || config.Cmd == "." || config.Cmd == ".." || filepath.Base(config.Cmd) != config.Cmd ||
		strings.ContainsRune(config.Cmd, 0) {
		return config, fmt.Errorf("Web 启动配置不合法")
	}
	for _, arg := range config.Argv {
		if strings.ContainsRune(arg, 0) {
			return config, fmt.Errorf("Web 启动参数含 NUL")
		}
	}
	return config, nil
}

func readWorkerConfig() (*workerConfig, error) {
	if os.Getenv(workerConfigEnv) == "" {
		return nil, nil
	}
	in := os.NewFile(3, "worker-startup")
	defer in.Close()
	var config workerConfig
	if err := json.NewDecoder(in).Decode(&config); err != nil {
		return nil, fmt.Errorf("读取 worker 启动配置: %w", err)
	}
	if len(config.Argv) == 0 || config.Argv[0] == "" || !filepath.IsAbs(config.Cwd) ||
		!config.Dims.valid() || !nameValid(config.Name) || !config.WaitFirstViewer {
		return nil, fmt.Errorf("worker 启动配置不合法")
	}
	return &config, nil
}

type workerHost struct {
	id       string
	paths    sessionPaths
	config   *workerConfig
	cmd      string
	mu       sync.Mutex
	session  *session
	claimed  bool
	stopping bool
	done     chan struct{}
}

func runWorker(uuid string, paths sessionPaths) error {
	if !uuidRe.MatchString(uuid) {
		return fmt.Errorf("uuid 不合法: %q", uuid)
	}
	var config *workerConfig
	var command string
	if os.Getenv(webLaunchEnv) == "1" {
		web, err := readWebLaunch(filepath.Join(paths.socketDir, uuid+".launch.json"))
		if err != nil {
			return err
		}
		config = &workerConfig{
			Argv: web.Argv, Cwd: web.Cwd, Name: web.Name,
			Dims: viewerDims{Cols: gridCols, Rows: gridRows},
		}
		command = web.Cmd
	} else {
		var err error
		config, err = readWorkerConfig()
		if err != nil {
			return err
		}
		if config != nil {
			command = filepath.Base(config.Argv[0])
		}
	}
	if err := setSubreaper(); err != nil {
		log.Printf("setSubreaper 失败（已孤儿化的任务可能无法回收）: %v", err)
	}
	for _, path := range []string{paths.socketDir, paths.histDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}

	w := &workerHost{id: uuid, paths: paths, config: config, cmd: command, done: make(chan struct{})}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	waitFirstViewer := config != nil && config.WaitFirstViewer
	if !waitFirstViewer {
		s, err := startSession(paths, uuid, config, nil, nil)
		if err != nil {
			return err
		}
		w.claimed = true
		w.session = s
		go w.waitSession(s)
	}

	sockPath := filepath.Join(paths.socketDir, uuid+".sock")
	l, err := listenWorker(sockPath)
	if err != nil {
		w.stop(false)
		<-w.done
		return err
	}
	defer os.Remove(sockPath)
	defer l.Close()
	log.Printf("worker %s 监听 (pid %d, socket %s)", uuid, os.Getpid(), sockPath)

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go w.handleConn(newFrameConn(c))
		}
	}()

	var timeout <-chan time.Time
	if waitFirstViewer {
		timer := time.NewTimer(firstViewerWait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		select {
		case <-timeout:
			w.stop(true)
			timeout = nil
		case <-sig:
			w.stop(false)
		case <-w.done:
			log.Printf("worker %s 退出", uuid)
			return nil
		}
	}
}

func listenWorker(path string) (_ *net.UnixListener, err error) {
	// The pathname exists after bind but before listen. Publish the
	// discoverable name only once a probe can connect.
	staged := strings.TrimSuffix(path, ".sock") + ".tmp"
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: staged, Net: "unix"})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = l.Close()
		}
	}()
	if err := os.Chmod(staged, 0o600); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("worker socket 路径已存在: %s", path)
		}
		return nil, err
	}
	if err := os.Rename(staged, path); err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(false)
	return l, nil
}

func (w *workerHost) waitSession(s *session) {
	<-s.done
	close(w.done)
}

func (w *workerHost) stop(onlyUnclaimed bool) {
	w.mu.Lock()
	if w.stopping || (onlyUnclaimed && w.claimed) {
		w.mu.Unlock()
		return
	}
	w.stopping = true
	s := w.session
	if !w.claimed {
		close(w.done)
	}
	w.mu.Unlock()
	if s != nil {
		s.shutdown()
	}
}

func (w *workerHost) handleConn(fc *frameConn) {
	defer fc.close()
	_ = fc.c.SetReadDeadline(time.Now().Add(frameBudget))
	t, payload, err := fc.readFrame()
	if err != nil || t != ftHandshake {
		return
	}
	_ = fc.c.SetReadDeadline(time.Time{})
	var req handshakeReq
	if jsonUnmarshal(payload, &req) != nil {
		return
	}
	ack := handshakeAck{Op: req.Op, UUID: w.id, PID: os.Getpid()}
	reply := func() error { return fc.writeFrame(ftHandshakeAck, mustJSON(ack)) }
	if req.UUID != w.id {
		ack.Reason = fmt.Sprintf("uuid 不符: 请求 %q，worker %q", req.UUID, w.id)
		_ = reply()
		return
	}
	if req.Op == scrollbackOp && !validScrollback(req.Scrollback) {
		ack.Reason = "回滚行数不合法"
		_ = reply()
		return
	}
	w.mu.Lock()
	s, stopping := w.session, w.stopping
	switch req.Op {
	case "probe":
		ready := s != nil && !stopping && !s.ending.Load()
		ack.OK, ack.Alive, ack.Ready = true, true, &ready
		if ready {
			ack.Busy = s.attached()
			ack.CreatedAt = s.createdAt.UnixMilli()
			if w.config != nil {
				ack.Name, ack.Cwd = w.config.Name, w.config.Cwd
				ack.Cmd = w.cmd
			}
		}
		w.mu.Unlock()
		_ = reply()
	case "kill":
		// Linearize the stop with start before acknowledging. Keep the
		// waiting worker alive until the bounded acknowledgement completes.
		alreadyStopping := w.stopping
		w.stopping = true
		unclaimed := !w.claimed
		w.mu.Unlock()
		ack.OK = true
		_ = reply()
		if !alreadyStopping {
			if unclaimed {
				close(w.done)
			} else if s != nil {
				s.shutdown()
			}
		}
	case scrollbackOp:
		w.mu.Unlock()
		switch {
		case stopping:
			ack.Reason = "会话正在结束"
		case s == nil:
			ack.Reason = "会话尚未就绪"
		default:
			if err := s.syncScrollback(w.paths.settingsPath, req.Scrollback); err != nil {
				ack.Reason = err.Error()
			} else {
				ack.OK = true
			}
		}
		_ = reply()
	case "start":
		if w.config == nil || w.claimed || stopping {
			w.mu.Unlock()
			ack.Reason = "会话不能再次启动"
			_ = reply()
			return
		}
		w.claimed = true
		w.mu.Unlock()
		v := newViewer(fc, 0)
		v.dims = w.config.Dims
		s, err := startSession(w.paths, w.id, w.config, v, func(err error) {
			ack.Reason = err.Error()
			_ = reply()
		})
		if err != nil {
			close(w.done)
			return
		}
		w.mu.Lock()
		w.session, stopping = s, w.stopping
		w.mu.Unlock()
		go w.waitSession(s)
		if stopping {
			go s.shutdown()
		} else if err := s.syncScrollback(w.paths.settingsPath, nil); err != nil {
			log.Printf("worker %s 在 CLI 接入前重检回滚设置失败: %v", w.id, err)
		}
		ack.OK = true
		s.serveViewer(v, ack, false)
	case "attach":
		w.mu.Unlock()
		v := newViewer(fc, 1<<62)
		if s == nil || stopping || !s.addViewer(v) {
			ack.Reason = "会话尚未就绪或已经结束"
			_ = reply()
			return
		}
		ack.OK = true
		s.serveViewer(v, ack, true)
	default:
		w.mu.Unlock()
		ack.Reason = "未知 worker 操作"
		_ = reply()
	}
}

func newViewer(fc *frameConn, cut int64) *viewer {
	v := &viewer{conn: fc, queue: make(chan viewerEvent, queueChunks), done: make(chan struct{})}
	v.cutOffset.Store(cut)
	return v
}

func (s *session) addViewer(v *viewer) bool {
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	if s.ending.Load() {
		return false
	}
	s.viewers[v] = struct{}{}
	s.viewerWG.Add(1)
	return true
}

// serveViewer is the sole writer from handshake to the final frame. A closed
// queue signals graceful end after buffered output, while done drops a viewer.
func (s *session) serveViewer(v *viewer, ack handshakeAck, replay bool) {
	fc := v.conn
	defer s.viewerWG.Done()
	defer s.removeViewer(v)
	defer fc.close()
	defer v.closeWith(0, "")
	defer func() {
		if v.closed.Load() {
			<-v.done
			if v.dropCode != 0 {
				_ = fc.writeFrameTimeout(ftDetach, mustJSON(detachMsg{Code: v.dropCode, Reason: v.dropReason}), 300*time.Millisecond)
			}
		}
	}()
	if fc.writeFrame(ftHandshakeAck, mustJSON(ack)) != nil {
		return
	}

	var out, cont []byte
	// The first CLI viewer receives output from byte zero, at its startup dimensions.
	cols, rows := v.dims.Cols, v.dims.Rows
	if replay {
		var err error
		out, cont, cols, rows, err = s.rebuildFor(v)
		if err != nil {
			return
		}
	}
	if fc.writeFrame(ftReady, mustJSON(resizeMsg{Cols: cols, Rows: rows})) != nil {
		return
	}
	for i := 0; i < len(out); i += wsChunk {
		if v.closed.Load() || fc.writeFrame(ftOutput, out[i:min(i+wsChunk, len(out))]) != nil {
			return
		}
	}
	if len(cont) != 0 && fc.writeFrame(ftOutput, cont) != nil {
		return
	}
	go s.readViewer(v)

	buf := make([]byte, 0, wsChunk)
	var lastFlush time.Time
	// flushOutput 把已累积的字节作为一帧写出。失败即丢弃该 viewer，与原路径一致。
	flushOutput := func() bool {
		if v.closed.Load() {
			return false
		}
		if fc.writeFrame(ftOutput, buf) != nil {
			v.closeWith(4001, "too slow")
			return false
		}
		buf = buf[:0]
		lastFlush = time.Now()
		return true
	}
	writeSize := func(size resizeMsg) bool {
		if v.closed.Load() {
			return false
		}
		if fc.writeFrame(ftSize, mustJSON(size)) != nil {
			v.closeWith(4001, "too slow")
			return false
		}
		return true
	}
	// 合批等待定时器：只在等待期间 armed，复用同一实例，不泄漏。
	// （Go 1.23+ 语义：Stop 返回后 t.C 不会再送达残值，Stop+Reset 无需排空。）
	batch := time.NewTimer(time.Hour)
	batch.Stop()
	defer batch.Stop()
	for !v.closed.Load() {
		select {
		case <-v.done:
			return
		case event, ok := <-v.queue:
			if !ok {
				if !v.closed.Load() {
					_ = fc.writeFrameTimeout(ftExit, mustJSON(s.end), exitWriteBudget)
				}
				return
			}
			if len(event.data) == 0 {
				if !writeSize(event.size) {
					return
				}
				continue
			}
			buf = append(buf[:0], event.data...)
			v.queued.Add(-int64(len(event.data)))
			var nextSize resizeMsg
		drain:
			for len(buf) < wsChunk {
				select {
				case event, ok := <-v.queue:
					if !ok {
						break drain
					}
					if len(event.data) == 0 {
						nextSize = event.size
						break drain
					}
					buf = append(buf, event.data...)
					v.queued.Add(-int64(len(event.data)))
				default:
					break drain
				}
			}
			if v.closed.Load() {
				return
			}
			// 爆发期微批：距上次刷出不足一个窗口且缓冲未满时，在剩余窗口内继续
			// 收集后续 ftOutput 合并成一帧。待写的尺寸事件会切开批次，先行直发。
			if since := time.Since(lastFlush); nextSize.Cols == 0 && len(buf) < wsChunk && since < viewerBatchWindow {
				batch.Stop()
				batch.Reset(viewerBatchWindow - since)
			batchWait:
				for len(buf) < wsChunk {
					select {
					case <-batch.C:
						break batchWait
					case <-v.done:
						return
					case event, ok := <-v.queue:
						if !ok {
							// 退出不拖延：立即刷出已累积的字节，再走原有退出路径。
							if !flushOutput() {
								return
							}
							if !v.closed.Load() {
								_ = fc.writeFrameTimeout(ftExit, mustJSON(s.end), exitWriteBudget)
							}
							return
						}
						if len(event.data) == 0 {
							// 尺寸事件不能越过前后的输出字节：先刷出已累积字节再写尺寸。
							if !flushOutput() {
								return
							}
							if !writeSize(event.size) {
								return
							}
							break batchWait
						}
						buf = append(buf, event.data...)
						v.queued.Add(-int64(len(event.data)))
					}
				}
				// 因缓冲写满或尺寸事件提前跳出时定时器仍 armed：停掉避免多触发一次。
				batch.Stop()
				if len(buf) != 0 && !flushOutput() {
					return
				}
				continue
			}
			if !flushOutput() {
				return
			}
			if nextSize.Cols != 0 && !writeSize(nextSize) {
				return
			}
		}
	}
}

func (s *session) readViewer(v *viewer) {
	defer func() {
		v.closeWith(0, "")
		_ = v.conn.close()
		s.inputMu.Lock()
		if s.owner == v {
			s.owner = nil
		}
		s.inputMu.Unlock()
	}()
	for !v.closed.Load() {
		t, payload, err := v.conn.readFrame()
		if err != nil {
			return
		}
		switch t {
		case ftResize:
			_, dims, ok := decodeResize(payload)
			if !ok {
				continue
			}
			s.inputMu.Lock()
			v.dims = dims
			v.locked = dims == (viewerDims{})
			if v.locked && s.owner == v {
				s.owner = nil
			} else if s.owner == v && !v.closed.Load() {
				s.resize(dims.Cols, dims.Rows)
			}
			s.inputMu.Unlock()
		case ftInput:
			if len(payload) == 0 {
				continue
			}
			s.inputMu.Lock()
			if v.closed.Load() || s.ending.Load() {
				s.inputMu.Unlock()
				return
			}
			if !v.locked && !isTerminalReportOnly(payload) {
				if s.cli {
					s.owner = v
				}
				if v.dims.valid() {
					s.resize(v.dims.Cols, v.dims.Rows)
				}
			}
			_, err := s.ptmx.Write(payload)
			s.inputMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// isTerminalReportOnly reports whether b consists exclusively of automatic
// terminal query responses/focus notifications rather than user keystrokes.
func isTerminalReportOnly(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for i := 0; i < len(b); {
		n := consumeTerminalReport(b[i:])
		if n == 0 {
			return false
		}
		i += n
	}
	return true
}

func consumeTerminalReport(b []byte) int {
	if len(b) < 3 || b[0] != 0x1b {
		return 0
	}
	switch b[1] {
	case '[':
		return consumeCSIReport(b)
	case ']':
		return consumeOSCReport(b)
	case 'P':
		return consumeSTTerminated(b, 2)
	case '_':
		if len(b) >= 5 && b[2] == 'G' {
			return consumeSTTerminated(b, 3)
		}
	}
	return 0
}

func consumeCSIReport(b []byte) int {
	if b[2] == 'I' || b[2] == 'O' {
		return 3
	}
	i := 2
	var prefix byte
	if b[i] == '?' || b[i] == '>' {
		prefix = b[i]
		i++
	}
	if i >= len(b) || b[i] < '0' || b[i] > '9' {
		return 0
	}
	groups := 0
	firstStart, firstEnd := i, i
	for {
		dStart := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		if i == dStart {
			return 0
		}
		if groups == 0 {
			firstStart, firstEnd = dStart, i
		}
		groups++
		if i < len(b) && b[i] == ';' {
			i++
			continue
		}
		break
	}
	if i >= len(b) {
		return 0
	}
	if b[i] == '$' {
		if i+1 < len(b) && b[i+1] == 'y' && (prefix == 0 || prefix == '?') && groups == 2 {
			return i + 2
		}
		return 0
	}
	switch b[i] {
	case 'u':
		if prefix == '?' && groups == 1 {
			return i + 1
		}
	case 'c':
		if prefix == '?' || prefix == '>' {
			return i + 1
		}
	case 'R':
		if (prefix == 0 || prefix == '?') && groups == 2 {
			return i + 1
		}
	case 'n':
		if (prefix == 0 || prefix == '?') && groups >= 1 {
			return i + 1
		}
	case 't':
		if prefix == 0 && groups == 3 && firstEnd-firstStart == 1 &&
			(b[firstStart] == '4' || b[firstStart] == '6' || b[firstStart] == '8') {
			return i + 1
		}
	}
	return 0
}

func consumeOSCReport(b []byte) int {
	rest := b[2:]
	offset := 0
	for _, p := range [][]byte{[]byte("4;"), []byte("10;"), []byte("11;"), []byte("12;")} {
		if bytes.HasPrefix(rest, p) {
			offset = 2 + len(p)
			break
		}
	}
	if offset == 0 {
		return 0
	}
	for j := offset; j < len(b); j++ {
		if b[j] == 0x07 {
			return j + 1
		}
		if b[j] == 0x1b {
			if j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
			return 0
		}
		if b[j] < 0x20 || b[j] == 0x7f {
			return 0
		}
	}
	return 0
}

func consumeSTTerminated(b []byte, start int) int {
	for j := start; j < len(b); j++ {
		if b[j] == 0x1b {
			if j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
			return 0
		}
		if b[j] < 0x20 || b[j] == 0x7f {
			return 0
		}
	}
	return 0
}
