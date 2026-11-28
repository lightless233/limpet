package main

// server 侧的会话管理：worker 进程的启停、恢复扫描、注册表与列表。
// server 自身不持有任何 PTY/网格——worker 进程才是 PTY 持有者（spec 003），
// server 死亡时 worker 孤儿化继续运行，新 server 靠扫描 + 探测握手找回。
// 多视图（spec 004）：一个 worker 可带 N 个 viewer。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// uuidRe 稳定 ID：8 字节随机 hex。
var uuidRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// errDuplicate 表示与活会话重名（HTTP 409）。
var (
	errDuplicate = errors.New("同名会话已存在")
	errNotFound  = errors.New("会话不存在")
	errNotReady  = errors.New("worker 尚未就绪")
)

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// workerHandle 是 server 眼中的一条活会话：一个运行中的 worker 进程。
type workerHandle struct {
	uuid       string
	name       string
	createdAt  time.Time
	socketPath string
	logPath    string
	attached   bool
}

type regEntry struct {
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Cwd       string     `json:"cwd,omitempty"`
	Cmd       string     `json:"cmd,omitempty"`
}

type manager struct {
	mu         sync.Mutex
	sessions   map[string]*workerHandle
	creating   map[string]struct{} // worker 就绪前预占的名称
	pendingIDs map[string]struct{} // Web create owns admission until it completes
	histDir    string
	workersDir string // socket 与 worker 日志的所在目录（0700）
	regPath    string
	registry   map[string]regEntry
	exePath    string
	launch     launchMode // worker 启动方式（spec 005，见 launch.go）

	settingsPath string
	settingsMu   sync.Mutex // serializes persistence and broadcast
	scrollback   int        // guarded by mu
}

func newManager(launch launchMode) (*manager, error) {
	base, err := limpetHome()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &manager{
		sessions:     map[string]*workerHandle{},
		creating:     map[string]struct{}{},
		pendingIDs:   map[string]struct{}{},
		histDir:      filepath.Join(base, "history"),
		workersDir:   filepath.Join(base, "workers"),
		regPath:      filepath.Join(base, "sessions.json"),
		registry:     map[string]regEntry{},
		exePath:      exe,
		launch:       launch,
		settingsPath: filepath.Join(base, "settings.json"),
	}
	for _, d := range []string{m.histDir, m.workersDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := m.loadRegistry(); err != nil {
		return nil, err
	}
	if err := m.loadSettings(); err != nil {
		return nil, err
	}
	m.recoverWorkers()
	return m, nil
}

func (m *manager) loadRegistry() error {
	b, err := os.ReadFile(m.regPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取会话注册表失败: %w", err)
	}

	var registry map[string]regEntry
	if err := json.Unmarshal(b, &registry); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", m.regPath, time.Now().UnixNano())
		log.Printf("解析会话注册表 %s 失败: %v", m.regPath, err)
		if err := os.Rename(m.regPath, backup); err != nil {
			return fmt.Errorf("备份损坏的会话注册表失败: %w", err)
		}
		log.Printf("已备份损坏的会话注册表至 %s", backup)
	} else if registry != nil {
		m.registry = registry
	}
	return nil
}

// saveRegistry 原子写（tmp + rename），调用方持 m.mu。注册表单写者是 server。
func (m *manager) saveRegistry() (err error) {
	defer func() {
		if err != nil {
			log.Printf("保存会话注册表 %s 失败: %v", m.regPath, err)
		}
	}()
	b, err := json.MarshalIndent(m.registry, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.regPath, b)
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (m *manager) registryEndLocked(id string) {
	if e, ok := m.registry[id]; ok && e.EndedAt == nil {
		now := time.Now()
		e.EndedAt = &now
		m.registry[id] = e
		m.saveRegistry()
	}
}

// recoverWorkers is startup-only. A missing socket marks a previously
// registered session ended; transient errors preserve its registration.
func (m *manager) recoverWorkers() {
	present, err := m.scanWorkers(true)
	if err != nil {
		return
	}
	for uuid, e := range m.registry {
		if e.EndedAt == nil && !present[uuid] {
			m.registryEndLocked(uuid)
		}
	}
}

// scanWorkers discovers new IDs. Already managed workers are probed by list.
func (m *manager) scanWorkers(recovering bool) (map[string]bool, error) {
	entries, err := os.ReadDir(m.workersDir)
	if err != nil {
		log.Printf("扫描 worker 目录失败: %v", err)
		return nil, err
	}
	present := make(map[string]bool)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".sock")
		if !ok || !uuidRe.MatchString(id) || e.Type()&os.ModeSocket == 0 {
			continue
		}
		present[id] = true
		m.mu.Lock()
		known := m.sessions[id] != nil
		m.mu.Unlock()
		if known {
			continue
		}
		_, err := m.discover(id, "", recovering)
		if staleSocket(err) {
			delete(present, id)
		}
	}
	return present, nil
}

