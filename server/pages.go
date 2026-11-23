package main

// 前端页面的托管（spec 005）。发布二进制把 web/dist 内嵌在 limpet/web 包里；开发构建没有
// 内嵌，退回磁盘上的 web/dist（从仓库根启动时）——开发时页面通常由 vite(4041) 提供，这条
// 退路只是让页面不经 vite 也能打开。两处都没有就只提供 API。

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	limpetweb "limpet/web"
)

// pageHandler 返回前端页面的处理器；没有可托管的页面时返回 nil。
func pageHandler() http.Handler {
	if dist, err := fs.Sub(limpetweb.Dist, "dist"); err == nil {
		if _, err := fs.Stat(dist, "index.html"); err == nil {
			return serveDist(dist)
		}
	}
	if st, err := os.Stat("web/dist/index.html"); err == nil && !st.IsDir() {
		return serveDist(os.DirFS("web/dist"))
	}
	return nil
}

// serveDist 托管一份前端构建产物：文件名带内容哈希的 assets/ 可以长期缓存；其余路径
// （含单页应用的深链接、不存在的路径）一律回到入口页，入口页每次都回源校验。
func serveDist(dist fs.FS) http.Handler {
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil
	}
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if st, err := fs.Stat(dist, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
