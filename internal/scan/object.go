package scan

import (
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Format is the detected object format (by case-insensitive extension
// in v2; magic bytes and Content-Encoding are staged work, §13).
type Format int

const (
	FormatPlain Format = iota
	FormatGzip
	FormatBzip2
	FormatZip
)

// DetectFormat classifies a key by extension.
func DetectFormat(key string) Format {
	switch strings.ToLower(path.Ext(key)) {
	case ".gz", ".gzip":
		return FormatGzip
	case ".bz2":
		return FormatBzip2
	case ".zip":
		return FormatZip
	default:
		return FormatPlain
	}
}

// errZipExpandBudget aborts an object (marking it partially scanned)
// without failing the run.
var errZipExpandBudget = errors.New("zip expansion budget exceeded")

// ScanOptions carries the per-object scanning configuration.
type ScanOptions struct {
	Grep         *Matcher
	CatMode      bool  // -cat: Grep matches every line; no highlighting
	MaxMatches   int64 // per object; whole ZIP = one object (M-04). 0 = unlimited
	MaxLineSize  int
	NamesOnly    bool // -l: first-hit exit
	DiscoverApps bool // with -l: read on until an app ID is found

	MaxZipEntries       int
	MaxZipExpandedBytes int64 // cumulative across all entries (§6.3)
	TempDir             string

	// limiter enforces the run-wide -max-total-matches cap; set by
	// the engine before workers start. nil = unlimited.
	limiter *matchLimiter
}

// ObjectOutcome reports what scanning one object produced.
type ObjectOutcome struct {
	Matches      int64
	StoppedEarly bool       // terminated by request (-l, -max-matches), not by error
	Partial      bool       // truncated lines, budget aborts, mid-stream failures
	Err          error      // non-nil only when nothing usable was scanned
	ErrClass     ErrorClass // classification for Err or the partial cause
	PartialWhy   string
	AppIDs       []string // every application ID attributed to a match
}

// objectScan holds the mutable state shared across all lines (and, for
// ZIPs, all entries) of a single object.
type objectScan struct {
	ctx     context.Context
	opts    *ScanOptions
	desc    *ObjectDescriptor
	bucket  string
	writer  *Writer
	counter *Counters

	matches    int64
	sawMatch   bool
	linesSeen  int64
	partial    bool
	partialWhy string
	streamErr  error
	ctxErr     error // context expiry observed while scanning (H-01)

	appIDs     map[string]struct{}
	appIDOrder []string

	// -l state machine: after the first match we either stop or, with
	// -discover-apps and no ID yet, keep reading without printing
	// until an ID appears or the object ends (§8).
	silentIDHunt bool
}

func (s *objectScan) addAppID(id string) {
	if id == "" {
		return
	}
	if _, dup := s.appIDs[id]; dup {
		return
	}
	s.appIDs[id] = struct{}{}
	s.appIDOrder = append(s.appIDOrder, id)
}

// ScanObject scans one already-opened object body. The caller owns the
// body and its close. Returns the outcome; per-object errors are never
// fatal to the run.
func ScanObject(ctx context.Context, bucket string, desc *ObjectDescriptor, body io.Reader, format Format, opts *ScanOptions, writer *Writer, counters *Counters) ObjectOutcome {
	s := &objectScan{
		ctx:     ctx,
		opts:    opts,
		desc:    desc,
		bucket:  bucket,
		writer:  writer,
		counter: counters,
		appIDs:  make(map[string]struct{}),
	}

	var err error
	switch format {
	case FormatGzip:
		err = s.scanGzip(body)
	case FormatBzip2:
		s.scanLines(bzip2.NewReader(body), "")
	case FormatZip:
		err = s.scanZip(body)
	default:
		s.scanLines(body, "")
	}

	out := ObjectOutcome{
		Matches:      s.matches,
		StoppedEarly: s.done(),
		Partial:      s.partial,
		PartialWhy:   s.partialWhy,
		AppIDs:       s.appIDOrder,
	}

	// H-01: an object deadline observed mid-scan means unread data may
	// remain — never a full scan. A plain cancellation is left to the
	// engine, which knows whether the whole run is being torn down.
	deadlineHit := errors.Is(s.ctxErr, context.DeadlineExceeded) && !out.StoppedEarly

	switch {
	case err != nil && s.linesSeen == 0 && !out.Partial:
		// Nothing usable was scanned: a hard failure, not a partial.
		out.Err = err
		out.ErrClass = classifyContentError(ctx, err)
	case err != nil:
		out.Partial = true
		if out.PartialWhy == "" {
			out.PartialWhy = err.Error()
		}
		out.ErrClass = classifyContentError(ctx, err)
	case deadlineHit && s.linesSeen == 0 && !out.Partial:
		out.Err = s.ctxErr
		out.ErrClass = ErrClassTimeout
	case deadlineHit:
		out.Partial = true
		if out.PartialWhy == "" {
			out.PartialWhy = "object timeout before end of stream"
		}
		out.ErrClass = ErrClassTimeout
	case s.streamErr != nil:
		// scanLines already marked the object partial; classify it.
		out.ErrClass = classifyContentError(ctx, s.streamErr)
	}
	return out
}

func (s *objectScan) scanGzip(body io.Reader) error {
	zr, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()
	// Multistream mode (the default) is required: rotated logs are
	// concatenated gzip members.
	zr.Multistream(true)
	s.scanLines(zr, "")
	return nil
}

func (s *objectScan) scanZip(body io.Reader) error {
	// The object streams to a temporary file, never RAM (§6.3). The
	// file is removed on every path, including cancellation.
	tmp, err := os.CreateTemp(s.opts.TempDir, "emrgrep-zip-*")
	if err != nil {
		return fmt.Errorf("zip temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	size, err := io.Copy(tmp, body)
	if err != nil {
		return fmt.Errorf("zip download: %w", err)
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("zip open: %w", err)
	}

	if s.opts.MaxZipEntries > 0 && len(zr.File) > s.opts.MaxZipEntries {
		s.markPartial(fmt.Sprintf("zip has %d entries; budget is %d (-max-zip-entries)", len(zr.File), s.opts.MaxZipEntries))
		return nil
	}

	// Cumulative expanded bytes across all entries — the
	// decompression-bomb guard the compressed-size cap cannot provide.
	var expanded int64
	for _, f := range zr.File {
		if s.done() || s.ctx.Err() != nil {
			return nil // done()/ctxErr already capture why
		}
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			s.markPartial(fmt.Sprintf("zip entry %s: %v", f.Name, err))
			return nil
		}
		lr := &expansionLimitedReader{r: rc, total: &expanded, limit: s.opts.MaxZipExpandedBytes}
		s.scanLines(lr, f.Name)
		rc.Close()
		if lr.exceeded {
			s.markPartial(fmt.Sprintf("cumulative expansion exceeded %d bytes (-max-uncompressed-object-size)", s.opts.MaxZipExpandedBytes))
			return nil
		}
	}
	return nil
}

// expansionLimitedReader enforces the cumulative ZIP expansion budget.
// Reads are capped at one byte past the remaining budget so that an
// archive whose expanded size is exactly the limit reaches EOF without
// a false positive, while overshoot beyond the limit is at most one
// byte (M-01).
type expansionLimitedReader struct {
	r        io.Reader
	total    *int64
	limit    int64
	exceeded bool
}

func (e *expansionLimitedReader) Read(p []byte) (int, error) {
	if e.limit > 0 {
		remaining := e.limit - *e.total
		if remaining < 0 {
			e.exceeded = true
			return 0, errZipExpandBudget
		}
		if int64(len(p)) > remaining+1 {
			p = p[:remaining+1]
		}
	}
	n, err := e.r.Read(p)
	*e.total += int64(n)
	if e.limit > 0 && *e.total > e.limit {
		e.exceeded = true
		return n, errZipExpandBudget
	}
	return n, err
}

// done reports whether the object's termination condition is met:
// the run-wide match cap, max-matches for this object, or -l
// satisfied (§8 termination rules).
func (s *objectScan) done() bool {
	if s.opts.limiter.Satisfied() {
		return true
	}
	if s.opts.MaxMatches > 0 && s.matches >= s.opts.MaxMatches {
		return true
	}
	if s.opts.NamesOnly && s.sawMatch && !s.silentIDHunt {
		return true
	}
	return false
}

func (s *objectScan) markPartial(why string) {
	if !s.partial {
		s.partial = true
		s.partialWhy = why
	}
}

// scanLines runs the line iterator over one decompressed stream and
// applies matching, discovery, and termination rules. Each stream (the
// object, or one ZIP entry) gets its own AppIDTracker seeded from the
// object key, so preceding-context attribution never leaks across ZIP
// entries (M-11).
func (s *objectScan) scanLines(r io.Reader, zipEntry string) {
	it := NewLineIterator(r, s.opts.MaxLineSize)
	tracker := NewAppIDTracker(s.desc.Key)
	for {
		// H-01: record context expiry instead of silently stopping.
		if err := s.ctx.Err(); err != nil {
			s.ctxErr = err
			return
		}
		if s.done() {
			return
		}
		line, truncated, ok := it.Next()
		if !ok {
			break
		}
		s.linesSeen++
		if truncated {
			s.counter.OversizedLines.Add(1)
			// Conservative: truncation could have hidden a match in
			// the drained tail (§6.4).
			s.markPartial("oversized line truncated at -max-line-size")
		}
		// Discovery source 3: preceding context within this stream.
		tracker.Observe(line)

		if s.silentIDHunt {
			// -l -discover-apps: match found, ID still missing — keep
			// reading without printing until an ID appears or EOF.
			if id := tracker.Current(); id != "" {
				s.addAppID(id)
				s.silentIDHunt = false
				return
			}
			continue
		}

		if !s.opts.Grep.Match(line) {
			continue
		}

		// H-03: attribution is per match, resolved now — a later
		// unrelated ID can never overwrite it, and one object can
		// contribute several IDs.
		ids := tracker.IDsForMatch(line)
		for _, id := range ids {
			s.addAppID(id)
		}

		if s.opts.NamesOnly {
			if !s.sawMatch {
				// The run-wide cap counts each reported object once.
				if !s.opts.limiter.Reserve() {
					return
				}
				// M-12: count only after the writer accepted the result.
				if !s.writer.Emit(s.ctx, Result{Bucket: s.bucket, Key: s.desc.Key, KeyOnly: true}) {
					s.opts.limiter.Release()
					s.ctxErr = s.ctx.Err()
					return
				}
			}
			s.sawMatch = true
			s.matches++
			s.counter.MatchedLines.Add(1)
			if s.opts.DiscoverApps && len(s.appIDs) == 0 {
				s.silentIDHunt = true
				continue
			}
			return
		}

		if !s.opts.limiter.Reserve() {
			return // run-wide cap exhausted
		}
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		if !s.writer.Emit(s.ctx, Result{
			Bucket:   s.bucket,
			Key:      s.desc.Key,
			ZipEntry: zipEntry,
			LineNo:   it.LineNo(),
			Line:     lineCopy,
		}) {
			s.opts.limiter.Release()
			s.ctxErr = s.ctx.Err()
			return
		}
		s.sawMatch = true
		s.matches++
		s.counter.MatchedLines.Add(1)
	}
	if err := it.Err(); err != nil && !errors.Is(err, errZipExpandBudget) {
		// Mid-stream failure: partially scanned, never retried — a
		// compressed stream cannot resume, and re-scanning would
		// duplicate already-emitted matches (§6.1).
		s.streamErr = err
		s.markPartial(fmt.Sprintf("stream error: %v", err))
	}
}

// classifyContentError buckets decompression/stream failures.
func classifyContentError(ctx context.Context, err error) ErrorClass {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrClassTimeout
	case errors.Is(err, context.Canceled):
		return ErrClassOther
	case errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum),
		errors.Is(err, zip.ErrFormat), errors.Is(err, zip.ErrChecksum),
		strings.Contains(err.Error(), "bzip2"),
		strings.Contains(err.Error(), "gzip"),
		strings.Contains(err.Error(), "zip"):
		return ErrClassCorrupt
	default:
		return ErrClassOther
	}
}
