package main

import (
	"io"
	"sync"
	"testing"
	"time"
)

// stubBody blocks forever on Read, like a stalled upstream stream.
type stubBody struct {
	done chan struct{}
}

func (s *stubBody) Read(p []byte) (int, error) {
	<-s.done
	return 0, io.ErrClosedPipe
}
func (s *stubBody) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

func TestIdleTimeoutReaderFires(t *testing.T) {
	body := &stubBody{done: make(chan struct{})}
	it := newIdleTimeoutReader(body, 200*time.Millisecond)
	start := time.Now()
	buf := make([]byte, 16)
	_, err := it.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected read error after idle timeout")
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("unexpected elapsed %v", elapsed)
	}
	_ = it.Close()
}

// fastBody returns data immediately and never trips the timer.
type fastBody struct {
	n int
}

func (f *fastBody) Read(p []byte) (int, error) {
	if f.n > 0 {
		f.n--
		p[0] = 'x'
		return 1, nil
	}
	return 0, io.EOF
}
func (f *fastBody) Close() error { return nil }

func TestIdleTimeoutReaderNoFalsePositive(t *testing.T) {
	it := newIdleTimeoutReader(&fastBody{n: 3}, 300*time.Millisecond)
	buf := make([]byte, 4)
	for i := 0; i < 3; i++ {
		if _, err := it.Read(buf); err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = it.Close()
}

// TestIdleTimeoutReaderConcurrentClose exercises the two real Close paths
// (handler defer + client-gone AfterFunc) racing with an in-flight Read.
// Run with -race to verify the atomic state is race-free.
func TestIdleTimeoutReaderConcurrentClose(t *testing.T) {
	body := &blockingBody{done: make(chan struct{})}
	it := newIdleTimeoutReader(body, time.Hour) // timer must not interfere
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8)
		_, _ = it.Read(buf)
	}()
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = it.Close()
		}()
	}
	wg.Wait()
}

// blockingBody blocks on Read until Close, mimicking a stalled upstream.
type blockingBody struct {
	done chan struct{}
	once sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	<-b.done
	return 0, io.ErrClosedPipe
}
func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}
