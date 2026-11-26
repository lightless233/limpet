package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
)

const terminalReset = "\x18\x1b[=0u\x1b[?1049l\x1b[=0u" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" +
	"\x1b[?2004l\x1b[?1004l\x1b[?25h\x1b[0m"

type cliTerminal struct {
	in, out       *os.File
	input, output syscall.Termios
	shared        bool
}

func terminalIOCTL(file *os.File, op uintptr, value *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), op, uintptr(unsafe.Pointer(value)))
	if errno != 0 {
		return errno
	}
	return nil
}

func openCLITerminal() (*cliTerminal, error) {
	t := new(cliTerminal)
	if err := terminalIOCTL(os.Stdin, syscall.TCGETS, &t.input); err != nil {
		return nil, fmt.Errorf("标准输入需要交互式终端: %w", err)
	}
	if err := terminalIOCTL(os.Stdout, syscall.TCGETS, &t.output); err != nil {
		return nil, fmt.Errorf("标准输出需要交互式终端: %w", err)
	}
	// Reopen the TTYs with separate nonblocking open descriptions. Setting
	// O_NONBLOCK on inherited stdin/stdout would also affect the parent shell.
	var err error
	t.in, err = os.OpenFile("/proc/self/fd/0", os.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("打开终端输入: %w", err)
	}
	t.out, err = os.OpenFile("/proc/self/fd/1", os.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		_ = t.in.Close()
		return nil, fmt.Errorf("打开终端输出: %w", err)
	}
	in, inErr := t.in.Stat()
	out, outErr := t.out.Stat()
	if err := errors.Join(inErr, outErr, t.in.SetReadDeadline(time.Time{}), t.out.SetWriteDeadline(time.Time{})); err != nil {
		t.close()
		return nil, fmt.Errorf("初始化终端 I/O: %w", err)
	}
	t.shared = os.SameFile(in, out)
	return t, nil
}

func (t *cliTerminal) close() {
	_ = t.in.Close()
	_ = t.out.Close()
}

func (t *cliTerminal) dims() (viewerDims, error) {
	size, err := pty.GetsizeFull(os.Stdout)
	if err != nil {
		return viewerDims{}, fmt.Errorf("读取终端尺寸: %w", err)
	}
	dims := viewerDims{Cols: size.Cols, Rows: size.Rows}
	if !dims.valid() {
		return dims, fmt.Errorf("终端尺寸超出支持范围: %d×%d", dims.Cols, dims.Rows)
	}
	return dims, nil
}

func rawTermios(state syscall.Termios) syscall.Termios {
	state.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	state.Oflag &^= syscall.OPOST
	state.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	state.Cflag &^= syscall.CSIZE | syscall.PARENB
	state.Cflag |= syscall.CS8
	state.Cc[syscall.VMIN], state.Cc[syscall.VTIME] = 1, 0
	return state
}

func terminalHungUp(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENOTTY)
}

type cliViewerResult struct {
	code  int
	reset bool
	err   error
}

