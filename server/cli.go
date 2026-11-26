package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

const cliHelp = `用法
  limpet                         在当前目录启动交互式 Shell
  limpet [-n 名称] [--] 命令 ...   启动命令并接入
  limpet ls                      列出活跃会话
  limpet attach 名称或ID          接入已有会话
  limpet server                  启动 HTTP server
  limpet version                 显示版本
`

func runCLI(args []string) (int, error) {
	var name string
	var explicit, literal bool
parse:
	for len(args) > 0 {
		switch args[0] {
		case "-h", "--help":
			fmt.Print(cliHelp)
			return 0, nil
		case "-n", "--name":
			if len(args) < 2 || !nameValid(args[1]) {
				return 1, fmt.Errorf("名称不合法，不能包含 :、. 或控制字符，最多 100 字符")
			}
			name, explicit = args[1], true
			args = args[2:]
		case "--":
			literal = true
			args = args[1:]
			if len(args) == 0 {
				return 1, fmt.Errorf("-- 后缺少目标命令")
			}
			break parse
		default:
			if strings.HasPrefix(args[0], "-") {
				return 1, fmt.Errorf("未知 Limpet 选项 %q，请用 limpet --help 查看用法", args[0])
			}
			break parse
		}
	}
	base, err := limpetHome()
	if err != nil {
		return 1, err
	}
	if !literal && len(args) > 0 {
		switch args[0] {
		case "help", "server", "ls", "attach", "version", "worker":
			want := 1
			if args[0] == "attach" {
				want = 2
			}
			if explicit || len(args) != want || args[0] == "worker" {
				return 1, fmt.Errorf("limpet %s 参数错误，请用 limpet --help 查看用法", args[0])
			}
			switch args[0] {
			case "help":
				fmt.Print(cliHelp)
				return 0, nil
			case "version":
				fmt.Println(version)
				return 0, nil
			case "server":
				if err := runServer(); err != nil {
					return 1, err
				}
				return 0, nil
			case "ls":
				items, err := cliSessions(base)
				if err != nil {
					return 1, err
				}
				fmt.Println("名称\tID\t接入\t目录\t命令")
				for _, row := range items {
					fmt.Printf("%s\t%s\t%t\t%s\t%s\n", row.Name, row.ID, row.Attached, row.Cwd, row.Cmd)
				}
				return 0, nil
			case "attach":
				return cliAttach(base, args[1])
			}
		}
	}
	return cliStart(base, name, explicit, args)
}

func cliControl(ctx context.Context, base string, budget time.Duration, req controlRequest) (controlReply, error) {
	var reply controlReply
	d := net.Dialer{Timeout: budget}
	c, err := d.DialContext(ctx, "unix", filepath.Join(base, "server.sock"))
	if err != nil {
		return reply, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(budget))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return reply, err
	}
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func cliSessions(base string) ([]listedSession, error) {
	reply, err := cliControl(context.Background(), base, controlBudget+time.Second, controlRequest{Op: "list"})
	if err == nil {
		return reply.Sessions, nil
	}
	if !staleSocket(err) {
		return nil, fmt.Errorf("读取本机控制列表: %w", err)
	}

	path := filepath.Join(base, "workers")
	files, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 worker 目录: %w", err)
	}
	var registry map[string]regEntry
	data, err := os.ReadFile(filepath.Join(base, "sessions.json"))
	if err == nil {
		if err := json.Unmarshal(data, &registry); err != nil {
			return nil, fmt.Errorf("解析会话注册表: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取会话注册表: %w", err)
	}
	var items []listedSession
	for _, file := range files {
		id, ok := strings.CutSuffix(file.Name(), ".sock")
		if !ok || !uuidRe.MatchString(id) || file.Type()&os.ModeSocket == 0 {
			continue
		}
		reg := registry[id]
		if reg.EndedAt != nil {
			continue
		}
		ack, err := probeWorker(filepath.Join(path, file.Name()), id)
		if staleSocket(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("探测会话 %s: %w", id, err)
		}
		if !ack.readyFor(id) {
			continue
		}
		name := reg.Name
		if name == "" {
			name = ack.Name
		}
		if name == "" {
			name = id
		}
		if reg.CreatedAt.IsZero() && ack.CreatedAt != 0 {
			reg.CreatedAt = time.UnixMilli(ack.CreatedAt)
		}
		created := ""
		if !reg.CreatedAt.IsZero() {
			created = reg.CreatedAt.Format(time.RFC3339)
		}
		if reg.Cwd == "" {
			reg.Cwd = ack.Cwd
		}
		if reg.Cmd == "" {
			reg.Cmd = ack.Cmd
		}
		items = append(items, listedSession{ID: id, Name: name, Attached: ack.Busy, Created: created, Cwd: reg.Cwd, Cmd: reg.Cmd})
	}
	return items, nil
}

