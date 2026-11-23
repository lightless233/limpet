// Limpet server：自研会话宿主（spec 003）。
//
// 单二进制。当前阶段（首个竖切片）所有会话都在本进程内；worker 进程拆分、
// 探测握手与 unix socket 协议按计划在生命周期单元引入，帧协议与路由不变。
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("LIMPET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4040"
	}

	m, err := newManager()
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/term", m.handleCreate)
	mux.HandleFunc("GET /api/term", m.handleList)
	mux.HandleFunc("POST /api/term/{id}/close", m.handleClose)
	mux.HandleFunc("POST /api/term/{id}/rename", m.handleRename)
	mux.HandleFunc("GET /api/ws/term/{id}", m.handleWS)

	// 未知 API 路由统一 404 JSON，不静默落进静态文件。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	})

	// web/dist 存在时托管前端产物；开发期前端走 vite(4041) 代理，不受影响。
	if st, err := os.Stat("web/dist/index.html"); err == nil && !st.IsDir() {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				if _, err := os.Stat("web/dist" + r.URL.Path); err != nil {
					http.ServeFile(w, r, "web/dist/index.html")
					return
				}
			}
			http.ServeFile(w, r, "web/dist"+r.URL.Path)
		})
	}

	log.Printf("limpet-server 监听 %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
