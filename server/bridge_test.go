package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHandshakeAndCrossOriginGuards(t *testing.T) {
	m := testManager(t)
	h, err := createTestShell(m, "terminal")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("worker 报文没有协议版本", func(t *testing.T) {
		conn, err := net.DialTimeout("unix", h.socketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := writeJSONFrame(conn, ftHandshake, map[string]string{"op": "probe", "uuid": h.uuid}); err != nil {
			t.Fatal(err)
		}
		kind, payload, err := readFrame(conn)
		if err != nil || kind != ftHandshakeAck {
			t.Fatalf("握手应答类型 = %v, err=%v", kind, err)
		}
		var ack handshakeAck
		if err := json.Unmarshal(payload, &ack); err != nil || !ack.OK || ack.Op != "probe" || ack.UUID != h.uuid {
			t.Fatalf("握手应答 = %s, err=%v", payload, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["proto"]; exists {
			t.Errorf("握手应答仍带 proto: %s", payload)
		}
	})

	server := httptest.NewServer(m.handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	post := func(t *testing.T, origin, name string, want int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/term/"+h.uuid+"/rename",
			strings.NewReader(string(mustJSON(map[string]string{"name": name}))))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("origin=%q POST 状态=%d，期望 %d", origin, res.StatusCode, want)
		}
	}
	t.Run("HTTP 同源写入与跨站拦截", func(t *testing.T) {
		post(t, server.URL, "accepted", http.StatusOK)
		post(t, "https://cross-site.invalid", "rejected", http.StatusForbidden)
		if list := m.list(); len(list) != 1 || list[0]["name"] != "accepted" {
			t.Errorf("HTTP 写入后的列表 = %v", list)
		}
	})
	t.Run("WS 同源升级与跨站拦截", func(t *testing.T) {
		url := server.URL + "/api/ws/term/" + h.uuid
		conn, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {"https://cross-site.invalid"}},
		})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Fatalf("跨站 WS 响应=%v, err=%v", res, err)
		}

		conn, _, err = websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {server.URL}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		kind, body, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageText || string(body) != `{"type":"ready","cols":80,"rows":24}` {
			t.Fatalf("同源接入的首帧 = %v %q, err=%v", kind, body, err)
		}
	})
}

func TestRelayLegacyReadyAndExplicitResize(t *testing.T) {
	for _, payload := range []string{"", `{"cols":80,"rows":null}`} {
		t.Run(payload, func(t *testing.T) {
			serverEnd, worker := net.Pipe()
			defer worker.Close()
			_ = worker.SetDeadline(time.Now().Add(3 * time.Second))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err == nil {
					relay(&wsConn{c: conn}, newFrameConn(serverEnd))
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := writeFrame(worker, ftReady, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			kind, body, err := conn.Read(ctx)
			if err != nil || kind != websocket.MessageText || string(body) != `{"type":"ready"}` {
				t.Fatalf("旧 worker ready = %v %q, %v", kind, body, err)
			}
			for _, msg := range []string{
				`{"type":"resize"}`, `{"type":"resize","cols":null,"rows":0}`,
				`{"type":"resize","cols":0,"rows":20}`, `{"type":"resize","cols":0,"rows":0}`,
			} {
				if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("marker")); err != nil {
				t.Fatal(err)
			}
			frame, data, err := readFrame(worker)
			if err != nil || frame != ftResize || string(data) != `{"cols":0,"rows":0}` {
				t.Fatalf("显式清空 = %v %q, %v", frame, data, err)
			}
			frame, data, err = readFrame(worker)
			if err != nil || frame != ftInput || string(data) != "marker" {
				t.Fatalf("清空后的下一帧 = %v %q, %v", frame, data, err)
			}
		})
	}
}
