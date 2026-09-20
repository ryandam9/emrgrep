package scan

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingWriter releases one write at a time, so a test can hold the
// writer goroutine still and observe what the queue does behind it.
type blockingWriter struct {
	gate chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.gate
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *blockingWriter) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Queue depth alone bounds the queue by COUNT; a queued line can be as
// large as -max-line-size, so without a byte budget the writer's
// memory is workers × depth × max-line-size. Emit must block once the
// budget is full rather than accepting every result a fast scan can
// produce.
func TestEmitBlocksOnByteBudget(t *testing.T) {
	bw := &blockingWriter{gate: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Room for two 100-byte lines; the queue can hold far more by count.
	w := NewWriter(bw, WriterConfig{QueueDepth: 64, QueueBytes: 250}, cancel)

	line := bytes.Repeat([]byte("x"), 100)
	emit := func() bool {
		return w.Emit(ctx, Result{Bucket: "b", Key: "k", LineNo: 1, Line: append([]byte(nil), line...)})
	}
	// The writer goroutine takes the first result off the channel and
	// blocks inside Write, so it is holding one result of its own.
	if !emit() {
		t.Fatal("first emit rejected")
	}

	accepted := make(chan int, 1)
	go func() {
		n := 0
		for i := 0; i < 20; i++ {
			if !emit() {
				break
			}
			n++
		}
		accepted <- n
	}()

	select {
	case n := <-accepted:
		t.Fatalf("emit accepted %d results without the writer draining; the byte budget is not holding", n)
	case <-time.After(150 * time.Millisecond):
		// Blocked, as it must be.
	}

	// Let the writer drain; every queued result now flows through.
	close(bw.gate)
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("emitters never unblocked after the writer drained")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if bw.len() == 0 {
		t.Fatal("nothing was written")
	}
}

// The budget must never wedge the run: a line larger than the whole
// budget is admitted to an empty queue rather than waiting for room
// that can never appear.
func TestEmitAdmitsLineLargerThanBudget(t *testing.T) {
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 4, QueueBytes: 16}, cancel)

	huge := bytes.Repeat([]byte("y"), 4096)
	done := make(chan bool, 1)
	go func() {
		done <- w.Emit(ctx, Result{Bucket: "b", Key: "k", LineNo: 1, Line: huge})
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("oversized line was rejected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an empty queue must admit a line larger than the budget")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), strings.Repeat("y", 4096)) {
		t.Fatal("oversized line not written")
	}
}

// Key-only results carry no line bytes, so they never consume budget
// and a full queue can never stop list-only mode from reporting.
func TestKeyOnlyResultsBypassBudget(t *testing.T) {
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 64, QueueBytes: 1}, cancel)
	for i := 0; i < 50; i++ {
		if !w.Emit(ctx, Result{Bucket: "b", Key: "k", KeyOnly: true}) {
			t.Fatalf("key-only emit %d rejected", i)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "s3://b/k"); got != 50 {
		t.Fatalf("wrote %d keys, want 50", got)
	}
}

// Cancellation must release a blocked emitter, not strand it. The
// emitter is driven until the budget actually stalls it — the writer
// frees bytes as soon as it has copied a result, so a fixed number of
// emits is not reliably enough to fill the queue.
func TestEmitUnblocksOnCancel(t *testing.T) {
	bw := &blockingWriter{gate: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWriter(bw, WriterConfig{QueueDepth: 2, QueueBytes: 50}, cancel)

	line := bytes.Repeat([]byte("z"), 100)
	rejected := make(chan bool, 1)
	go func() {
		for {
			if !w.Emit(ctx, Result{Bucket: "b", Key: "k", LineNo: 1, Line: line}) {
				rejected <- true
				return
			}
		}
	}()

	// The writer is stuck in its first Flush, so the queue fills and
	// the emitter blocks; nothing should come back yet.
	select {
	case <-rejected:
		t.Fatal("emit returned before cancellation")
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	select {
	case <-rejected:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not release the blocked emitter")
	}
	close(bw.gate)
	_ = w.Close()
}
