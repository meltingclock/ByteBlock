// Create this file: internal/telemetry/benchmark_test.go

package telemetry

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Test high-volume logging (simulates mempool burst)
func TestHighVolumeLogging(t *testing.T) {
	Start()
	defer Stop()

	// Record initial memory
	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	// Simulate high-volume logging from multiple goroutines
	const numGoroutines = 10
	const logsPerGoroutine = 1000

	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < logsPerGoroutine; j++ {
				Infof("Goroutine %d: Processing transaction %d hash=0x%x", id, j, j*id)
				Debugf("Goroutine %d: Debug info for tx %d", id, j)
				if j%100 == 0 {
					Warnf("Goroutine %d: Checkpoint at %d", id, j)
				}
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Give async processing time to complete
	time.Sleep(100 * time.Millisecond)

	// Check final memory
	var m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m2)

	totalLogs := numGoroutines * logsPerGoroutine
	logsPerSecond := float64(totalLogs) / elapsed.Seconds()
	memoryUsed := m2.Alloc - m1.Alloc

	fmt.Printf("Performance Results:\n")
	fmt.Printf("- Processed %d log entries in %v\n", totalLogs, elapsed)
	fmt.Printf("- Throughput: %.0f logs/second\n", logsPerSecond)
	fmt.Printf("- Memory used: %d bytes (%.2f KB)\n", memoryUsed, float64(memoryUsed)/1024)
	fmt.Printf("- Memory per log: %.2f bytes\n", float64(memoryUsed)/float64(totalLogs))

	// Verify tail functionality works
	tail := Tail(10)
	if len(tail) != 10 {
		t.Errorf("Expected 10 tail entries, got %d", len(tail))
	}

	fmt.Printf("- Tail test: ✅ Retrieved %d recent entries\n", len(tail))

	// Performance expectations
	if logsPerSecond < 10000 {
		t.Errorf("Performance too slow: %.0f logs/second (expected >10k)", logsPerSecond)
	}

	if memoryUsed > 1024*1024 { // 1MB limit
		t.Errorf("Memory usage too high: %d bytes (expected <1MB)", memoryUsed)
	}

	fmt.Printf("✅ All performance tests passed!\n")
}

// Test debug/trace toggle performance
func TestDebugTogglePerformance(t *testing.T) {
	Start()
	defer Stop()

	// Test with debug OFF - should be very fast
	EnableDebug(false)
	start := time.Now()

	for i := 0; i < 100000; i++ {
		Debugf("This debug message should not be formatted: %d %s", i, "expensive formatting")
	}

	elapsedOff := time.Since(start)

	// Test with debug ON - will be slower due to formatting
	EnableDebug(true)
	start = time.Now()

	for i := 0; i < 100000; i++ {
		Debugf("This debug message will be formatted: %d %s", i, "expensive formatting")
	}

	elapsedOn := time.Since(start)

	fmt.Printf("Debug Toggle Performance:\n")
	fmt.Printf("- Debug OFF: %v (%.0f ops/sec)\n", elapsedOff, 100000/elapsedOff.Seconds())
	fmt.Printf("- Debug ON:  %v (%.0f ops/sec)\n", elapsedOn, 100000/elapsedOn.Seconds())
	fmt.Printf("- Speedup when disabled: %.1fx\n", elapsedOn.Seconds()/elapsedOff.Seconds())

	// Debug OFF should be much faster (no string formatting)
	if elapsedOff > elapsedOn/10 {
		t.Errorf("Debug toggle not effective enough")
	}

	fmt.Printf("✅ Debug toggle performance test passed!\n")
}

// Benchmark concurrent tail operations (Telegram users requesting logs)
func BenchmarkTailConcurrent(b *testing.B) {
	Start()
	defer Stop()

	// Fill buffer with test data
	for i := 0; i < 2000; i++ {
		Infof("Test log entry %d", i)
	}

	time.Sleep(10 * time.Millisecond) // Let async processing complete

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tail := Tail(50) // Typical /tail request
			if len(tail) == 0 {
				b.Errorf("Tail returned no entries")
			}
		}
	})
}