func cliAttach(base, query string) (int, error) {
	tty, err := openCLITerminal()
	if err != nil {
		return 1, err
	}
	defer tty.close()
	rows, err := cliSessions(base)
	if err != nil {
		return 1, err
	}
	var ids []string
	for _, row := range rows {
		if row.ID == query || row.Name == query {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) == 0 {
		return 1, fmt.Errorf("找不到活跃会话 %q", query)
	}
	if len(ids) > 1 {
		slices.Sort(ids)
		return 1, fmt.Errorf("会话 %q 有歧义，候选 ID：%s", query, strings.Join(ids, "、"))
	}
	return tty.run(true, func(ctx context.Context) (*frameConn, error) {
		return connectCLIViewer(ctx, base, ids[0], "attach")
	}, nil)
}

func cliStart(base, name string, explicit bool, argv []string) (int, error) {
	tty, err := openCLITerminal()
	if err != nil {
		return 1, err
	}
	defer tty.close()
	if len(argv) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = loginShell()
		}
		argv = []string{shell, "-i"}
	}
	if explicit {
		reply, err := cliControl(context.Background(), base, controlBudget+time.Second, controlRequest{Op: "list"})
		if err != nil && !staleSocket(err) {
			return 1, fmt.Errorf("检查会话名称: %w", err)
		}
		for _, row := range reply.Sessions {
			if row.Name == name {
				return 1, fmt.Errorf("%w: %s", errDuplicate, name)
			}
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("获取当前目录: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("定位 Limpet 可执行文件: %w", err)
	}
	id := newID()
	if !explicit {
		name = strings.Map(func(r rune) rune {
			if r == ':' || r == '.' || r == '/' || unicode.IsControl(r) {
				return '-'
			}
			return r
		}, filepath.Base(cwd)+"-"+filepath.Base(argv[0]))
		runes := []rune(name)
		name = string(runes[:min(len(runes), 96)])
		if !nameValid(name) {
			name = id
		}
	}

	dir := filepath.Join(base, "workers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 1, err
	}
	logPath := filepath.Join(dir, id+".log")
	return tty.run(false, func(ctx context.Context) (*frameConn, error) {
		dims, err := tty.dims()
		if err != nil {
			return nil, err
		}
		config := workerConfig{Argv: argv, Cwd: cwd, Dims: dims, Name: name, WaitFirstViewer: true}
		if _, err := spawnWorkerDirect(exe, id, logPath, &config); err != nil {
			return nil, fmt.Errorf("启动 worker: %w", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, firstViewerWait)
		defer cancel()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			fc, err := connectCLIViewer(waitCtx, base, id, "start")
			if !staleSocket(err) {
				return fc, err
			}
			select {
			case <-waitCtx.Done():
				return nil, fmt.Errorf("等待 worker %s 就绪失败（日志 %s）: %w", id, logPath, waitCtx.Err())
			case <-tick.C:
			}
		}
	}, func(ctx context.Context) error {
		_, err := cliControl(ctx, base, 500*time.Millisecond, controlRequest{
			Op: "register", UUID: id, Name: name, ExplicitName: explicit,
		})
		var timeout net.Error
		if ctx.Err() != nil || staleSocket(err) || (errors.As(err, &timeout) && timeout.Timeout()) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("通知本机 server 登记会话 %s: %w", id, err)
		}
		return nil
	})
}

func connectCLIViewer(ctx context.Context, base, id, op string) (*frameConn, error) {
	path := filepath.Join(base, "workers", id+".sock")
	fc, ack, err := openWorkerContext(ctx, path, id, op, frameBudget)
	if err != nil {
		return nil, fmt.Errorf("连接会话 %s: %w", id, err)
	}
	if ack.OK && ack.UUID == id && ack.Op == op {
		return fc, nil
	}
	_ = fc.close()
	if ack.Reason == "" {
		ack.Reason = "worker 操作或身份与请求不符"
	}
	return nil, fmt.Errorf("接入会话 %s 失败: %s", id, ack.Reason)
}
