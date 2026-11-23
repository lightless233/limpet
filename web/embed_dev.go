//go:build !limpet_embed

package web

import "embed"

// Dist 为空：开发构建不带前端，server 退回磁盘上的 web/dist（若存在）。
var Dist embed.FS
