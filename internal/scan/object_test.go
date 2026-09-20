package scan

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"
)

func gzipBytes(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Sorted iteration is not needed; map order is fine for tests that
	// don't depend on entry order.
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type scanRun struct {
	outcome  ObjectOutcome
	output   string
	counters *Counters
}

func runObjectScan(t *testing.T, key string, body []byte, opts ScanOptions) scanRun {
	t.Helper()
	if opts.MaxLineSize == 0 {
		opts.MaxLineSize = 1 << 20
	}
	if opts.MaxZipEntries == 0 {
		opts.MaxZipEntries = 10000
	}
	if opts.MaxZipExpandedBytes == 0 {
		opts.MaxZipExpandedBytes = 512 << 20
	}
	opts.TempDir = t.TempDir()

	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWriter(&out, WriterConfig{QueueDepth: 64, Sanitize: true}, cancel)
	var c Counters
	desc := &ObjectDescriptor{Key: key, Size: int64(len(body))}
	outcome := ScanObject(ctx, "test-bucket", desc, bytes.NewReader(body), DetectFormat(key), &opts, w, &c)
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}
	return scanRun{outcome: outcome, output: out.String(), counters: &c}
}

func grepOpts(t *testing.T, pattern string) ScanOptions {
	t.Helper()
	m, err := NewMatcher(pattern, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return ScanOptions{Grep: m}
}

func TestScanPlainText(t *testing.T) {
	r := runObjectScan(t, "logs/app.log", []byte("ok line\nERROR bad thing\nok again\n"), grepOpts(t, "ERROR"))
	if r.outcome.Matches != 1 || r.outcome.Partial || r.outcome.Err != nil {
		t.Fatalf("outcome: %+v", r.outcome)
	}
	want := "s3://test-bucket/logs/app.log:2: ERROR bad thing\n"
	if r.output != want {
		t.Fatalf("output:\n%q\nwant:\n%q", r.output, want)
	}
}

func TestScanGzip(t *testing.T) {
	body := gzipBytes(t, "a\nERROR x\n")
	r := runObjectScan(t, "logs/syslog.gz", body, grepOpts(t, "ERROR"))
	if r.outcome.Matches != 1 || r.outcome.Err != nil {
		t.Fatalf("outcome: %+v", r.outcome)
	}
}

// Rotated logs concatenate gzip members; multistream must read all.
func TestScanGzipMultistream(t *testing.T) {
	body := append(gzipBytes(t, "first ERROR\n"), gzipBytes(t, "second ERROR\n")...)
	r := runObjectScan(t, "logs/syslog.gz", body, grepOpts(t, "ERROR"))
	if r.outcome.Matches != 2 {
		t.Fatalf("multistream members not all scanned: %+v\n%s", r.outcome, r.output)
	}
	// Line numbers continue across members.
	if !strings.Contains(r.output, ":2: second ERROR") {
		t.Fatalf("line numbering across members wrong:\n%s", r.output)
	}
}

func TestScanCorruptGzip(t *testing.T) {
	r := runObjectScan(t, "logs/broken.gz", []byte("this is not gzip"), grepOpts(t, "ERROR"))
	if r.outcome.Err == nil {
		t.Fatal("corrupt gzip must fail the object")
	}
	if r.outcome.ErrClass != ErrClassCorrupt {
		t.Fatalf("class: %v", r.outcome.ErrClass)
	}
}

// A gzip that starts valid but is truncated mid-stream: lines before
// the failure are scanned, the object is marked partial.
func TestScanTruncatedGzip(t *testing.T) {
	full := gzipBytes(t, strings.Repeat("filler line\n", 20000)+"ERROR near end\n")
	truncated := full[:len(full)/2]
	r := runObjectScan(t, "logs/truncated.gz", truncated, grepOpts(t, "filler"))
	if !r.outcome.Partial {
		t.Fatalf("truncated stream must be partial: %+v", r.outcome)
	}
	if r.outcome.Matches == 0 {
		t.Fatal("lines before the failure should have been scanned")
	}
}

func TestScanZip(t *testing.T) {
	body := zipBytes(t, map[string]string{
		"stderr": "boring\nERROR from stderr\n",
		"stdout": "ERROR from stdout\n",
	})
	r := runObjectScan(t, "logs/bundle.zip", body, grepOpts(t, "ERROR"))
	if r.outcome.Matches != 2 || r.outcome.Partial {
		t.Fatalf("outcome: %+v", r.outcome)
	}
	if !strings.Contains(r.output, "logs/bundle.zip!stderr:2: ERROR from stderr") {
		t.Fatalf("zip entry name missing from output:\n%s", r.output)
	}
}

func TestScanZipEntryBudget(t *testing.T) {
	body := zipBytes(t, map[string]string{"a": "x\n", "b": "y\n", "c": "z\n"})
	opts := grepOpts(t, "nomatch")
	opts.MaxZipEntries = 2
	r := runObjectScan(t, "logs/many.zip", body, opts)
	if !r.outcome.Partial || !strings.Contains(r.outcome.PartialWhy, "max-zip-entries") {
		t.Fatalf("entry budget not enforced: %+v", r.outcome)
	}
}

// The decompression-bomb guard: cumulative expanded bytes across all
// entries, which the compressed-size cap cannot provide.
func TestScanZipExpansionBudget(t *testing.T) {
	big := strings.Repeat("A", 300) + "\n"
	body := zipBytes(t, map[string]string{
		"e1": strings.Repeat(big, 10),
		"e2": strings.Repeat(big, 10),
	})
	opts := grepOpts(t, "nomatch")
	opts.MaxZipExpandedBytes = 1000
	r := runObjectScan(t, "logs/bomb.zip", body, opts)
	if !r.outcome.Partial || !strings.Contains(r.outcome.PartialWhy, "expansion") {
		t.Fatalf("expansion budget not enforced: %+v", r.outcome)
	}
}

func TestScanCorruptZip(t *testing.T) {
	r := runObjectScan(t, "logs/broken.zip", []byte("PK garbage but not a zip"), grepOpts(t, "x"))
	if r.outcome.Err == nil || r.outcome.ErrClass != ErrClassCorrupt {
		t.Fatalf("corrupt zip: %+v", r.outcome)
	}
}

// M-04: -max-matches counts across the whole ZIP object.
func TestMaxMatchesAcrossZip(t *testing.T) {
	body := zipBytes(t, map[string]string{
		"e1": "ERROR 1\nERROR 2\n",
		"e2": "ERROR 3\nERROR 4\n",
	})
	opts := grepOpts(t, "ERROR")
	opts.MaxMatches = 3
	r := runObjectScan(t, "logs/multi.zip", body, opts)
	if r.outcome.Matches != 3 {
		t.Fatalf("max-matches across zip: got %d want 3", r.outcome.Matches)
	}
}

func TestMaxMatchesPlain(t *testing.T) {
	r := scanRun{}
	opts := grepOpts(t, "ERROR")
	opts.MaxMatches = 2
	r = runObjectScan(t, "l.log", []byte("ERROR a\nERROR b\nERROR c\n"), opts)
	if r.outcome.Matches != 2 {
		t.Fatalf("got %d matches want 2", r.outcome.Matches)
	}
}

// §6.4: a match within the first max-line-size bytes of an oversized
// line is found; the object is conservatively partial.
func TestOversizedLineMatchBeforeTruncation(t *testing.T) {
	line := "ERROR early " + strings.Repeat("x", 5000)
	opts := grepOpts(t, "ERROR")
	opts.MaxLineSize = 100
	r := runObjectScan(t, "l.log", []byte(line+"\nERROR after\n"), opts)
	if r.outcome.Matches != 2 {
		t.Fatalf("matches: got %d want 2 (truncated head + following line)", r.outcome.Matches)
	}
	if !r.outcome.Partial {
		t.Fatal("truncation must mark the object partially scanned")
	}
	if r.counters.OversizedLines.Load() != 1 {
		t.Fatalf("oversizedLines: %d", r.counters.OversizedLines.Load())
	}
}

// A match hidden in the drained tail is missed — which is exactly why
// the object must be flagged partial.
func TestOversizedLineMatchHiddenInTail(t *testing.T) {
	line := strings.Repeat("x", 5000) + " ERROR hidden"
	opts := grepOpts(t, "ERROR")
	opts.MaxLineSize = 100
	r := runObjectScan(t, "l.log", []byte(line+"\n"), opts)
	if r.outcome.Matches != 0 {
		t.Fatalf("match in drained tail should not be found (got %d)", r.outcome.Matches)
	}
	if !r.outcome.Partial {
		t.Fatal("object must be partial so the miss is visible")
	}
}

func TestNamesOnlyFirstHit(t *testing.T) {
	opts := grepOpts(t, "ERROR")
	opts.NamesOnly = true
	r := runObjectScan(t, containerKey, gzipBytes(t, "ERROR one\nERROR two\n"), opts)
	if r.output != "s3://test-bucket/"+containerKey+"\n" {
		t.Fatalf("names-only output:\n%q", r.output)
	}
	if r.outcome.Matches != 1 {
		t.Fatalf("first-hit exit: %d matches", r.outcome.Matches)
	}
	if len(r.outcome.AppIDs) != 1 || r.outcome.AppIDs[0] != "application_1700000000000_0007" {
		t.Fatalf("app ID from key: %v", r.outcome.AppIDs)
	}
}

// C-01 scenario: step log (no ID in key), -l alone stops at the first
// match before any ID has appeared — best-effort means no ID.
func TestNamesOnlyStepLogBestEffort(t *testing.T) {
	content := "step started\nERROR: table not found\nfor application_1700000000000_0099\n"
	opts := grepOpts(t, "ERROR")
	opts.NamesOnly = true
	r := runObjectScan(t, stepKey, gzipBytes(t, content), opts)
	if r.outcome.Matches != 1 || r.outcome.Err != nil {
		t.Fatalf("scan failed: %+v", r.outcome)
	}
	if len(r.outcome.AppIDs) != 0 {
		t.Fatalf("without -discover-apps the later ID must not be read: %v", r.outcome.AppIDs)
	}
}

// §8: -l -discover-apps keeps reading (without printing) after the
// first match until an ID appears or the object ends.
func TestNamesOnlyDiscoverApps(t *testing.T) {
	content := "step started\nERROR: table not found\nmore text\nrunning application_1700000000000_0099\ntrailing\n"
	opts := grepOpts(t, "ERROR")
	opts.NamesOnly = true
	opts.DiscoverApps = true
	r := runObjectScan(t, stepKey, gzipBytes(t, content), opts)
	if len(r.outcome.AppIDs) != 1 || r.outcome.AppIDs[0] != "application_1700000000000_0099" {
		t.Fatalf("discover-apps must find the ID after the match: %v", r.outcome.AppIDs)
	}
	if strings.Count(r.output, "\n") != 1 {
		t.Fatalf("only the key must be printed:\n%q", r.output)
	}
}

// Preceding context: the ID appears before the match.
func TestDiscoverAppsPrecedingContext(t *testing.T) {
	content := "launching application_1700000000000_0050\nstuff\nERROR: exploded\n"
	opts := grepOpts(t, "ERROR")
	opts.NamesOnly = true
	opts.DiscoverApps = true
	r := runObjectScan(t, stepKey, gzipBytes(t, content), opts)
	if len(r.outcome.AppIDs) != 1 || r.outcome.AppIDs[0] != "application_1700000000000_0050" {
		t.Fatalf("preceding-context ID: %v", r.outcome.AppIDs)
	}
}

// -l -discover-apps with no ID anywhere: reads to EOF, reports none.
func TestDiscoverAppsNoIDToEOF(t *testing.T) {
	opts := grepOpts(t, "ERROR")
	opts.NamesOnly = true
	opts.DiscoverApps = true
	r := runObjectScan(t, stepKey, gzipBytes(t, "ERROR at start\nno ids anywhere\n"), opts)
	if len(r.outcome.AppIDs) != 0 {
		t.Fatalf("expected no ID, got %v", r.outcome.AppIDs)
	}
	if r.outcome.Matches != 1 {
		t.Fatalf("matches: %d", r.outcome.Matches)
	}
}

func TestBzip2(t *testing.T) {
	// A tiny pre-built bzip2 stream containing "ERROR bz2 works\n"
	// (compress/bzip2 has no writer; bytes generated externally).
	r := runObjectScan(t, "logs/x.bz2", bz2Sample, grepOpts(t, "ERROR"))
	if r.outcome.Matches != 1 || r.outcome.Err != nil {
		t.Fatalf("bzip2: %+v", r.outcome)
	}
}

// A wholly undecodable .bz2 is a hard failure, classified the same as
// a corrupt .gz. bzip2 differs only in WHEN it notices: gzip and zip
// validate their headers when the reader is opened, while bzip2 fails
// on the first read, so the failure arrives as a stream error. That
// must not be mistaken for a partial scan — nothing was scanned at
// all, and "partially scanned, 0 matches" reads as an object that had
// no matches rather than one that could not be read.
func TestCorruptBzip2IsAHardFailure(t *testing.T) {
	r := runObjectScan(t, "logs/x.bz2", []byte("BZh9 not really bzip2 data"), grepOpts(t, "x"))
	if r.outcome.Err == nil {
		t.Fatalf("corrupt bzip2 must fail, not partially succeed: %+v", r.outcome)
	}
	if r.outcome.Partial {
		t.Errorf("nothing was decoded, so the object is not partially scanned: %+v", r.outcome)
	}
	if r.outcome.ErrClass != ErrClassCorrupt {
		t.Errorf("ErrClass = %v, want %v (same as a corrupt .gz)", r.outcome.ErrClass, ErrClassCorrupt)
	}
}

// A .bz2 that decodes for a while and then breaks IS a partial scan:
// some lines were delivered, so the run must report the object as
// incomplete rather than failed, and keep the matches it found.
func TestTruncatedBzip2IsPartial(t *testing.T) {
	truncated := bz2Sample[:len(bz2Sample)-6]
	r := runObjectScan(t, "logs/x.bz2", truncated, grepOpts(t, "ERROR"))
	if r.outcome.Err != nil {
		t.Fatalf("a stream that decoded some data must not be a hard failure: %+v", r.outcome)
	}
	if !r.outcome.Partial {
		t.Errorf("expected a partial scan: %+v", r.outcome)
	}
}
