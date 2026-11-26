package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestCLITerminalExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exit  string
		code  int
		reset bool
	}{
		{"normal", `{"reason":"command-exit","exitCode":42}`, 42, false},
		{"target-signal", `{"reason":"command-exit","exitCode":143}`, 143, true},
		{"remote-close", `{"reason":"killed","exitCode":0}`, 0, true},
		{"legacy", `{"reason":"shell-exit"}`, 1, true},
		{"hangup", "", 0, true},
		{"startup-hangup", "", 0, true},
		{"blocked-output", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "limpet-terminal-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "tty.sock")
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			_ = l.SetDeadline(time.Now().Add(5 * time.Second))
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			if err := pty.Setsize(master, &pty.Winsize{Cols: 100, Rows: 30}); err != nil {
				t.Fatal(err)
			}
			var original syscall.Termios
			if err := terminalIOCTL(slave, syscall.TCGETS, &original); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLITerminalChild$")
			cmd.Env = append(os.Environ(), "LIMPET_TEST_TTY_SOCKET="+path)
			cmd.Stdin, cmd.Stdout = slave, slave
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			c, err := l.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			var raw syscall.Termios
			if err := terminalIOCTL(slave, syscall.TCGETS, &raw); err != nil || raw.Lflag&syscall.ICANON != 0 {
				t.Fatalf("客户端在握手前未进入 raw mode: %v", err)
			}

			if tc.name != "startup-hangup" {
				if err := writeFrame(c, ftReady, nil); err != nil {
					t.Fatal(err)
				}
				if kind, _, err := readFrame(c); err != nil || kind != ftResize {
					t.Fatalf("接回未先申报尺寸: %v, %v", kind, err)
				}
			}
			var output bytes.Buffer
			if tc.name == "blocked-output" {
				go func() {
					_ = writeFrame(c, ftOutput, bytes.Repeat([]byte{'x'}, 1<<20))
				}()
				// Let a large frame fill the PTY before delivering the signal.
				time.Sleep(100 * time.Millisecond)
			}
			if tc.exit == "" {
				if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := writeFrame(c, ftOutput, []byte("tail")); err != nil {
					t.Fatal(err)
				}
				if err := writeFrame(c, ftExit, []byte(tc.exit)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name != "blocked-output" {
				if err := master.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, 256)
				want := "tail"
				if tc.reset {
					want = "\x1b[=0u\x1b[?1049l\x1b[=0u"
				}
				for !strings.Contains(output.String(), want) {
					n, err := master.Read(buf)
					output.Write(buf[:n])
					if err != nil {
						t.Fatalf("未收到 %q，输出 %q: %v", want, output.String(), err)
					}
				}
			}
			err = <-exited
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("退出码 = %d，期待 %d，stderr=%s", code, tc.code, stderr.String())
			}
			var restored syscall.Termios
			if err := terminalIOCTL(slave, syscall.TCGETS, &restored); err != nil || restored != original {
				t.Fatalf("termios 未恢复: %+v，%v", restored, err)
			}
		})
	}
}

func TestCLITerminalChild(t *testing.T) {
	path := os.Getenv("LIMPET_TEST_TTY_SOCKET")
	if path == "" {
		return
	}
	tty, err := openCLITerminal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code, err := tty.run(true, func(ctx context.Context) (*frameConn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		return newFrameConn(c), nil
	}, nil)
	tty.close()
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
