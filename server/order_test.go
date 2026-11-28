package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func orderRequest(m *manager, method, body string) *httptest.ResponseRecorder {
	reply := httptest.NewRecorder()
	m.handler().ServeHTTP(reply, httptest.NewRequest(method, "/api/order", strings.NewReader(body)))
	return reply
}

func requireOrder(t *testing.T, m *manager, want sessionOrder) {
	t.Helper()
	reply := orderRequest(m, "GET", "")
	var got sessionOrder
	if err := json.Unmarshal(reply.Body.Bytes(), &got); reply.Code != 200 || err != nil {
		t.Fatalf("GET /api/order: status=%d body=%s err=%v", reply.Code, reply.Body, err)
	}
	if len(got.Workspaces) != len(want.Workspaces) || len(got.Sessions) != len(want.Sessions) {
		t.Fatalf("GET /api/order = %s, want %+v", reply.Body, want)
	}
	for i, key := range want.Workspaces {
		if got.Workspaces[i] != key {
			t.Fatalf("GET /api/order workspaces = %v, want %v", got.Workspaces, want.Workspaces)
		}
	}
	for key, ids := range want.Sessions {
		if gotIDs, ok := got.Sessions[key]; !ok || len(gotIDs) != len(ids) {
			t.Fatalf("GET /api/order sessions[%q] = %v, want %v", key, got.Sessions[key], ids)
		}
	}
}

func TestSessionOrderPersistence(t *testing.T) {
	m := testManager(t)

	// 默认空值：文件不存在时不落盘，GET 返回空结构。
	requireOrder(t, m, sessionOrder{})
	if _, err := os.Stat(m.orderPath); !os.IsNotExist(err) {
		t.Fatalf("GET created order.json: %v", err)
	}

	// 非法 JSON 一律 400，且不改动磁盘。
	for _, invalid := range []string{`{`, `{"workspaces":"x"}`, `{"sessions":{"a":1}}`, `[]`} {
		if reply := orderRequest(m, "PUT", invalid); reply.Code != 400 {
			t.Fatalf("invalid PUT %q returned %d: %s", invalid, reply.Code, reply.Body)
		}
	}
	requireOrder(t, m, sessionOrder{})

	// PUT 后 GET 持久化：响应与磁盘均为规范化形态（0600 权限）。
	put := orderRequest(m, "PUT", `{"workspaces":["/tmp","/home/u/a","/home/u/a",""],
		"sessions":{"":["","s1","s1","s2"],"/tmp":["t1","t1"],"gone":[]}}`)
	if put.Code != 200 {
		t.Fatalf("PUT returned %d: %s", put.Code, put.Body)
	}
	var saved sessionOrder
	if err := json.Unmarshal(put.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Workspaces) != 2 || saved.Workspaces[0] != "/home/u/a" || saved.Workspaces[1] != "" {
		t.Fatalf("PUT reply kept junk workspaces: %+v", saved.Workspaces)
	}
	if len(saved.Sessions) != 2 {
		t.Fatalf("PUT reply kept empty-array key: %+v", saved.Sessions)
	}
	if ids := saved.Sessions["/tmp"]; len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("PUT reply /tmp ids: %+v", ids)
	}
	if ids := saved.Sessions[""]; len(ids) != 2 || ids[0] != "s1" || ids[1] != "s2" {
		t.Fatalf("PUT reply kept junk ids: %+v", ids)
	}
	info, err := os.Stat(m.orderPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("order file: %v %v", info, err)
	}
	requireOrder(t, m, saved)

	// 模拟重启：同一 orderPath 重新 loadOrder 后数据仍在。
	restarted := &manager{orderPath: m.orderPath}
	restarted.loadOrder()
	requireOrder(t, restarted, saved)

	// 磁盘损坏：记日志、回退默认空值，不阻塞启动，也不改写坏文件。
	if err := os.WriteFile(m.orderPath, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := &manager{orderPath: m.orderPath}
	corrupt.loadOrder()
	requireOrder(t, corrupt, sessionOrder{})
	if data, err := os.ReadFile(m.orderPath); err != nil || string(data) != "{oops" {
		t.Fatalf("corrupt file was touched: %q %v", data, err)
	}

	// 超过 64KiB 上限的请求体 400。
	big := `{"workspaces":["` + strings.Repeat("w", maxOrderBody) + `"]}`
	if reply := orderRequest(m, "PUT", big); reply.Code != 400 {
		t.Fatalf("oversized PUT returned %d", reply.Code)
	}

	// 写入失败（目录缺失）返回 500，内存快照不变。
	broken := &manager{orderPath: filepath.Join(t.TempDir(), "missing-dir", "order.json")}
	broken.order = saved
	if reply := orderRequest(broken, "PUT", `{"workspaces":["/x"]}`); reply.Code != 500 {
		t.Fatalf("write failure returned %d: %s", reply.Code, reply.Body)
	}
	requireOrder(t, broken, saved)
}

func TestSessionOrderConcurrentAccess(t *testing.T) {
	m := testManager(t)
	var concurrent sync.WaitGroup
	for i := range 8 {
		i := i
		concurrent.Go(func() {
			for range 20 {
				body := `{"workspaces":["/w` + string(rune('a'+i)) + `"],"sessions":{"":["s` + string(rune('a'+i)) + `"]}}`
				if reply := orderRequest(m, "PUT", body); reply.Code != 200 {
					t.Errorf("concurrent PUT: %d %s", reply.Code, reply.Body)
					return
				}
				if reply := orderRequest(m, "GET", ""); reply.Code != 200 {
					t.Errorf("concurrent GET: %d %s", reply.Code, reply.Body)
					return
				}
			}
		})
	}
	concurrent.Wait()
	// orderMu 串行化落盘与内存更新：最终磁盘与内存一致，且只含规范化数据。
	requireOrder(t, m, m.currentOrder())
	data, err := os.ReadFile(m.orderPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk sessionOrder
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("disk JSON: %v (%s)", err, data)
	}
	if len(onDisk.Workspaces) != 1 || len(onDisk.Sessions) != 1 {
		t.Fatalf("disk order after concurrent PUTs: %s", data)
	}
}
