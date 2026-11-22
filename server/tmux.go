// tmux 隔离层：Limpet 使用独立 socket 与托管配置，与用户个人 tmux 环境互不影响。
// tmux 是会话常驻状态的唯一事实来源：创建/列表/重命名/关闭都落在 tmux 上，
// Go 进程内存只保留运行时接入状态（claimed、客户端进程）。
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const tmuxSocket = "limpet"

// maxSessionNameLen 限制会话名长度（UTF-8 字符数，非字节）。
const maxSessionNameLen = 100

var errTmuxMissing = errors.New("tmux 未安装或不在 PATH 中")

// tmuxConf 在 ~/.limpet/tmux.conf 维护托管配置，每次启动重写。
// 行为约束：Esc 响应无明显延迟（escape-time）、回滚充足（history-limit）、
// 会话尺寸跟随唯一接入的客户端（aggressive-resize）。
func tmuxConf() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".limpet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tmux.conf")
	conf := `# Limpet managed tmux configuration (rewritten on every server start)
set -g escape-time 10
set -g history-limit 50000
set -g aggressive-resize on
set -g renumber-windows on
set -g mouse off
# 浏览器页面本身就是 chrome（会话信息在页头），关闭状态栏省出一整行
set -g status off
`
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// tmuxBase 拼接 socket 与配置参数。
func tmuxBase(conf string) []string {
	return []string{"-L", tmuxSocket, "-f", conf}
}

// runTmux 执行 tmux 子命令，返回 stdout；失败时错误信息取自 stderr。
func runTmux(conf string, args ...string) (string, error) {
	full := append(tmuxBase(conf), args...)
	cmd := exec.Command("tmux", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}

// tmuxSessionNameValid 校验会话名：非空、长度受限、不含 tmux 目标解析保留字符。
func tmuxSessionNameValid(name string) bool {
	if name == "" || uint(len([]rune(name))) > maxSessionNameLen {
		return false
	}
	return !strings.ContainsAny(name, ":.")
}

// tmuxSessionExists 判断会话是否存在（socket 无服务端视为不存在）。
func tmuxSessionExists(conf, id string) bool {
	_, err := runTmux(conf, "has-session", "-t", id)
	return err == nil
}

// tmuxCreate 新建 detached 会话（Shell 随会话立即启动）。
func tmuxCreate(conf, id string) error {
	_, err := runTmux(conf, "new-session", "-d", "-s", id, "-x", "80", "-y", "24")
	return err
}

// tmuxClientArgs 构造接入命令：会话存在则 attach，不存在则创建并接入。
// 由调用方先行确认会话存在，-A 仅作为兜底。
func tmuxClientArgs(conf, id string, cols, rows uint16) []string {
	return append(tmuxBase(conf),
		"new-session", "-A", "-s", id,
		"-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows),
	)
}

// tmuxList 列出会话：返回 (名, 创建epoch秒, 客户端数) 列表。
// socket 无服务端（尚无任何会话）视为空列表。
func tmuxList(conf string) ([]struct {
	Name     string
	Created  int64
	Clients  int
}, error) {
	out, err := runTmux(conf, "list-sessions", "-F", "#{session_created}\t#{session_attached}\t#{session_name}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return nil, nil
		}
		return nil, err
	}
	var sessions []struct {
		Name     string
		Created  int64
		Clients  int
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		var created, clients int64
		if _, err := fmt.Sscanf(parts[0], "%d", &created); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(parts[1], "%d", &clients); err != nil {
			continue
		}
		sessions = append(sessions, struct {
			Name     string
			Created  int64
			Clients  int
		}{Name: parts[2], Created: created, Clients: int(clients)})
	}
	return sessions, nil
}

// tmuxRename 重命名会话。
func tmuxRename(conf, oldID, newID string) error {
	_, err := runTmux(conf, "rename-session", "-t", oldID, newID)
	return err
}

// tmuxKill 关闭会话（Shell 一并终止）。
func tmuxKill(conf, id string) error {
	_, err := runTmux(conf, "kill-session", "-t", id)
	return err
}
