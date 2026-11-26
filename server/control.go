package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	controlBudget = 2 * time.Second
	controlLimit  = 16 << 10
)

type controlRequest struct {
	Op           string `json:"op"`
	UUID         string `json:"uuid,omitempty"`
	Name         string `json:"name,omitempty"`
	ExplicitName bool   `json:"explicitName,omitempty"`
}

type controlReply struct {
	Name     string          `json:"name,omitempty"`
	Sessions []listedSession `json:"sessions"`
	Error    string          `json:"error,omitempty"`
}

func lockServer(base string) (*os.File, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(base, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("无法独占 Limpet 数据目录 %s: %w", base, err)
	}
	return f, nil
}

// Caller owns the directory lock before touching the control socket.
func listenControl(base string) (net.Listener, error) {
	path := filepath.Join(base, "server.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("控制通道路径已被非 socket 文件占用: %s", path)
		}
		c, err := net.DialTimeout("unix", path, controlBudget)
		switch {
		case err == nil:
			_ = c.Close()
			return nil, fmt.Errorf("控制通道已在监听: %s", path)
		case errors.Is(err, syscall.ECONNREFUSED):
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func (m *manager) serveControl(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go m.handleControl(c)
	}
}

func (m *manager) handleControl(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(controlBudget))
	line, err := bufio.NewReaderSize(c, controlLimit).ReadSlice('\n')
	if err != nil {
		return
	}
	var req controlRequest
	var reply controlReply
	switch {
	case json.Unmarshal(line, &req) != nil:
		reply.Error = "控制请求不是合法 JSON"
	case req.Op == "list":
		reply.Sessions = m.listSessions()
	case req.Op == "register":
		if !uuidRe.MatchString(req.UUID) || ((req.Name != "" || req.ExplicitName) && !nameValid(req.Name)) {
			reply.Error = "会话 ID 或名称不合法"
			break
		}
		h, err := m.discover(req.UUID, req.Name, false)
		if err != nil {
			reply.Error = err.Error()
			break
		}
		m.mu.Lock()
		reply.Name = h.name
		m.mu.Unlock()
	default:
		reply.Error = "未知本机控制操作"
	}
	_ = json.NewEncoder(c).Encode(reply)
}
