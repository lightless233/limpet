package main

// worker 的启动方式（spec 005「systemd 部署」），由 LIMPET_WORKER_LAUNCH 选择：
//
//	direct（默认）  前台开发：server 直接 spawn，worker 自成会话。
//	systemd-run    服务部署：经 systemd-run --user 在独立的临时单元里启动。worker 不在
//	               server 的控制组里——server 单元停止或重启时不会被 systemd 一并清理
//	               （spec 003「运行方式与生命周期边界」）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type launchMode string

const (
	launchDirect     launchMode = "direct"
	launchSystemdRun launchMode = "systemd-run"
)

func workerUnit(id string) string { return "limpet-worker-" + id + ".service" }

func webPreset(preset, cwd string) (webLaunchConfig, error) {
	shell := defaultShell()
	config := webLaunchConfig{Cwd: cwd, Cmd: filepath.Base(shell), Argv: []string{shell, "-i"}}
	if preset == "" || preset == "shell" {
		return config, nil
	}
	switch filepath.Base(shell) {
	case "sh", "bash", "zsh", "dash", "ksh":
	default:
		return webLaunchConfig{}, fmt.Errorf("预设 %s 不支持登录 Shell %s", preset, filepath.Base(shell))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, shell, "-lic", `
unalias -a 2>/dev/null || true
unset -f -- "$1" 2>/dev/null || true
p=$(command -v -- "$1") && [ -n "$p" ] && [ -f "$p" ] && [ -x "$p" ]`, shell, preset)
	check.Dir, check.Env = cwd, append(cliEnv(), "TERM=xterm-256color")
	if err := check.Run(); err != nil {
		if ctx.Err() != nil {
			return webLaunchConfig{}, fmt.Errorf("检查登录环境中的 %s 超时", preset)
		}
		return webLaunchConfig{}, fmt.Errorf("登录环境中找不到可执行的 %s: %w", preset, err)
	}
	flag := "--yolo"
	if preset == "agy" {
		flag = "--dangerously-skip-permissions"
	}
	config.Cmd = preset
	// zsh's exec can resolve a shell function in its default mode.
	config.Argv = []string{shell, "-lic", `unset -f -- "$1" 2>/dev/null || true; exec "$@"`, shell, preset, flag}
	return config, nil
}

// launchFromEnv 读取 LIMPET_WORKER_LAUNCH（缺省 direct）；未知取值是配置错误。
func launchFromEnv(v string) (launchMode, error) {
	switch launchMode(strings.TrimSpace(v)) {
	case "", launchDirect:
		return launchDirect, nil
	case launchSystemdRun:
		return launchSystemdRun, nil
	}
	return "", fmt.Errorf("LIMPET_WORKER_LAUNCH 取值未知 %q（可选 direct / systemd-run）", v)
}

// checkLaunch 是启动前的保护：systemd-run 方式必须真能通过同一个 systemd 实例拉起单元，
// 否则拒绝启动——而不是等到创建会话时才失败；direct 方式若 server 自己就运行在服务单元里，
// 告警——这个组合下 worker 留在 server 的控制组里，会随 server 单元的停止或重启被一并结束。
func checkLaunch(l launchMode) error {
	switch l {
	case launchSystemdRun:
		out, err := exec.Command("systemd-run", "--user", "--wait", "--collect", "--quiet", "--pipe", "/bin/true").CombinedOutput()
		if err != nil {
			return fmt.Errorf("LIMPET_WORKER_LAUNCH=systemd-run 需要 systemd 用户管理器: %w: %s", err, bytes.TrimSpace(out))
		}
	case launchDirect:
		if unit := ownServiceUnit(); unit != "" {
			log.Printf("警告：server 运行在服务单元 %s 内，但 worker 启动方式是 direct——server 单元停止或重启时 worker 会被一并结束；服务部署请设置 LIMPET_WORKER_LAUNCH=systemd-run", unit)
		}
	}
	return nil
}

