package telemetry

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	enableDebug atomic.Bool
	enableTrace atomic.Bool

	logCh chan string
	once  sync.Once

	// ring buffer for /tail
	rbMu   sync.Mutex
	rbData []string
	rbNext int
	rbSize = 2000 // keep last 2k lines
)

func Start() {
	once.Do(func() {
		log.SetOutput(os.Stdout) // keep default; just to be explicit
		logCh = make(chan string, 8192)

		rbData = make([]string, rbSize)

		go func() {
			for line := range logCh {
				// append to ring
				rbMu.Lock()
				rbData[rbNext] = line
				rbNext = (rbNext + 1) % rbSize
				rbMu.Unlock()

				// write to stdout
				fmt.Println(line)
			}
		}()
	})
}

func Stop() {
	if logCh != nil {
		close(logCh)
	}
}

func EnableDebug(on bool) { enableDebug.Store(on) }
func DebugOn() bool       { return enableDebug.Load() }

func EnableTrace(on bool) { enableTrace.Store(on) }
func TraceOn() bool       { return enableTrace.Load() }

// Non-blocking enqueue; drop if saturated.
func enqueue(line string) {
	select {
	case logCh <- line:
	default:
		// drop to avoid blocking hot path
	}
}

// INFO is always on (use sparingly on hot path).
func Infof(format string, args ...any) {
	enqueue(ts() + " [INFO] " + fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	enqueue(ts() + " [WARN] " + fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	enqueue(ts() + " [ERROR] " + fmt.Sprintf(format, args...))
}

// DEBUG only formats if enabled (zero cost when off).
func Debugf(format string, args ...any) {
	if !enableDebug.Load() {
		return
	}
	enqueue(ts() + " [DEBUG] " + fmt.Sprintf(format, args...))
}

// TRACE is for very noisy spots; off by deafault.
func Tracef(format string, args ...any) {
	if !enableTrace.Load() {
		return
	}
	enqueue(ts() + " [TRACE] " + fmt.Sprintf(format, args...))
}

func ts() string { return time.Now().Format("2006/01/02 15:04:05.000") }

// Tail returns the last n lines (up to buffer size).
func Tail(n int) []string {
	if n <= 0 {
		return nil
	}
	if n > rbSize {
		n = rbSize
	}
	rbMu.Lock()
	defer rbMu.Unlock()
	out := make([]string, 0, n)
	// walk ring starting from latest-1 backwards
	idx := (rbNext - 1 + rbSize) % rbSize
	for i := 0; i < n; i++ {
		line := rbData[idx]
		if line != "" {
			out = append(out, line)
		}
		if idx == 0 {
			idx = rbSize - 1
		} else {
			idx--
		}
	}
	// reverse to chronological
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