func (m *manager) discover(id, name string, recovering bool) (*workerHandle, error) {
	path := filepath.Join(m.workersDir, id+".sock")
	m.mu.Lock()
	_, pending := m.pendingIDs[id]
	ended := m.registry[id].EndedAt != nil
	m.mu.Unlock()
	if pending || ended {
		return nil, errNotFound
	}

	ack, err := probeWorker(path, id)
	if staleSocket(err) {
		m.mu.Lock()
		if _, pending := m.pendingIDs[id]; !pending && m.sessions[id] == nil {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(path)
			}
		}
		m.mu.Unlock()
		return nil, err
	}
	if err != nil && !recovering {
		return nil, err
	}
	if err == nil && !ack.readyFor(id) {
		return nil, errNotReady
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	reg, registered := m.registry[id]
	if _, pending := m.pendingIDs[id]; pending || reg.EndedAt != nil {
		return nil, errNotFound
	}
	if h := m.sessions[id]; h != nil {
		return h, nil
	}
	if err != nil && (!registered || reg.CreatedAt.IsZero()) {
		return nil, err
	}
	if reg.CreatedAt.IsZero() {
		reg.CreatedAt = time.Now()
		if ack.CreatedAt != 0 {
			reg.CreatedAt = time.UnixMilli(ack.CreatedAt)
		}
	}
	if reg.Name == "" {
		if !nameValid(name) {
			name = ack.Name
		}
		if !nameValid(name) {
			name = id
		}
		reg.Name = name
		base := []rune(name)
		for n := 2; m.nameTakenLocked(reg.Name, id); n++ {
			suffix := fmt.Sprintf("-%d", n)
			reg.Name = string(base[:min(len(base), 100-len(suffix))]) + suffix
		}
	}
	if err == nil {
		if reg.Cwd == "" && ack.Cwd != "" {
			reg.Cwd = ack.Cwd
		}
		if reg.Cmd == "" && ack.Cmd != "" {
			reg.Cmd = ack.Cmd
		}
	}
	if !registered || m.registry[id] != reg {
		previous := m.registry[id]
		m.registry[id] = reg
		if err := m.saveRegistry(); err != nil {
			if registered {
				m.registry[id] = previous
			} else {
				delete(m.registry, id)
			}
			return nil, fmt.Errorf("登记会话失败: %w", err)
		}
	}
	h := &workerHandle{
		uuid: id, name: reg.Name, createdAt: reg.CreatedAt,
		socketPath: path, logPath: filepath.Join(m.workersDir, id+".log"),
		attached: ack.Busy,
	}
	m.sessions[id] = h
	return h, nil
}
func (m *manager) nameTakenLocked(name, exceptID string) bool {
	if _, exists := m.creating[name]; exists {
		return true
	}
	for _, h := range m.sessions {
		if h.uuid != exceptID && h.name == name {
			return true
		}
	}
	return false
}

func (m *manager) get(id string) *workerHandle {
	m.mu.Lock()
	h := m.sessions[id]
	m.mu.Unlock()
	if h == nil && uuidRe.MatchString(id) {
		h, _ = m.discover(id, "", false)
	}
	return h
}

