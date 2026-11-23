package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// manager 持有全部活会话与注册表。注册表 ~/.limpet/sessions.json 是死亡会话的
// 唯一记录（spec 003）：活会话以内存为准，死亡在首次观测时写 endedAt。
type manager struct {
	mu       sync.Mutex
	sessions map[string]*session

	home     string
	histDir  string
	regPath  string
	registry map[string]regEntry // uuid → 条目（含已结束）
}

type regEntry struct {
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

func newManager() (*manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	m := &manager{
		sessions:  map[string]*session{},
		home:      home,
		histDir:   filepath.Join(home, ".limpet", "history"),
		regPath:   filepath.Join(home, ".limpet", "sessions.json"),
		registry:  map[string]regEntry{},
	}
	if err := os.MkdirAll(m.histDir, 0o755); err != nil {
		return nil, err
	}
	m.loadRegistry()
	return m, nil
}

func (m *manager) loadRegistry() {
	b, err := os.ReadFile(m.regPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &m.registry)
}

// saveRegistry 原子写（tmp + rename），调用方持 m.mu。
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

// registryCreate 创建登记。
func (m *manager) registryCreate(id, name string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry[id] = regEntry{Name: name, CreatedAt: at}
	m.saveRegistry()
}

// registryEnd 死亡登记（首次观测语义）。
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

// registryRename 改名登记。
func (m *manager) registryRename(id, old, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.registry[id]; ok {
		e.Name = name
		m.registry[id] = e
		m.saveRegistry()
	}
}

func (m *manager) byID(id string) (*session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *manager) byIDLocked(id string) *session {
	return m.sessions[id]
}

// create 分配 uuid、登记、启动会话（PTY 就绪才返回）。
func (m *manager) create(name string) (*session, error) {
	if name != "" && !nameValid(name) {
		return nil, fmt.Errorf("名称不合法：不能包含 : 或 .，长度不超过 100 字符")
	}

	m.mu.Lock()
	if name != "" {
		for _, s := range m.sessions {
			if s.name == name {
				m.mu.Unlock()
				return nil, fmt.Errorf("同名会话已存在")
			}
		}
	}
	if name == "" {
		name = newID()
	}
	id := newID()
	m.mu.Unlock()

	s, err := startSession(m, id, name)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	m.registryCreate(id, name, s.createdAt)
	return s, nil
}

func (m *manager) list() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, map[string]any{
			"id":       s.id,
			"name":     s.name,
			"created":  s.createdAt.Format(time.RFC3339),
			"attached": s.attached(),
		})
	}
	return out
}

// remove 会话死亡后的收尾：从活表移除 + 死亡登记。
func (m *manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.registryEnd(id)
}

// newID 稳定 ID：8 字节随机 hex（spec 003 修订一）。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
