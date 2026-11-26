package main

import (
	"fmt"
	"log"
	"os"
	"time"
)

const (
	recordingQueueSize = 64 // 每批最多 64 KiB
	cwdSampleThrottle  = 200 * time.Millisecond
)

type recordingEvent struct {
	data []byte
	at   int64
	dims viewerDims
	gap  int64
}

// recording 的提交与关闭由 gridMu 串行；文件、偏移和 cwd 由写协程独占。
type recording struct {
	queue      chan recordingEvent
	done       chan struct{}
	pendingGap int64
	finalDims  viewerDims
}

func (r *recording) enqueue(dims viewerDims, data []byte, at int64) {
	r.finalDims = dims
	select {
	case r.queue <- recordingEvent{data: data, at: at, dims: dims, gap: r.pendingGap}:
		r.pendingGap = 0
	default:
		r.pendingGap += int64(len(data))
	}
}

func (r *recording) write(raw, meta *os.File, at time.Time, dims viewerDims, cwd, cwdPath string) {
	defer close(r.done)
	defer raw.Close()
	defer meta.Close()

	var offset int64
	var logBroken, metaBroken bool
	writeMeta := func(kind string, pos int64, value any) {
		if !metaBroken {
			if _, err := fmt.Fprintf(meta, "%s\t%d\t%v\n", kind, pos, value); err != nil {
				log.Printf("写入录制事件 %s 失败: %v", meta.Name(), err)
				metaBroken = true
			}
		}
	}
	writeMeta("T", 0, at.UnixMilli())
	writeMeta("S", 0, fmt.Sprintf("%dx%d", dims.Cols, dims.Rows))
	if cwd != "" {
		writeMeta("C", 0, cwd)
	}
	var lastCwdAt time.Time
	writeEvent := func(event recordingEvent) {
		if logBroken {
			if dropped := event.gap + int64(len(event.data)); dropped > 0 {
				writeMeta("G", offset, dropped)
			}
			return
		}
		if event.gap > 0 {
			writeMeta("G", offset, event.gap)
		}
		if event.dims != dims {
			dims = event.dims
			writeMeta("S", offset, fmt.Sprintf("%dx%d", dims.Cols, dims.Rows))
		}
		if len(event.data) == 0 {
			return
		}
		start := offset
		n, err := raw.Write(event.data)
		offset += int64(n)
		if n > 0 {
			writeMeta("T", start, event.at)
		}
		if err != nil {
			log.Printf("写入录制 %s 失败: %v", raw.Name(), err)
			logBroken = true
			if n < len(event.data) {
				writeMeta("G", offset, len(event.data)-n)
			}
			return
		}
		// Shell 切换目录时通常会输出新提示符，因此随输出节流采样。
		if time.Since(lastCwdAt) >= cwdSampleThrottle {
			lastCwdAt = time.Now()
			if path, err := os.Readlink(cwdPath); err == nil && path != cwd {
				cwd = path
				writeMeta("C", offset, cwd)
			}
		}
	}
	for event := range r.queue {
		writeEvent(event)
	}
	writeEvent(recordingEvent{dims: r.finalDims, gap: r.pendingGap})
}