// create 启动 worker 进程并等它就绪（socket 可探测、自报 uuid 一致）。
// Shell 启动失败则 worker 退出、创建请求失败，不留半死会话（spec 003）。
func (m *manager) create(name string, config *webLaunchConfig, autoName bool) (*workerHandle, error) {
	id := newID()
	home, _ := os.UserHomeDir()
	if name == "" {
		name = id
		if autoName {
			base := config.Cmd
			if config.Cwd != filepath.Clean(home) {
				base = filepath.Base(config.Cwd) + "-" + base
			}
			runes := []rune(strings.Map(func(r rune) rune {
				if r == ':' || r == '.' || r == '/' || unicode.IsControl(r) {
					return '-'
				}
				return r
			}, base))
			if candidate := string(runes[:min(len(runes), 96)]); nameValid(candidate) {
				name = candidate
			}
		}
	} else if !nameValid(name) {
		return nil, fmt.Errorf("名称不合法：不能包含 : 或 .，长度不超过 100 字符")
	} else {
		autoName = false
	}

	m.mu.Lock()
	if autoName {
		base := []rune(name)
		for n := 2; m.nameTakenLocked(name, ""); n++ {
			suffix := fmt.Sprintf("-%d", n)
			name = string(base[:min(len(base), 100-len(suffix))]) + suffix
		}
	} else if m.nameTakenLocked(name, "") {
		m.mu.Unlock()
		return nil, errDuplicate
	}
	m.creating[name] = struct{}{}
	m.pendingIDs[id] = struct{}{}
	m.mu.Unlock()
	releasePending := true
	defer func() {
		m.mu.Lock()
		delete(m.creating, name)
		if releasePending {
			delete(m.pendingIDs, id)
		}
		m.mu.Unlock()
	}()

	sockPath := filepath.Join(m.workersDir, id+".sock")
	logPath := filepath.Join(m.workersDir, id+".log")
	launchPath := filepath.Join(m.workersDir, id+".launch.json")
	config.Name = name
	if err := atomicWrite(launchPath, mustJSON(config)); err != nil {
		return nil, fmt.Errorf("写入 Web 启动配置失败: %w", err)
	}
	defer func() {
		if err := os.Remove(launchPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("清理 Web 启动配置 %s 失败: %v", id, err)
		}
	}()
	killHard, err := m.spawnWorker(id, logPath)
	if err != nil {
		if m.launch == launchSystemdRun {
			stopWorkerUnit(id)
		}
		return nil, fmt.Errorf("启动 worker 失败: %w", err)
	}

	abortWorker := func() error {
		killHard()
		if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			releasePending = false
			return fmt.Errorf("清理 worker socket 失败: %w", err)
		}
		return nil
	}

	// 等就绪：socket 可探测且自报 uuid 一致（间隔 50ms，上限 spawnWait）。
	deadline := time.Now().Add(spawnWait)
	var createdAt time.Time
	for {
		if time.Now().After(deadline) {
			return nil, errors.Join(fmt.Errorf("worker %s 启动失败或在 %v 内未就绪", id, spawnWait), abortWorker())
		}
		time.Sleep(50 * time.Millisecond)
		ack, err := probeWorker(sockPath, id)
		if err == nil && ack.readyFor(id) {
			createdAt = time.UnixMilli(ack.CreatedAt)
			break
		}
		if err == nil && ack.Op == "probe" && ack.OK && ack.UUID == id && ack.Ready != nil && !*ack.Ready {
			return nil, errors.Join(fmt.Errorf("worker %s 在就绪前结束", id), abortWorker())
		}
	}

	h := &workerHandle{
		uuid: id, name: name, createdAt: createdAt,
		socketPath: sockPath, logPath: logPath,
	}
	m.mu.Lock()
	m.registry[id] = regEntry{Name: name, CreatedAt: h.createdAt, Cwd: config.Cwd, Cmd: config.Cmd}
	if err := m.saveRegistry(); err != nil {
		delete(m.registry, id)
		m.mu.Unlock()
		if stopErr := stopWorkerAndWait(sockPath, id); stopErr != nil {
			err = errors.Join(err, stopErr, abortWorker())
		}
		return nil, fmt.Errorf("登记会话失败: %w", err)
	}
	m.sessions[id] = h
	delete(m.pendingIDs, id)
	m.mu.Unlock()
	_, _ = m.syncWorkerScrollback(h, m.currentScrollback())
	return h, nil
}

// spawnWorker 按配置的启动方式拉起 worker（见 launch.go）。
func (m *manager) spawnWorker(id, logPath string) (killHard func(), err error) {
	if m.launch == launchSystemdRun {
		return spawnWorkerSystemdRun(m.exePath, id, logPath)
	}
	return spawnWorkerDirect(m.exePath, id, logPath, nil)
}

// closeSession 等 worker 回收进程、完成通知并自行移除 socket。超时后保留会话，允许重试。
func (m *manager) closeSession(id string) error {
	h := m.get(id)
	if h == nil {
		return errNotFound
	}

	if err := stopWorkerAndWait(h.socketPath, id); err != nil {
		return err
	}
	m.dropStale(id, h)
	return nil
}

func stopWorkerAndWait(socketPath, id string) error {
	ack, err := killWorker(socketPath, id)
	if err != nil {
		if staleSocket(err) {
			return nil
		}
		return fmt.Errorf("请求 worker 关闭失败: %w", err)
	}
	if !ack.OK || ack.UUID != id || ack.Op != "kill" {
		return fmt.Errorf("worker 关闭握手不符（op=%q, uuid=%q, ok=%t）", ack.Op, ack.UUID, ack.OK)
	}

	budget := killGrace + reapAfterKill + frameBudget + exitWriteBudget + time.Second
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		_, err := os.Stat(socketPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("检查 worker socket 失败: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("worker 未在 %v 内完成关闭", budget)
}

// 只有拨号明确失败才确认 socket 已失效。握手超时或异常保留会话供下一次探测。
func staleSocket(err error) bool {
	var dialErr *net.OpError
	return errors.As(err, &dialErr) && dialErr.Op == "dial" &&
		(errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED))
}

// dropStale 清理已确认失效的 socket 并登记死亡。
func (m *manager) dropStale(id string, h *workerHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] != h {
		return
	}
	if err := os.Remove(h.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("清理失效 worker socket %s 失败: %v", h.socketPath, err)
	}
	delete(m.sessions, id)
	m.registryEndLocked(id)
}

