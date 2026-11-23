package main

// server 侧的会话管理：worker 进程的启停、恢复扫描、注册表与列表。
// server 自身不持有任何 PTY/网格——worker 进程才是 PTY 持有者（spec 003），
// server 死亡时 worker 孤儿化继续运行，新 server 靠扫描 + 探测握手找回。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// uuidRe 稳定 ID：8 字节随机 hex。
var uuidRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

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
	pid        int
	busy       atomic.Bool // 接入中（server 侧跟踪；权威判定在 worker 的握手）
}

type regEntry struct {
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

type manager struct {
	mu         sync.Mutex
	sessions   map[string]*workerHandle
	home       string
	histDir    string
	workersDir string // socket 与 worker 日志的所在目录（0700）
	regPath    string
	registry   map[string]regEntry
	exePath    string
}

func newManager() (*manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &manager{
		sessions:   map[string]*workerHandle{},
		home:       home,
		histDir:    filepath.Join(home, ".limpet", "history"),
		workersDir: filepath.Join(home, ".limpet", "workers"),
		regPath:    filepath.Join(home, ".limpet", "sessions.json"),
		registry:   map[string]regEntry{},
		exePath:    exe,
	}
	for _, d := range []string{m.histDir, m.workersDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	m.loadRegistry()
	m.recoverWorkers()
	return m, nil
}

func (m *manager) loadRegistry() {
	b, err := os.ReadFile(m.regPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &m.registry)
}

// saveRegistry 原子写（tmp + rename），调用方持 m.mu。注册表单写者是 server。
func (m *manager) saveRegistry() {
	b, err := json.MarshalIndent(m.registry, "", "  ")
	if err != nil {
		return
	}
	tmp := m.regPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.regPath)
}

func (m *manager) registryCreate(id, name string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry[id] = regEntry{Name: name, CreatedAt: at}
	m.saveRegistry()
}

func (m *manager) registryEnd(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.registry[id]; ok && e.EndedAt == nil {
		now := time.Now()
		e.EndedAt = &now
		m.registry[id] = e
		m.saveRegistry()
	}
}

func (m *manager) registryRename(id, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.registry[id]; ok {
		e.Name = name
		m.registry[id] = e
		m.saveRegistry()
	}
}

// recoverWorkers 启动恢复：扫描 socket 目录，逐个探测握手。
// - 探测成功且自报 uuid 与文件名一致 → 会话恢复（名字取自注册表）；
// - 探测成功但身份不符 → 不动、不列入（spec 验收 18），记录日志；
// - 拨号失败 → stale：删除 socket，首次观测登记死亡。
func (m *manager) recoverWorkers() {
	entries, err := os.ReadDir(m.workersDir)
	if err != nil {
		return
	}
	recovered := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		uuid := strings.TrimSuffix(name, ".sock")
		path := filepath.Join(m.workersDir, name)
		ack, err := probeWorker(path, uuid)
		if err != nil {
			// stale：进程已死。清理 socket；注册表首次观测登记死亡。
			_ = os.Remove(path)
			m.registryEnd(uuid)
			continue
		}
		if ack.UUID != uuid || !ack.OK {
			// 活着但身份不符：不动、不删除、不列入。
			fmt.Printf("[recover] worker %s 自报 uuid %q 与文件名不符，跳过\n", uuid, ack.UUID)
			continue
		}
		recovered[uuid] = true
		reg := m.registry[uuid]
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
			pid: ack.PID,
		}
		m.mu.Lock()
		m.sessions[uuid] = h
		m.mu.Unlock()
		fmt.Printf("[recover] 会话恢复: %s（%s，pid %d）\n", uuid, nm, ack.PID)
	}
	// 注册表里既无 socket 又未恢复的 → 首次观测死亡（kill-server/WSL 重启场景）。
	m.mu.Lock()
	for uuid, e := range m.registry {
		if e.EndedAt == nil && !recovered[uuid] {
			now := time.Now()
			e.EndedAt = &now
			m.registry[uuid] = e
		}
	}
	m.saveRegistry()
	m.mu.Unlock()
}

func (m *manager) byIDLocked(id string) *workerHandle { return m.sessions[id] }

