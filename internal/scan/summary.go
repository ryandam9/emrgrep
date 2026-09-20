package scan

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Summary tints (stderr): neutral counts cyan, good outcomes green,
// cautionary counts yellow, failures red; zero values are dimmed so
// the numbers that actually moved stand out.
const (
	sumCyan   = "\x1b[36m"
	sumGreen  = "\x1b[32m"
	sumYellow = "\x1b[33m"
	sumRed    = "\x1b[31m"
	sumDim    = "\x1b[2m"
)

// PrintSummary writes the end-of-run account to w (stderr). It always
// prints — including after SIGINT — and separates objects scanned to
// EOF, stopped early by request, partially scanned, skipped (with
// reasons), and failed (§10, M-06). With color enabled (stderr is a
// terminal), the status line and every count are tinted.
func PrintSummary(w io.Writer, r *RunResult, listOnly, color bool) {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + ansiReset
	}
	// num colors a count; zeros are dimmed regardless of the tint so
	// non-zero values stand out.
	num := func(code string, n int64) string {
		s := strconv.FormatInt(n, 10)
		if !color {
			return s
		}
		if n == 0 {
			return sumDim + s + ansiReset
		}
		return code + s + ansiReset
	}
	c := r.Counters
	fmt.Fprintln(w, "---")
	status := paint(sumGreen, "completed")
	switch {
	case r.MatchLimitHit && !r.Interrupted && r.WriteErr == nil && r.ListingErr == nil:
		status = paint(sumGreen, "completed: -max-total-matches reached")
	case r.Interrupted:
		status = paint(sumYellow, "interrupted")
	case r.TimedOut:
		status = paint(sumYellow, "stopped: -overall-timeout exceeded")
	case r.WriteErr != nil:
		status = paint(sumRed, fmt.Sprintf("stopped: stdout write failed (%v)", r.WriteErr))
	case r.ListingErr != nil:
		status = paint(sumRed, "failed while listing")
	}
	fmt.Fprintf(w, "emrgrep: %s in %s\n", status, r.Elapsed.Round(1e6))

	fmt.Fprintf(w, "  listed %s, survived filters %s\n",
		num(sumCyan, c.Listed.Load()), num(sumCyan, c.Survived.Load()))

	var skips []string
	appendSkip := func(n int64, label string) {
		if n > 0 {
			skips = append(skips, fmt.Sprintf("%s %s", num(sumYellow, n), label))
		}
	}
	appendSkip(c.FoldersSkipped.Load(), "folder markers")
	appendSkip(c.ArchivedSkipped.Load(), "archived without restored copy")
	appendSkip(c.OversizeSkipped.Load(), "over -max-size")
	appendSkip(c.TimeFiltered.Load(), "outside time window")
	appendSkip(c.ExtFiltered.Load(), "extension filtered")
	appendSkip(c.KeyFiltered.Load(), "key-pattern filtered")
	if len(skips) > 0 {
		fmt.Fprintf(w, "  filtered out: %s\n", strings.Join(skips, ", "))
	}

	if listOnly {
		fmt.Fprintf(w, "  list-only mode: %s objects reported, no downloads\n", num(sumGreen, c.MatchedObjects.Load()))
	} else {
		fmt.Fprintf(w, "  scanned to EOF %s, stopped early by request %s, partially scanned %s\n",
			num(sumGreen, c.ScannedFully.Load()), num(sumCyan, c.StoppedEarly.Load()), num(sumYellow, c.ScannedPartially.Load()))
		fmt.Fprintf(w, "  matched objects %s, matched lines %s\n",
			num(sumGreen, c.MatchedObjects.Load()), num(sumGreen, c.MatchedLines.Load()))
		dl := c.BytesDownloaded.Load()
		fmt.Fprintf(w, "  downloaded %s (%s compressed bytes)\n",
			paint(sumCyan, humanBytes(dl)), num(sumCyan, dl))
		if n := c.OversizedLines.Load(); n > 0 {
			fmt.Fprintf(w, "  oversized lines truncated %s\n", num(sumYellow, n))
		}
	}

	var errs []string
	appendErr := func(n int64, label string) {
		if n > 0 {
			errs = append(errs, fmt.Sprintf("%s %d", label, n))
		}
	}
	appendErr(c.AccessDenied.Load(), "accessDenied")
	appendErr(c.NotFound.Load(), "notFound")
	appendErr(c.ChangedAfterListing.Load(), "changedAfterListing")
	appendErr(c.Corrupt.Load(), "corrupt")
	appendErr(c.Timeout.Load(), "timeout")
	appendErr(c.ArchivedUnavailable.Load(), "archivedUnavailable")
	appendErr(c.Throttled.Load(), "throttled")
	appendErr(c.OtherErrors.Load(), "other")
	if len(errs) > 0 {
		fmt.Fprintf(w, "  %s\n", paint("\x1b[31m", fmt.Sprintf("object errors: %s", strings.Join(errs, ", "))))
	}

	if ids := r.AppIDs.Sorted(); len(ids) > 0 {
		fmt.Fprintf(w, "  application IDs discovered (%s):\n", num(sumGreen, int64(len(ids))))
		for _, id := range ids {
			fmt.Fprintf(w, "    %s\n", paint(ansiZip, id))
		}
	}
}

// ExitCode maps the run result to the stable exit codes (§10, H-08):
//
//	0    completed; ≥1 match; no object errors or partial scans
//	1    completed; no matches
//	2    fatal usage, configuration, credential, or listing error
//	3    completed with ≥1 object error or partially scanned object,
//	     the overall timeout expired, or stdout failed mid-run
//	130  interrupted (SIGINT/SIGTERM); summary is still printed
//
// -overall-timeout deliberately maps to 3, not 130: a configured
// deadline is a partial run, not an external interruption (H-02).
func ExitCode(r *RunResult) int {
	switch {
	case r.ListingErr != nil:
		return 2
	case r.Interrupted:
		return 130
	case r.TimedOut:
		return 3
	case r.WriteErr != nil:
		return 3
	case r.Counters.ObjectErrors() > 0 || r.Counters.ScannedPartially.Load() > 0:
		return 3
	case r.Counters.MatchedLines.Load() > 0 || r.Counters.MatchedObjects.Load() > 0:
		return 0
	default:
		return 1
	}
}
