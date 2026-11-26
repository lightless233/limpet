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
)

// uuidRe 稳定 ID：8 字节随机 hex。
var uuidRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// errDuplicate 表示与活会话重名（HTTP 409）。
var (
	errDuplicate = errors.New("同名会话已存在")
	errNotFound  = errors.New("会话不存在")
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
}

type manager struct {
	mu         sync.Mutex
	sessions   map[string]*workerHandle
	creating   map[string]struct{} // worker 就绪前预占的名称
	histDir    string
	workersDir string // socket 与 worker 日志的所在目录（0700）
	regPath    string
	registry   map[string]regEntry
	exePath    string
	launch     launchMode // worker 启动方式（spec 005，见 launch.go）
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
		sessions:   map[string]*workerHandle{},
		creating:   map[string]struct{}{},
		histDir:    filepath.Join(base, "history"),
		workersDir: filepath.Join(base, "workers"),
		regPath:    filepath.Join(base, "sessions.json"),
		registry:   map[string]regEntry{},
		exePath:    exe,
		launch:     launch,
	}
	for _, d := range []string{m.histDir, m.workersDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := m.loadRegistry(); err != nil {
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
func (m *manager) saveRegistry() {
	b, err := json.MarshalIndent(m.registry, "", "  ")
	defer func() {
		if err != nil {
			log.Printf("保存会话注册表 %s 失败: %v", m.regPath, err)
		}
	}()
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.regPath), ".sessions-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), m.regPath)
	}
}

func (m *manager) registryEndLocked(id string) {
	if e, ok := m.registry[id]; ok && e.EndedAt == nil {
		now := time.Now()
		e.EndedAt = &now
		m.registry[id] = e
		m.saveRegistry()
	}
}

// recoverWorkers 启动时探测现存 socket。瞬时探测失败时保留已有会话以便下次列表重试。
func (m *manager) recoverWorkers() {
	entries, err := os.ReadDir(m.workersDir)
	if err != nil {
		log.Printf("扫描 worker 目录失败: %v", err)
		return
	}
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		uuid, ok := strings.CutSuffix(name, ".sock")
		if !ok || !uuidRe.MatchString(uuid) {
			continue
		}
		path := filepath.Join(m.workersDir, name)
		ack, err := probeWorker(path, uuid)
		if staleSocket(err) {
			_ = os.Remove(path)
			continue
		}
		present[uuid] = true
		reg := m.registry[uuid]
		switch {
		case err != nil:
			log.Printf("[recover] 暂时无法探测 worker %s，保留 socket: %v", uuid, err)
			if reg.CreatedAt.IsZero() || reg.EndedAt != nil {
				continue
			}
		case ack.UUID != uuid || !ack.OK || ack.Op != "probe":
			log.Printf("[recover] worker %s 握手不符（op=%q, uuid=%q, ok=%t），保留 socket", uuid, ack.Op, ack.UUID, ack.OK)
			continue
		}

		created := reg.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		nm := reg.Name
		if nm == "" {
			nm = uuid
		}
		h := &workerHandle{
			uuid: uuid, name: nm, createdAt: created,
			socketPath: path, logPath: filepath.Join(m.workersDir, uuid+".log"),
			attached: ack.Busy,
		}
		m.sessions[uuid] = h
		if err == nil {
			log.Printf("[recover] 会话恢复: %s（%s，pid %d）", uuid, nm, ack.PID)
		}
	}
	// 注册表里已无 socket 的会话，记下首次观察到的结束时间。
	for uuid, e := range m.registry {
		if e.EndedAt == nil && !present[uuid] {
			now := time.Now()
			e.EndedAt = &now
			m.registry[uuid] = e
		}
	}
	m.saveRegistry()
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
	defer m.mu.Unlock()
	return m.sessions[id]
}

// create 启动 worker 进程并等它就绪（socket 可探测、自报 uuid 一致）。
// Shell 启动失败则 worker 退出、创建请求失败，不留半死会话（spec 003）。
func (m *manager) create(name string) (*workerHandle, error) {
	id := newID()
	if name == "" {
		name = id
	} else if !nameValid(name) {
		return nil, fmt.Errorf("名称不合法：不能包含 : 或 .，长度不超过 100 字符")
	}

	m.mu.Lock()
	if m.nameTakenLocked(name, "") {
		m.mu.Unlock()
		return nil, errDuplicate
	}
	m.creating[name] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.creating, name)
		m.mu.Unlock()
	}()

	sockPath := filepath.Join(m.workersDir, id+".sock")
	logPath := filepath.Join(m.workersDir, id+".log")
	killHard, err := m.spawnWorker(id, logPath)
	if err != nil {
		return nil, fmt.Errorf("启动 worker 失败: %w", err)
	}

	// 等就绪：socket 可探测且自报 uuid 一致（间隔 50ms，上限 spawnWait）。
	deadline := time.Now().Add(spawnWait)
	for {
		if time.Now().After(deadline) {
			killHard()
			_ = os.Remove(sockPath)
			return nil, fmt.Errorf("worker %s 未在 %v 内就绪", id, spawnWait)
		}
		time.Sleep(50 * time.Millisecond)
		ack, err := probeWorker(sockPath, id)
		if err == nil && ack.OK && ack.UUID == id && ack.Op == "probe" {
			break
		}
	}

	h := &workerHandle{
		uuid: id, name: name, createdAt: time.Now(),
		socketPath: sockPath, logPath: logPath,
	}
	m.mu.Lock()
	m.sessions[id] = h
	m.registry[id] = regEntry{Name: name, CreatedAt: h.createdAt}
	m.saveRegistry()
	m.mu.Unlock()
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

	ack, err := killWorker(h.socketPath, id)
	if err != nil {
		if staleSocket(err) {
			m.dropStale(id, h)
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
		_, err := os.Stat(h.socketPath)
		if errors.Is(err, os.ErrNotExist) {
			m.dropStale(id, h)
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

// list 活会话查询：快照按创建时间排序，对每个 worker 探测接入状态。
func (m *manager) list() []map[string]any {
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

	out := make([]map[string]any, 0, len(workers))
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
		if err == nil && ack.OK && ack.UUID == state.uuid && ack.Op == "probe" {
			h.attached = ack.Busy
		}
		name, attached := h.name, h.attached
		m.mu.Unlock()
		out = append(out, map[string]any{
			"id":       state.uuid,
			"name":     name,
			"created":  state.createdAt.Format(time.RFC3339),
			"attached": attached,
		})
	}
	return out
}

func (m *manager) renameSession(id, name string) error {
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