func (t *cliTerminal) run(replay bool, connect func(context.Context) (*frameConn, error),
	register func(context.Context) error) (code int, err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	raw := rawTermios(t.input)
	if err := terminalIOCTL(os.Stdin, syscall.TCSETS, &raw); err != nil {
		return 1, fmt.Errorf("设置终端 raw mode: %w", err)
	}
	restore := func(file *os.File, state *syscall.Termios) {
		if restoreErr := terminalIOCTL(file, syscall.TCSETS, state); restoreErr != nil && !terminalHungUp(restoreErr) {
			err = errors.Join(err, fmt.Errorf("恢复终端设置: %w", restoreErr))
		}
	}
	defer restore(os.Stdin, &t.input)
	if !t.shared {
		rawOut := t.output
		rawOut.Oflag &^= syscall.OPOST
		if err := terminalIOCTL(os.Stdout, syscall.TCSETS, &rawOut); err != nil {
			return 1, fmt.Errorf("设置终端输出模式: %w", err)
		}
		defer restore(os.Stdout, &t.output)
	}
	reset := false
	defer func() {
		if reset {
			_ = t.out.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
			if _, resetErr := t.out.WriteString(terminalReset); resetErr != nil && !terminalHungUp(resetErr) {
				err = errors.Join(err, fmt.Errorf("复位终端显示: %w", resetErr))
			}
		}
	}()
	if ctx.Err() != nil {
		reset = true
		return 0, nil
	}
	fc, err := connect(ctx)
	if ctx.Err() != nil {
		if fc != nil {
			_ = fc.close()
		}
		reset = true
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	defer fc.close()
	if register != nil {
		// Registration is best effort. The server can discover the worker
		// later, and a short command may already have exited.
		go register(ctx)
	}

	ready := make(chan struct{})
	results := make(chan cliViewerResult, 3)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		results <- t.readOutput(fc, replay, ready)
	}()
	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		sendSize := func() error {
			dims, err := t.dims()
			if err != nil {
				return err
			}
			return fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: dims.Cols, Rows: dims.Rows}))
		}
		if err := sendSize(); err != nil && !fc.closed.Load() {
			// The command may have already exited and sent its final
			// frames. The read side owns the protocol result in that case.
			if replay && !errors.Is(err, syscall.EPIPE) {
				results <- cliViewerResult{code: 1, reset: true, err: err}
				return
			}
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-winch:
					if dims, err := t.dims(); err == nil {
						if fc.writeFrame(ftResize, mustJSON(resizeMsg{Cols: dims.Cols, Rows: dims.Rows})) != nil {
							return
						}
					}
				}
			}
		}()
		buf := make([]byte, wsChunk)
		for {
			n, err := t.in.Read(buf)
			if n > 0 {
				if writeErr := fc.writeFrame(ftInput, buf[:n]); writeErr != nil {
					// Drain a possible final ftExit even if the peer closed
					// its write side while the user was typing.
					_ = fc.c.SetReadDeadline(time.Now().Add(frameBudget))
					return
				}
			}
			if err != nil {
				if ctx.Err() == nil {
					results <- cliViewerResult{reset: true}
				}
				return
			}
		}
	}()

	var result cliViewerResult
	select {
	case result = <-results:
	case <-ctx.Done():
		result.reset = true
	}
	cancel()
	_ = fc.close()
	_ = t.in.Close()
	_ = t.out.SetWriteDeadline(time.Now())
	<-outDone
	<-inDone
	reset = result.reset
	return result.code, result.err
}

func (t *cliTerminal) readOutput(fc *frameConn, replay bool, ready chan<- struct{}) cliViewerResult {
	started := false
	write := func(p []byte) error {
		_ = t.out.SetWriteDeadline(time.Now().Add(frameBudget))
		_, err := t.out.Write(p)
		return err
	}
	for {
		kind, payload, err := fc.readFrame()
		if err != nil {
			return cliViewerResult{1, true, fmt.Errorf("worker 连接中断: %w", err)}
		}
		switch kind {
		case ftReady:
			if !started {
				if replay {
					if err := write([]byte("\x1b[2J\x1b[H")); err != nil {
						return cliViewerResult{1, true, fmt.Errorf("初始化终端画面: %w", err)}
					}
				}
				started = true
				close(ready)
			}
		case ftOutput:
			if err := write(payload); err != nil {
				return cliViewerResult{1, true, fmt.Errorf("写入本地终端: %w", err)}
			}
		case ftDetach:
			var msg detachMsg
			_ = jsonUnmarshal(payload, &msg)
			return cliViewerResult{1, true, fmt.Errorf("已脱离会话（%d %s）", msg.Code, msg.Reason)}
		case ftExit:
			var msg exitMsg
			if err := jsonUnmarshal(payload, &msg); err != nil || msg.ExitCode == nil {
				return cliViewerResult{1, true, fmt.Errorf("会话结束，无法取得目标进程退出码")}
			}
			code := *msg.ExitCode
			return cliViewerResult{code: code, reset: code >= 128 || (msg.Reason != "shell-exit" && msg.Reason != "command-exit")}
		}
	}
}
