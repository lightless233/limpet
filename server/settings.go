package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const (
	defaultScrollback = 10000
	maxScrollback     = 1000000
)

func validScrollback(lines *int) bool {
	return lines != nil && *lines >= 0 && *lines <= maxScrollback
}

func parseScrollback(data []byte) (int, error) {
	var settings struct {
		Scrollback *int `json:"scrollback"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return 0, err
	}
	if !validScrollback(settings.Scrollback) {
		return 0, errors.New("回滚行数必须为 0 到 1000000 之间的整数")
	}
	return *settings.Scrollback, nil
}

func (m *manager) loadSettings() error {
	m.scrollback = defaultScrollback
	data, err := os.ReadFile(m.settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取设置失败: %w", err)
	}
	lines, err := parseScrollback(data)
	if err == nil {
		m.scrollback = lines
		return nil
	}
	backup := fmt.Sprintf("%s.corrupt-%d", m.settingsPath, time.Now().UnixNano())
	log.Printf("解析设置 %s 失败: %v", m.settingsPath, err)
	if err := os.Rename(m.settingsPath, backup); err != nil {
		return fmt.Errorf("备份损坏的设置失败: %w", err)
	}
	log.Printf("已备份损坏的设置至 %s", backup)
	return nil
}

func readScrollback(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultScrollback, nil
	}
	if err != nil {
		return 0, err
	}
	return parseScrollback(data)
}

type scrollbackSetting struct {
	Scrollback int `json:"scrollback"`
}

type settingsResult struct {
	scrollbackSetting
	Applied     int `json:"applied"`
	Unsupported int `json:"unsupported"`
	Failed      int `json:"failed"`
}

func (m *manager) currentScrollback() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scrollback
}

func (m *manager) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, scrollbackSetting{Scrollback: m.currentScrollback()})
}

func (m *manager) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
		return
	}
	lines, err := parseScrollback(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := m.saveScrollback(lines)
	if err != nil {
		log.Printf("保存设置失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存设置失败"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *manager) saveScrollback(lines int) (settingsResult, error) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()
	result := settingsResult{scrollbackSetting: scrollbackSetting{Scrollback: lines}}
	if err := atomicWrite(m.settingsPath, mustJSON(result.scrollbackSetting)); err != nil {
		return result, err
	}
	m.mu.Lock()
	m.scrollback = lines
	m.mu.Unlock()
	_, _ = m.scanWorkers(false)
	m.mu.Lock()
	workers := make([]*workerHandle, 0, len(m.sessions))
	for _, h := range m.sessions {
		workers = append(workers, h)
	}
	m.mu.Unlock()

	for _, h := range workers {
		applied, err := m.syncWorkerScrollback(h, lines)
		switch {
		case staleSocket(err):
		case err != nil:
			result.Failed++
		case applied:
			result.Applied++
		default:
			result.Unsupported++
		}
	}
	return result, nil
}

func (m *manager) syncWorkerScrollback(h *workerHandle, lines int) (bool, error) {
	ack, err := workerOp(h.socketPath, handshakeReq{Op: scrollbackOp, UUID: h.uuid, Scrollback: &lines}, frameBudget)
	if staleSocket(err) {
		m.dropStale(h.uuid, h)
		return false, err
	}
	if err == nil {
		if ack.Op != scrollbackOp || ack.UUID != h.uuid {
			err = fmt.Errorf("应答身份或操作不符 (uuid=%q, op=%q)", ack.UUID, ack.Op)
		} else if ack.OK {
			return true, nil
		} else if ack.Reason == "未知 worker 操作" {
			return false, nil
		} else {
			err = fmt.Errorf("worker 拒绝请求: %q", ack.Reason)
		}
	}
	log.Printf("同步 worker %s 的回滚行数失败: %v", h.uuid, err)
	return false, err
}
