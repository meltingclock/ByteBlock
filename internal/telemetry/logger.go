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

	logCh chan logEntry
	once  sync.Once

	// Bounded ring buffer for /tail - prevents memory leaks
	rbMu      sync.Mutex
	rbData    []logEntry
	rbNext    int
	rbSize    = 2000 // keep last 2k lines
	rbWrapped bool   // track if we've wrapped around
)

type logEntry struct {
	timestamp time.Time
	level     string
	message   string
}

func Start() {
	once.Do(func() {
		log.SetOutput(os.Stdout) // keep default; just to be explicit
		logCh = make(chan logEntry, 8192)
		rbData = make([]logEntry, rbSize)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("telemetry panic: %v\n", r)
				}
			}()

			for entry := range logCh {
				// append to ring
				rbMu.Lock()
				rbData[rbNext] = entry
				rbNext = (rbNext + 1) % rbSize
				if rbNext == 0 {
					rbWrapped = true
				}
				rbMu.Unlock()

				// Write to stdout
				fmt.Printf("%s [%s] %s\n",
					entry.timestamp.Format("2006/01/02 15:04:05.000"),
					entry.level,
					entry.message)
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
func enqueue(level, message string) {
	entry := logEntry{
		timestamp: time.Now(),
		level:     level,
		message:   message,
	}
	select {
	case logCh <- entry:
	default:
		// Channel full - log to stderr and drop
		fmt.Fprintf(os.Stderr, "telemetry: buffer full, dropping log: %s\n", message)
	}
}

// INFO is always on (use sparingly on hot path).
func Infof(format string, args ...any) {
	enqueue("INFO", fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	enqueue("WARN", fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	enqueue("ERROR", fmt.Sprintf(format, args...))
}

// DEBUG only formats if enabled (zero cost when off).
func Debugf(format string, args ...any) {
	if !enableDebug.Load() {
		return
	}
	enqueue("DEBUG", fmt.Sprintf(format, args...))
}

// TRACE is for very noisy spots; off by deafault.
func Tracef(format string, args ...any) {
	if !enableTrace.Load() {
		return
	}
	enqueue("TRACE", fmt.Sprintf(format, args...))
}

// Improved Tail with proper bounds checking
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

	// Determine actual number of entries available
	available := rbSize
	if !rbWrapped {
		available = rbNext
	}
	if available == 0 {
		return nil
	}

	if n > available {
		n = available
	}

	// Start from the most recent entry and go backwards
	start := rbNext - 1
	if start < 0 {
		start = rbSize - 1
	}

	for i := 0; i < n; i++ {
		idx := (start - i + rbNext) % rbSize
		entry := rbData[idx]
		if !entry.timestamp.IsZero() {
			formatted := fmt.Sprintf("%s [%s] %s",
				entry.timestamp.Format("15:04:05.000"),
				entry.level,
				entry.message)
			out = append(out, formatted)
		}
	}
	// reverse to chronological
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
