// Limpet 后端入口。
// 开发阶段监听 :4040，仅提供 /api 前缀的 HTTP 与 WebSocket 接口；
// 前端页面由 Vite dev server (:4041) 托管并代理 /api 到这里。
// 生产阶段由 go:build 之后的静态资源托管接管（见 serveStatic）。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
)

func main() {
	addr := os.Getenv("LIMPET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4040"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", handleHello)
	mux.HandleFunc("GET /api/ws/echo", handleEcho)
	mux.HandleFunc("POST /api/term", handleTermCreate)
	mux.HandleFunc("GET /api/ws/term/{id}", handleTermWS)
	serveStatic(mux)

	log.Printf("limpet server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "hello from limpet",
		"time":    time.Now().Format(time.RFC3339),
	})
}

// handleEcho 是 WebSocket 链路的冒烟测试端点：收到什么就回什么。
// 后续真正的终端转发会替换掉它。
func handleEcho(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	defer c.Close(websocket.StatusInternalError, "internal error")

	ctx := r.Context()
	for {
		msgType, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, msgType, data); err != nil {
			return
		}
	}
}

// serveStatic 在 web/dist 存在时托管构建产物（生产形态：Go 直接提供 H5）。
// 不存在时静默跳过，开发阶段由 Vite 提供页面。
func serveStatic(mux *http.ServeMux) {
	dist := filepath.Join("web", "dist")
	if _, err := os.Stat(dist); err != nil {
		return
	}

	fileServer := http.FileServer(http.Dir(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if _, err := os.Stat(filepath.Join(dist, filepath.Clean(r.URL.Path))); err != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
