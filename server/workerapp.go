package main

import (
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
	firstViewerWait = 10 * time.Second
)

type workerConfig struct {
	Argv            []string
	Cwd             string
	Dims            viewerDims
	Name            string
	WaitFirstViewer bool
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
	config, err := readWorkerConfig()
	if err != nil {
		return err
	}
	if err := setSubreaper(); err != nil {
		log.Printf("setSubreaper 失败（已孤儿化的任务可能无法回收）: %v", err)
	}
	for _, path := range []string{paths.socketDir, paths.histDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}

	w := &workerHost{id: uuid, paths: paths, config: config, done: make(chan struct{})}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	if config == nil {
		// Web create still starts the shell before publishing the socket.
		s, err := startSession(paths, uuid, nil, nil, nil)
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
	if config != nil {
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
				ack.Cmd = filepath.Base(w.config.Argv[0])
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
			if fc.writeFrame(ftOutput, buf) != nil {
				v.closeWith(4001, "too slow")
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
			if !v.locked {
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
