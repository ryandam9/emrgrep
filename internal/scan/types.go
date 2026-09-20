// Package scan implements the emrgrep engine: listing, filtering,
// scheduling, downloading, decompression, line matching, and reporting.
package scan

import (
	"sync/atomic"
	"time"
)

// ObjectDescriptor carries the listing metadata for one S3 object.
// The ETag is retained so the later GetObject can be conditioned with
// If-Match, guaranteeing listing metadata and scanned content belong to
// the same object version.
type ObjectDescriptor struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	StorageClass string
	// Restored reports that an archived object (Glacier/Deep Archive)
	// currently has a readable restored copy, per the RestoreStatus
	// attribute returned by ListObjectsV2.
	Restored bool
}

// ErrorClass buckets per-object failures into the named counters
// required by the design (§10). Errors are classified, never lumped.
type ErrorClass int

const (
	ErrClassNone ErrorClass = iota
	ErrClassAccessDenied
	ErrClassNotFound
	ErrClassChangedAfterListing
	ErrClassCorrupt
	ErrClassTimeout
	ErrClassArchived  // InvalidObjectState: archived and not restored
	ErrClassThrottled // SlowDown and other rate-limit responses
	ErrClassOther
)

func (c ErrorClass) String() string {
	switch c {
	case ErrClassAccessDenied:
		return "accessDenied"
	case ErrClassNotFound:
		return "notFound"
	case ErrClassChangedAfterListing:
		return "changedAfterListing"
	case ErrClassCorrupt:
		return "corrupt"
	case ErrClassTimeout:
		return "timeout"
	case ErrClassArchived:
		return "archivedUnavailable"
	case ErrClassThrottled:
		return "throttled"
	case ErrClassOther:
		return "other"
	}
	return "none"
}

// Counters is the shared, atomically updated account of everything the
// run did. Every budget violation and every skipped or failed object is
// visible here; nothing is silently absorbed (§9, goal 6).
type Counters struct {
	Listed          atomic.Int64 // keys returned by ListObjectsV2
	Survived        atomic.Int64 // survived the metadata filter chain
	FoldersSkipped  atomic.Int64 // zero-byte "/"-suffixed markers
	ArchivedSkipped atomic.Int64 // Glacier / Deep Archive at listing time
	OversizeSkipped atomic.Int64 // above -max-size
	TimeFiltered    atomic.Int64
	ExtFiltered     atomic.Int64
	KeyFiltered     atomic.Int64

	ScannedFully     atomic.Int64 // read to EOF
	StoppedEarly     atomic.Int64 // terminated by request: -l or -max-matches
	ScannedPartially atomic.Int64
	MatchedObjects   atomic.Int64
	MatchedLines     atomic.Int64
	BytesDownloaded  atomic.Int64 // compressed bytes read from S3
	OversizedLines   atomic.Int64

	AccessDenied        atomic.Int64
	NotFound            atomic.Int64
	ChangedAfterListing atomic.Int64
	Corrupt             atomic.Int64
	Timeout             atomic.Int64
	ArchivedUnavailable atomic.Int64
	Throttled           atomic.Int64
	OtherErrors         atomic.Int64

	WarningsSuppressed atomic.Int64
}

// AddError bumps the counter for one classified per-object failure.
func (c *Counters) AddError(class ErrorClass) {
	switch class {
	case ErrClassAccessDenied:
		c.AccessDenied.Add(1)
	case ErrClassNotFound:
		c.NotFound.Add(1)
	case ErrClassChangedAfterListing:
		c.ChangedAfterListing.Add(1)
	case ErrClassCorrupt:
		c.Corrupt.Add(1)
	case ErrClassTimeout:
		c.Timeout.Add(1)
	case ErrClassArchived:
		c.ArchivedUnavailable.Add(1)
	case ErrClassThrottled:
		c.Throttled.Add(1)
	case ErrClassOther:
		c.OtherErrors.Add(1)
	}
}

// ObjectErrors is the total of all classified per-object failures.
// ChangedAfterListing counts here: the object was selected but not
// scanned, which callers must be able to see in the exit status.
func (c *Counters) ObjectErrors() int64 {
	return c.AccessDenied.Load() +
		c.NotFound.Load() +
		c.ChangedAfterListing.Load() +
		c.Corrupt.Load() +
		c.Timeout.Load() +
		c.ArchivedUnavailable.Load() +
		c.Throttled.Load() +
		c.OtherErrors.Load()
}
