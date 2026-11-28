package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
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
	if name == "" || utf8.RuneCountInString(name) > 100 {
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
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	const maxBody = 4096
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
}

// handleCreate POST /api/term。创建即启动 Shell（80×24）并开始录制，worker 就绪后
// 才返回；失败 → 500，不留半死会话（spec 003）。非法名称 400；与活会话重名 409。
func (m *manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Cwd    string `json:"cwd"`
		Preset string `json:"preset"`
	}
	b, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
		return
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
	}
	name := strings.TrimSpace(body.Name)
	if name != "" && !nameValid(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称不合法：不能包含 : 或 .，长度不超过 100 字符"})
		return
	}

	preset := strings.TrimSpace(body.Preset)
	switch preset {
	case "", "shell", "codex", "agy":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知启动预设"})
		return
	}
	cwd, err := workspaceDir(body.Cwd)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errHome) {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	config, err := webPreset(preset, cwd)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h, err := m.create(name, &config, preset != "" || strings.TrimSpace(body.Cwd) != "")
	if err != nil {
		if errors.Is(err, errDuplicate) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]string{
		"id": h.uuid, "name": config.Name, "cwd": displayCwd(config.Cwd, home), "cmd": config.Cmd,
	})
}

var errHome = errors.New("无法获取用户主目录")

func workspaceDir(cwd string) (string, error) {
	if strings.ContainsRune(cwd, 0) {
		return "", errors.New("工作目录不能包含 NUL")
	}
	if strings.TrimSpace(cwd) == "" || cwd == "~" || strings.HasPrefix(cwd, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errHome
		}
		if suffix, ok := strings.CutPrefix(cwd, "~/"); ok {
			cwd = filepath.Join(home, suffix)
		} else {
			cwd = home
		}
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("工作目录必须是绝对路径")
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", errors.New("工作目录不存在、不可访问或不是目录")
	}
	return cwd, nil
}

func (m *manager) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.workspaces())
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type dirListing struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent"`
	Dirs   []dirEntry `json:"dirs"`
}

// handleListDirs GET /api/dirs。只读列出指定目录下的子目录与目录符号链接，供新建会话浏览路径。
func (m *manager) handleListDirs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, err := workspaceDir(q.Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errHome) {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	hiddenParam := q.Get("hidden")
	showHidden := hiddenParam == "1" || hiddenParam == "true"

	entries, err := os.ReadDir(dir)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrPermission) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": "读取目录失败：" + err.Error()})
		return
	}

	home, _ := os.UserHomeDir()
	dirs := make([]dirEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		fullPath := filepath.Join(dir, name)
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(fullPath)
			isDir = err == nil && info.IsDir()
		}
		if !isDir {
			continue
		}
		dirs = append(dirs, dirEntry{
			Name: name,
			Path: displayCwd(fullPath, home),
		})
	}
	slices.SortFunc(dirs, func(a, b dirEntry) int {
		if d := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})

	parent := ""
	if p := filepath.Dir(dir); p != dir {
		parent = displayCwd(p, home)
	}
	writeJSON(w, http.StatusOK, dirListing{
		Path:   displayCwd(dir, home),
		Parent: parent,
		Dirs:   dirs,
	})
}

// handleList GET /api/term。活会话实时查询（逐 worker 探测）。
func (m *manager) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.list())
}

// handleClose POST /api/term/{id}/close。worker 回收整棵会话进程树
// （前台任务 + 后台作业，验收 6），列表移除、endedAt 登记。
func (m *manager) handleClose(w http.ResponseWriter, r *http.Request) {
	if err := m.closeSession(r.PathValue("id")); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
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
	b, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
		return
	}
	if len(b) > 0 {
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

	if err := m.renameSession(id, name); err != nil {
		status := http.StatusConflict
		if errors.Is(err, errNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}