// ownServiceUnit 在 server 自己处于某个服务单元的控制组里时返回单元名（如 limpet-server.service）。
func ownServiceUnit() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		unit := path[strings.LastIndex(path, "/")+1:]
		if strings.HasSuffix(unit, ".service") {
			return unit
		}
		break
	}
	return ""
}

// spawnWorkerDirect 直接 spawn（前台开发）：worker 自成会话——server 的 Ctrl+C 所在的进程组
// 够不到它，终端关闭的挂断信号也不会波及。返回的 killHard 供 worker 迟迟不就绪时兜底。
func spawnWorkerDirect(exe, id, logPath string, config *workerConfig) (killHard func(), err error) {
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer lf.Close()

	cmd := exec.Command(exe, "worker", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = lf
	cmd.Stderr = lf
	var configIn *os.File
	if config != nil {
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		defer writer.Close()
		cmd.ExtraFiles = []*os.File{reader}
		cmd.Env = append(cliEnv(), workerConfigEnv+"=1")
		configIn = writer
	} else {
		cmd.Env = append(os.Environ(), webLaunchEnv+"=1")
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := cmd.Process
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	if configIn != nil {
		_ = cmd.ExtraFiles[0].Close()
		if err := json.NewEncoder(configIn).Encode(config); err != nil {
			_ = process.Kill()
			return nil, fmt.Errorf("传递 worker 启动配置: %w", err)
		}
	}
	return func() {
		_ = process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(killGrace + reapAfterKill + time.Second):
			_ = process.Kill()
			<-done
		}
	}, nil
}

func cliEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "LIMPET_") || key == "LIMPET_HOME" {
			env = append(env, kv)
		}
	}
	return env
}

// spawnWorkerSystemdRun 让 systemd 在独立的临时单元里启动 worker。systemd-run 不继承调用方的
// 环境，worker 的环境由 workerEnv 显式转交。返回的 killHard 供 worker 迟迟不就绪时兜底
// （停掉整个单元——此时还拿不到 worker 的 pid）。
func spawnWorkerSystemdRun(exe, id, logPath string) (killHard func(), err error) {
	args := []string{
		"--user", "--unit=" + workerUnit(id),
		"--collect", // 单元结束即回收——异常退出的 worker 不留下 failed 单元
		"--quiet",
		"--service-type=exec", // 进程 exec 成功才返回：二进制缺失之类的问题在创建会话时就暴露
		"-p", "StandardOutput=append:" + logPath,
		"-p", "StandardError=append:" + logPath,
		"-p", "TimeoutStopSec=5", // 单元收尾时残留进程 TERM → 5s → KILL
	}
	for _, kv := range workerEnv() {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--setenv="+webLaunchEnv+"=1", exe, "worker", id)
	out, err := exec.Command("systemd-run", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemd-run: %w: %s", err, bytes.TrimSpace(out))
	}
	return func() { stopWorkerUnit(id) }, nil
}

func stopWorkerUnit(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "stop", workerUnit(id)).CombinedOutput()
	if err != nil {
		log.Printf("停止 worker 单元 %s: %v: %s", workerUnit(id), err, bytes.TrimSpace(out))
	}
}

// systemdUnitEnv 是 systemd 给 server 自己这个单元专用的变量：描述的是 server，不是 worker。
var systemdUnitEnv = map[string]bool{
	"INVOCATION_ID": true, "JOURNAL_STREAM": true, "NOTIFY_SOCKET": true, "MANAGERPID": true,
	"SYSTEMD_EXEC_PID": true, "RUNTIME_DIRECTORY": true, "LISTEN_FDS": true, "LISTEN_PID": true,
	"WATCHDOG_PID": true, "WATCHDOG_USEC": true,
}

// workerEnv 是转交给 worker 单元的环境：server 自己的环境，去掉不该带进会话的两类——
// systemd 给 server 单元专用的变量，和 LIMPET_* 配置项（worker 用不上，却会出现在
// 用户的会话 Shell 里）。LIMPET_HOME 是例外：它就是 worker 要用的数据目录。
func workerEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if systemdUnitEnv[k] || (strings.HasPrefix(k, "LIMPET_") && k != "LIMPET_HOME") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
