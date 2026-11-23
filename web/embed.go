//go:build limpet_embed

// Package web 把前端构建产物（dist/）内嵌进发布二进制（spec 005）。
//
// 只在发布构建里启用：go build -tags limpet_embed ./server——须先 pnpm build，
// 否则下面的 embed 模式找不到目录、编译失败。开发构建不带这个文件，不构建前端也能
// go build，页面由 vite(4041) 提供。
package web

import "embed"

// Dist 是 pnpm build 的产物。all: 连下划线、点开头的文件一并内嵌。
//
//go:embed all:dist
var Dist embed.FS