func (m *manager) get(id string) *workerHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// create 启动 worker 进程并等它就绪（socket 可探测、自报 uuid 一致）。
// Shell 启动失败则 worker 退出、创建请求失败，不留半死会话（spec 003）。
func (m *manager) create(name string) (*workerHandle, error) {
	if name != "" {
		if !nameValid(name) {
			return nil, fmt.Errorf("名称不合法：不能包含 : 或 .，长度不超过 100 字符")
		}
		m.mu.Lock()
		for _, s := range m.sessions {
			if s.name == name {
				m.mu.Unlock()
				return nil, fmt.Errorf("同名会话已存在")
			}
		}
		m.mu.Unlock()
	}
	id := newID()
	if name == "" {
		name = id
	}

	sockPath := filepath.Join(m.workersDir, id+".sock")
	logPath := filepath.Join(m.workersDir, id+".log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer lf.Close()

	cmd := exec.Command(m.exePath, "worker", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // worker 自成会话：server 的 Ctrl+C 不可达
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 worker 失败: %w", err)
	}

	// 等就绪：socket 可探测且自报 uuid 一致（间隔 50ms，上限 spawnWait）。
	var ack handshakeAck
	deadline := time.Now().Add(spawnWait)
	for {
		if time.Now().After(deadline) {
			_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			_ = os.Remove(sockPath)
			return nil, fmt.Errorf("worker %s 未在 %v 内就绪", id, spawnWait)
		}
		time.Sleep(50 * time.Millisecond)
		a, err := probeWorker(sockPath, id)
		if err == nil && a.OK && a.UUID == id {
			ack = a
			break
		}
	}

	h := &workerHandle{
		uuid: id, name: name, createdAt: time.Now(),
		socketPath: sockPath, logPath: logPath, pid: ack.PID,
	}
	m.mu.Lock()
	m.sessions[id] = h
	m.mu.Unlock()
	m.registryCreate(id, name, h.createdAt)
	return h, nil
}

// closeSession 显式关闭：令 worker 回收整棵会话进程树并退出（TERM → 兜底 KILL）。
// worker 无应答时兜底 SIGKILL worker 本体（此为卡死路径：Shell 会因 PTY 关闭退出，
// 脱离进程组的后台作业泄漏是已接受行为，见 spec「退出路径」）。
func (m *manager) closeSession(id string) bool {
	m.mu.Lock()
	h := m.sessions[id]
	m.mu.Unlock()
	if h == nil {
		return false
	}

	ack, err := killWorker(h.socketPath, id)
	if err == nil && ack.OK {
		// 等 socket 消失（worker 完成回收并退出）。
		deadline := time.Now().Add(closeGrace + 2*time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(h.socketPath); err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	} else if h.pid > 0 && pidAlive(h.pid) {
		// worker 无应答（卡死）：兜底 SIGKILL worker 本体。
		_ = syscall.Kill(h.pid, syscall.SIGKILL)
	}
	_ = os.Remove(h.socketPath)

	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.registryEnd(id)
	return true
}

// dropStale 接入/列表时发现 socket 已死：清理并首次观测登记死亡。
// 不删其他内容，绝不触碰仍存活的 worker（spec 验收 18）。
func (m *manager) dropStale(id string, h *workerHandle) {
	_ = os.Remove(h.socketPath)
	if h.pid > 0 && pidAlive(h.pid) {
		_ = syscall.Kill(h.pid, syscall.SIGKILL)
	}
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.registryEnd(id)
}

// list 活会话实时查询：逐个探测，顺带清理死掉的。
func (m *manager) list() []map[string]any {
	m.mu.Lock()
	handles := make([]*workerHandle, 0, len(m.sessions))
	for _, h := range m.sessions {
		handles = append(handles, h)
	}
	m.mu.Unlock()

	out := make([]map[string]any, 0, len(handles))
	for _, h := range handles {
		ack, err := probeWorker(h.socketPath, h.uuid)
		if err != nil {
			m.dropStale(h.uuid, h)
			continue
		}
		h.pid = ack.PID
		h.busy.Store(ack.Busy)
		out = append(out, map[string]any{
			"id":       h.uuid,
			"name":     h.name,
			"created":  h.createdAt.Format(time.RFC3339),
			"attached": ack.Busy,
		})
	}
	return out
}

// renameSession 改显示名：id 与接入地址不变。
func (m *manager) renameSession(id, name string) bool {
	m.mu.Lock()
	h := m.sessions[id]
	if h == nil {
		m.mu.Unlock()
		return false
	}
	h.name = name
	m.mu.Unlock()
	m.registryRename(id, name)
	return true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
