// Limpet 后端入口。
// 开发阶段监听 :4040（可用 LIMPET_ADDR 覆盖）；
// 生产形态：web/dist 存在时由 Go 直接托管 H5，远程访问走 Cloudflare Tunnel。
package main

import (
	"log"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

var (
	tmuxConfPath string
	tmuxMissing  bool
)

func main() {
	addr := os.Getenv("LIMPET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4040"
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		log.Printf("tmux 未安装或不在 PATH 中: %v", err)
		tmuxMissing = true
	} else {
		conf, err := tmuxConf()
		if err != nil {
			log.Printf("tmux 配置初始化失败: %v", err)
			tmuxMissing = true
		} else {
			tmuxConfPath = conf
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/term", handleTermCreate)
	mux.HandleFunc("GET /api/term", handleTermList)
	mux.HandleFunc("POST /api/term/{id}/close", handleTermClose)
	mux.HandleFunc("POST /api/term/{id}/rename", handleTermRename)
	mux.HandleFunc("GET /api/ws/term/{id}", handleTermWS)
	serveStatic(mux)

	log.Printf("limpet server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
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