type listedSession struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	Attached bool   `json:"attached"`
	Cwd      string `json:"cwd,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
}

// list publishes only the HTTP display fields. CLI and registry retain the launch directory.
func (m *manager) list() []map[string]any {
	items := m.listSessions()
	out := make([]map[string]any, 0, len(items))
	home, _ := os.UserHomeDir()
	for _, item := range items {
		row := map[string]any{
			"id": item.ID, "name": item.Name, "created": item.Created, "attached": item.Attached,
		}
		if item.Cwd != "" {
			row["cwd"] = displayCwd(item.Cwd, home)
		}
		if item.Cmd != "" {
			row["cmd"] = filepath.Base(item.Cmd)
		}
		out = append(out, row)
	}
	return out
}

func displayCwd(cwd, home string) string {
	if home != "" {
		home = filepath.Clean(home)
		if cwd == home {
			return "~"
		}
		if suffix, ok := strings.CutPrefix(cwd, strings.TrimRight(home, "/")+"/"); ok {
			return "~/" + suffix
		}
	}
	return cwd
}

func (m *manager) workspaces() []string {
	home, _ := os.UserHomeDir()
	recent := make(map[string]time.Time)
	m.mu.Lock()
	for _, entry := range m.registry {
		if !filepath.IsAbs(entry.Cwd) {
			continue
		}
		path := filepath.Clean(entry.Cwd)
		if path == filepath.Clean(home) {
			continue
		}
		if prior, exists := recent[path]; !exists || entry.CreatedAt.After(prior) {
			recent[path] = entry.CreatedAt
		}
	}
	m.mu.Unlock()

	type workspace struct {
		path string
		used time.Time
	}
	items := make([]workspace, 0, len(recent))
	for path, used := range recent {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			items = append(items, workspace{displayCwd(path, home), used})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].used.Equal(items[j].used) {
			return items[i].path < items[j].path
		}
		return items[i].used.After(items[j].used)
	})
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.path)
	}
	return out
}

func (m *manager) listSessions() []listedSession {
	_, _ = m.scanWorkers(false)
	type workerSnapshot struct {
		handle *workerHandle
		state  workerHandle
	}
	m.mu.Lock()
	workers := make([]workerSnapshot, 0, len(m.sessions))
	for _, h := range m.sessions {
		workers = append(workers, workerSnapshot{handle: h, state: *h})
	}
	m.mu.Unlock()
	sort.Slice(workers, func(i, j int) bool {
		a, b := workers[i].state, workers[j].state
		if a.createdAt.Equal(b.createdAt) {
			return a.uuid < b.uuid
		}
		return a.createdAt.Before(b.createdAt)
	})

	out := make([]listedSession, 0, len(workers))
	for _, worker := range workers {
		h, state := worker.handle, worker.state
		ack, err := probeWorker(state.socketPath, state.uuid)
		if staleSocket(err) {
			m.dropStale(state.uuid, h)
			continue
		}
		m.mu.Lock()
		if m.sessions[state.uuid] != h {
			m.mu.Unlock()
			continue
		}
		if err == nil && ack.readyFor(state.uuid) {
			h.attached = ack.Busy
		}
		reg := m.registry[state.uuid]
		out = append(out, listedSession{
			ID: state.uuid, Name: h.name, Attached: h.attached,
			Created: state.createdAt.Format(time.RFC3339), Cwd: reg.Cwd, Cmd: reg.Cmd,
		})
		m.mu.Unlock()
	}
	return out
}

func (m *manager) renameSession(id, name string) error {
	if m.get(id) == nil {
		return errNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.sessions[id]
	if h == nil {
		return errNotFound
	}
	if h.name == name {
		return nil
	}
	if m.nameTakenLocked(name, id) {
		return errDuplicate
	}
	h.name = name
	e, ok := m.registry[id]
	if !ok {
		e.CreatedAt = h.createdAt
	}
	e.Name = name
	m.registry[id] = e
	m.saveRegistry()
	return nil
}
