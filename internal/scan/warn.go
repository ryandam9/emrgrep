package scan

import (
	"fmt"
	"io"
	"sync"
)

// Warner writes bounded diagnostics to stderr (M-08). After maxWarnings
// messages, further warnings are counted but suppressed; the summary
// reports how many were dropped.
type Warner struct {
	mu       sync.Mutex
	w        io.Writer
	max      int64
	emitted  int64
	counters *Counters
}

// NewWarner bounds warning output at max messages (0 = unlimited).
func NewWarner(w io.Writer, max int, counters *Counters) *Warner {
	return &Warner{w: w, max: int64(max), counters: counters}
}

// Logf emits one informational line (progress, verbose events). It
// shares the mutex with Warnf so lines never interleave, but does not
// count against -max-warnings: suppression exists to bound error
// noise, not requested status output.
func (wr *Warner) Logf(format string, args ...interface{}) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	fmt.Fprintf(wr.w, "emrgrep: "+format+"\n", args...)
}

// Warnf emits one warning, subject to the cap.
func (wr *Warner) Warnf(format string, args ...interface{}) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.max > 0 && wr.emitted >= wr.max {
		wr.counters.WarningsSuppressed.Add(1)
		return
	}
	wr.emitted++
	fmt.Fprintf(wr.w, "emrgrep: warning: "+format+"\n", args...)
}

// Flush prints the suppression trailer if any warnings were dropped.
func (wr *Warner) Flush() {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if n := wr.counters.WarningsSuppressed.Load(); n > 0 {
		fmt.Fprintf(wr.w, "emrgrep: %d further warnings were suppressed by -max-warnings (all causes are still counted in the summary)\n", n)
	}
}
