package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// writeJSON 统一的 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// nameValid 会话名（显示名）校验。规则沿用 001：非空、≤100 字符（UTF-8 计）、
// 禁 `:` `.` 与控制字符——tmux 的技术限制已消失，规则作为产品选择保留（spec 003）。
func nameValid(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	if strings.ContainsAny(name, ":.") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// readBody 读取请求体（上限 4KiB，名称场景绰绰有余）。
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 4096))
}

// handleCreate POST /api/term。创建即启动 Shell（80×24）并开始录制，worker 就绪后
// 才返回；失败 → 500，不留半死会话（spec 003）。
func (m *manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if b, err := readBody(r); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
	}
	name := strings.TrimSpace(body.Name)

	h, err := m.create(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": h.uuid})
}

// handleList GET /api/term。活会话实时查询（逐 worker 探测）。
func (m *manager) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.list())
}

// handleClose POST /api/term/{id}/close。worker 回收整棵会话进程树
// （前台任务 + 后台作业，验收 6），列表移除、endedAt 登记。
func (m *manager) handleClose(w http.ResponseWriter, r *http.Request) {
	if !m.closeSession(r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleRename POST /api/term/{id}/rename。只改显示名，id 与接入地址不变；
// 重名 409 保留为产品选择。
func (m *manager) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if b, err := readBody(r); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
	}
	name := strings.TrimSpace(body.Name)
	if !nameValid(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称不合法：不能包含 : 或 .，长度不超过 100 字符"})
		return
	}

	m.mu.Lock()
	h := m.byIDLocked(id)
	if h == nil {
		m.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	if name == h.name {
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
		return
	}
	for _, other := range m.sessions {
		if other.uuid != id && other.name == name {
			m.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "同名会话已存在"})
			return
		}
	}
	m.mu.Unlock()

	if !m.renameSession(id, name) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "会话不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}
