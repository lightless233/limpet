package main

// 会话手动排序的持久化：单独落盘 order.json。绝不写进 settings.json——worker 侧对
// settings.json 有严格解析（校验 scrollback 值，坏文件会让同步失败），排序数据塞进去
// 会破坏这份契约。order.json 只由 server 读写，worker 不感知。

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
)

// tempWorkspaceKey 与前端 TEMP_WORKSPACE_KEY 对齐：/tmp 下的会话统一归入
// 「临时工作区」，永远沉底，因此不参与 Workspaces 的组间手动排序。
const tempWorkspaceKey = "/tmp"

// maxOrderBody 限制 PUT /api/order 的请求体大小（排序数据只有 ID 列表，64KiB 绰绰有余）。
const maxOrderBody = 64 * 1024

// sessionOrder 是手动排序的唯一持久形态。Workspaces 是手动排好的普通工作区 key 列表
// （不含 "/tmp"；空切片＝组间走默认时间倒序）；Sessions 按工作区 key（含 "" 主目录与
// "/tmp" 临时工作区）记录组内手动排好的会话 ID 列表（无条目＝该组走默认时间倒序）。
type sessionOrder struct {
	Workspaces []string            `json:"workspaces"`
	Sessions   map[string][]string `json:"sessions"`
}

// normalizeSessionOrder 过滤非法数据：Workspaces 去掉 "/tmp" 与重复项，Sessions 去掉
// 空串与重复项并清掉空数组 key。GET/PUT 返回的都是规范化后的形态。
func normalizeSessionOrder(order sessionOrder) sessionOrder {
	workspaces := make([]string, 0, len(order.Workspaces))
	for _, key := range order.Workspaces {
		if key == tempWorkspaceKey || slices.Contains(workspaces, key) {
			continue
		}
		workspaces = append(workspaces, key)
	}
	sessions := make(map[string][]string, len(order.Sessions))
	for key, ids := range order.Sessions {
		cleaned := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != "" && !slices.Contains(cleaned, id) {
				cleaned = append(cleaned, id)
			}
		}
		if len(cleaned) > 0 {
			sessions[key] = cleaned
		}
	}
	return sessionOrder{Workspaces: workspaces, Sessions: sessions}
}

// loadOrder 在启动时读取 order.json。文件缺失用默认空值；损坏只记日志并回退默认，
// 不阻塞服务启动（排序丢了可以重拖，服务起不来才是事故）。
func (m *manager) loadOrder() {
	m.orderMu.Lock()
	defer m.orderMu.Unlock()
	m.order = sessionOrder{Workspaces: []string{}, Sessions: map[string][]string{}}
	data, err := os.ReadFile(m.orderPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("读取会话排序 %s 失败: %v", m.orderPath, err)
		return
	}
	var parsed sessionOrder
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Printf("解析会话排序 %s 失败: %v", m.orderPath, err)
		return
	}
	m.order = normalizeSessionOrder(parsed)
}

func (m *manager) currentOrder() sessionOrder {
	m.orderMu.Lock()
	defer m.orderMu.Unlock()
	return m.order
}

// saveOrder 规范化后原子落盘并更新内存快照。orderMu 独立于 m.mu：排序与注册表
// 生命周期无关，写盘也不该阻塞会话探测路径。
func (m *manager) saveOrder(order sessionOrder) (sessionOrder, error) {
	cleaned := normalizeSessionOrder(order)
	m.orderMu.Lock()
	defer m.orderMu.Unlock()
	if err := atomicWrite(m.orderPath, mustJSON(cleaned)); err != nil {
		return cleaned, err
	}
	m.order = cleaned
	return cleaned, nil
}

func (m *manager) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.currentOrder())
}

func (m *manager) handlePutOrder(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOrderBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
		return
	}
	var order sessionOrder
	if err := json.Unmarshal(body, &order); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	saved, err := m.saveOrder(order)
	if err != nil {
		log.Printf("保存会话排序失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存会话排序失败"})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}
