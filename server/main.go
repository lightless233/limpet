// Limpet server：自研会话宿主（spec 003/004/005）。单二进制两种角色：
//
//	limpet-server               —— server：路由 + 注册表 + worker 监督。前台运行或作为
//	                               systemd 用户服务运行，由 LIMPET_WORKER_LAUNCH 选择 worker 的启动方式
//	limpet-server worker <uuid> —— worker：PTY 持有进程，自成会话，独立存活；
//	                               多视图（spec 004）：PTY 输出广播给所有接入的渲染器
//
// server 停止（Ctrl+C / kill / systemctl stop / 崩溃）绝不触碰 worker——它们继续运行，
// 新 server 靠扫描 socket 目录 + 探测握手找回（spec 003「运行方式与生命周期边界」）。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "worker" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		paths := sessionPaths{
			histDir:   filepath.Join(home, ".limpet", "history"),
			socketDir: filepath.Join(home, ".limpet", "workers"),
		}
		if err := runWorker(args[1], paths); err != nil {
			log.Fatal(err)
		}
		return
	}

	// worker 的启动方式要先核实：systemd-run 方式下拉不起单元就不启动（见 launch.go）。
	launch, err := launchFromEnv(os.Getenv("LIMPET_WORKER_LAUNCH"))
	if err != nil {
		log.Fatal(err)
	}
	if err := checkLaunch(launch); err != nil {
		log.Fatal(err)
	}

	addr := os.Getenv("LIMPET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4040"
	}
	m, err := newManager(launch)
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

	srv := &http.Server{Addr: addr, Handler: mux}

	// server 停止路径：只关 HTTP，绝不向 worker 发终止信号（它们继续运行）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("server 停止（worker 不受影响）")
		_ = srv.Close()
	}()

	log.Printf("limpet-server 监听 %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
