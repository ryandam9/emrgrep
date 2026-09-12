# Design Document: `s3logscan`

**A resource-budgeted concurrent scanner for EMR/YARN logs stored in S3**

| | |
|---|---|
| Version | 2.0 |
| Status | **Implemented** (initial implementation in commit `7ebc696`; code-review fixes applied on top — see `S3_LOG_SCAN_CODE_REVIEW_20260722` findings) |
| Date | 22 July 2026 |
| Supersedes | v1.1 |
| Inputs | v1.1 design; comprehensive design review `v1-review.md` (22 Jul 2026); code review of 22 Jul 2026 |
| Status of review findings | All four critical design findings resolved; disposition table in Appendix A. Post-implementation code review found four high-priority defects (timeout accounting, timeout exit semantics, single-ID discovery, archive restore state), all fixed in the current source |

---

## 1. Problem Statement

EMR clusters write their logs to S3 under a deep, self-describing key hierarchy. A single bucket accumulates thousands of objects across many clusters, applications, and containers, mixing plain text with compressed formats (predominantly `.gz`, occasionally `.zip`, rarely `.bz2`).

Two investigation workflows recur:

1. **Known application.** The operator has a YARN application ID and must locate and read every log object belonging to it, without knowing which sub-directories contain them.
2. **Unknown application.** The operator has only a search string — an error message, job name, table or path — and must discover *which* application produced it, then drill into that application.

Downloading everything and grepping is slow and expensive at this scale. The tool must minimize both the number of objects downloaded and the number of bytes read from each, while remaining safe to run from an operator's laptop or a restricted operations host.

## 2. Goals and Non-Goals

### Goals

1. Scan an S3 prefix and report object keys and matching content lines, grep-style.
2. Reject as much work as possible before any download, using listing metadata only.
3. Handle `.gz`, `.bz2`, `.zip`, and plain text transparently, streaming wherever the format permits.
4. **Bound every resource by an explicit budget** — memory, temporary disk, archive expansion, open response bodies, queued output — independently of worker count. *(Promoted to a primary goal by the review.)*
5. Make the unknown-application workflow reliable: report the YARN application IDs behind matches even when the ID is absent from the object key (step logs).
6. **Never silently under-report.** Partial scans, truncated lines, skipped objects, and per-object failures are always visible in counters and exit status.
7. Degrade gracefully: one unreadable object is a classified warning, never a run-ending failure.
8. Be a well-behaved Unix citizen: clean stdout/stderr separation, stable exit codes, prompt reaction to `SIGINT` and broken pipes.

### Non-Goals

No persistent index, cache, or background crawling (S3 Inventory/Athena are the right tools at millions of objects — §13). No structured log parsing; matching is line-oriented. No bucket mutation of any kind. Not an analytics platform.

## 3. Background: EMR Key Layout and Honest Listing Semantics

### 3.1 Layout

```
<logUri>/j-<CLUSTER_ID>/
├── containers/application_<ts>_<seq>/container_<...>/{stdout,stderr,syslog}.gz
├── steps/s-<STEP_ID>/{controller,stderr,stdout}.gz
├── node/i-<INSTANCE>/applications/...
└── hadoop-mapreduce/history/...
```

Two structural facts drive the design:

- **Container logs embed the application ID in the key.** The known-application workflow therefore needs listing plus a client-side key filter — downloads are limited to the application's own objects.
- **Step logs do not.** `steps/s-*/stderr.gz` keys carry a cluster ID but no application ID, yet these small files usually contain the failure summary. Discovery logic must therefore look *inside* content, not just at keys (§8).

### 3.2 What filtering does and does not save *(corrects v1.1, per review C-03)*

The prefix is the **only server-side filter**. Key regex, extension, size, and time filters run client-side against each returned listing page. They reduce `GetObject` calls, downloaded bytes, and decompression work — often by orders of magnitude — but they do **not** reduce the number of keys S3 enumerates, the number of LIST pages, or listing latency. Listing time remains proportional to the number of keys beneath the supplied prefix.

Design consequences:

- An empty prefix (whole-bucket enumeration) is refused unless `-allow-whole-bucket-scan` is given.
- Documentation and examples steer operators to cluster-scoped prefixes (`.../j-<CLUSTER>/`).
- For very large buckets, alternative Phase-1 sources (S3 Inventory manifest; delimiter fan-out per cluster) are the roadmap (§13), not more client-side filtering.

## 4. Architecture Overview

```
┌────────────────────────────┐
│ S3 ListObjectsV2 paginator │  ObjectDescriptor:
└──────────────┬─────────────┘  key, size, mtime, ETag, storage class
               ▼
┌────────────────────────────┐  filters, cheapest first:
│ Metadata filter + windowed │  folder marker → storage class → size
│ smallest-first scheduler   │  → time window → extension → key regex
│ (bounded min-heap)         │
└──────────────┬─────────────┘
        bounded work channel
   ┌───────────┼───────────┐
   ▼           ▼           ▼
┌───────┐  ┌───────┐   ┌───────┐  per object: GET with If-Match,
│ W-1   │  │ W-2   │ … │ W-N   │  optional per-object timeout,
└───┬───┘  └───┬───┘   └───┬───┘  decompress, line-iterate, match
    └──────────┼───────────┘
     bounded result channel        ZIP path additionally gated by a
               ▼                   dedicated semaphore + temp file
┌────────────────────────────┐
│ Writer goroutine (sole     │  serialize, sanitize control chars,
│ owner of stdout)           │  EPIPE → cancel the run
└──────────────┬─────────────┘
               ▼
┌────────────────────────────┐
│ App-ID collector + summary │  key + matching line + preceding
└────────────────────────────┘  context; -discover-apps mode
```

The v1.1 shape (one lister, bounded channel, N workers, shared cancellation, atomic counters) is retained, with two structural additions demanded by the review: the **writer stage** (H-02) and the **resource-budget model** (§9) that governs ZIP handling, scheduling memory, and output pressure.

## 5. Phase 1 — Listing, Filtering, Scheduling

### 5.1 Filter chain (cheapest first, M-01)

1. **Folder markers**: skip only keys that end in `/` **and** are zero bytes (H-07). Non-empty `/`-suffixed objects are scanned.
2. **Storage class**: Glacier and Deep Archive objects cannot be read until restored; they are skipped at listing time and counted (`archivedSkipped`) rather than failing mid-scan (H-05).
3. **Size cap** (`-max-size`, compressed bytes): oversized objects are warned and skipped.
4. **Time window** against S3 `LastModified` (never timestamps inside log text). Semantics (M-02): date-only input means **UTC midnight**; `-after` is **inclusive**, `-before` is **exclusive**; full RFC3339 with offsets is accepted for precision.
5. **Extension allow-list**, case-insensitive.
6. **Key pattern** last, since regex evaluation is the costliest check.

### 5.2 Scheduling and smallest-first (H-01)

Without `-smallest-first`, survivors stream straight to the bounded work channel — dispatch begins with the first listing page.

With `-smallest-first`, v1.1 buffered the *entire* listing before dispatching; the review correctly flagged both the time-to-first-result stall and the unbounded descriptor memory. v2 uses a **bounded min-heap window** (`-smallest-first-window`, default 5 000): the lister pushes survivors into the heap; once the window is full, each new arrival evicts the current smallest to the workers; the heap drains smallest-first after listing completes. Properties:

- memory is bounded by the window size regardless of prefix breadth;
- the first (small) objects reach workers while listing is still running;
- ordering is **approximate**, not global — a deliberate, documented trade. Operators who want true global ordering can raise the window to exceed the expected survivor count.

Rationale for a window over a full priority queue: discovery workflows only need "mostly small first" — step/stderr files are so much smaller than container logs that approximate ordering finds them immediately.

## 6. Phase 2 — Download and Scan

### 6.1 Consistency, timeouts, retries (H-04, H-06)

Each work item carries the **listed ETag**, sent as `If-Match` on `GetObject`. A `412 Precondition Failed` means the object changed between LIST and GET; it is counted (`changedAfterListing`), reported as `object changed after listing; not scanned`, and skipped — metadata and content are never mixed across object versions.

Timeout layers: `-overall-timeout` bounds the whole command; `-object-timeout` bounds each object via a child context. Retry policy is explicit: SDK-level retries are permitted only **up to receipt of a usable response body**. Once scanning has begun, a mid-stream failure marks the object **partially scanned** and is never retried — a compressed stream cannot resume mid-way, and re-scanning from the start would duplicate already-emitted matches.

### 6.2 Format handling

Detection is by case-insensitive extension in v2 (`Content-Encoding`, magic bytes, and an explicit override are staged in §13 per M-07). Per format:

| Format | Strategy | Memory/disk profile |
|---|---|---|
| `.gz`, `.gzip` | streaming `gzip.Reader` over the response body, multistream enabled (rotated logs concatenate members) | O(buffer) RAM |
| `.bz2` | streaming `bzip2.Reader` | O(buffer) RAM |
| `.zip` | temp file + budgets, below | ≤ `-max-size` **disk** per concurrent ZIP |
| other | scanned as text; sanitization (§7.2) neutralizes binary output | O(buffer) RAM |

### 6.3 ZIP safety model *(replaces v1.1, per review C-02)*

v1.1 buffered whole ZIPs in RAM, capped only by *compressed* size — `workers × 128 MiB` of buffers before decompression even starts, and no bound at all on expansion. v2:

1. A **dedicated semaphore** (`-zip-workers`, default 2) gates concurrent ZIP processing independently of `-workers`.
2. The object streams to a **temporary file** (never RAM), bounded by `-max-size`; temp files are removed on every path, including cancellation.
3. A random-access ZIP reader opens the temp file; **entries stream** through the line iterator one at a time and are never materialized.
4. Budgets abort processing (marking the object partial): `-max-zip-entries` (default 10 000) and `-max-uncompressed-object-size` (default 512 MiB, **cumulative expanded bytes across all entries** — this is the decompression-bomb guard the compressed-size cap cannot provide).
5. `-max-matches` counts across the whole ZIP object; the ZIP is one object (M-04).

Worst-case envelope, now explicit: RAM ≈ `workers × (network + line buffers)`; temp disk ≤ `zip-workers × max-size`.

### 6.4 Line iteration *(replaces v1.1, per review C-04)*

The fixed-limit `bufio.Scanner` is removed: a line above its ceiling silently *terminated* the object's scan, hiding any later matches. v2 uses a `bufio.Reader`-based iterator with an explicit oversized-line policy:

- a line longer than `-max-line-size` (default 4 MiB) is matched against its **first** `max-line-size` bytes;
- the remainder is drained without allocation;
- the line number still increments and scanning **continues** to end of stream;
- each occurrence increments `oversizedLines`, and the object is flagged `scannedPartially` (conservative: truncation *could* have hidden a match within the drained tail);
- final lines without a trailing newline are processed; CRLF is normalized.

The policy default is truncate-and-continue; `error` (abort object) can be added as a flag later if a stricter mode is wanted.

## 7. Output Architecture

### 7.1 Writer stage (H-02)

Workers never write to stdout. Match results flow through a bounded channel to a single **writer goroutine** that serializes output through a buffered writer. On any write error — `EPIPE` from `| head`, a closed redirect — the writer cancels the shared context, drains its channel so no worker blocks, and the run reports interruption rather than a misleading success summary. This decouples download throughput from consumer speed and gives broken-pipe behavior a single, testable home.

Output ordering is **completion order** — non-deterministic under concurrency — and documented as such (M-06). Diagnostics go to stderr only, bounded by `-max-warnings` (default 100) with a final `further warnings suppressed; N additional object errors` line (M-08).

### 7.2 Sanitization (H-09)

By default (`-sanitize-output`, on), control characters other than tab — in matched content, object keys, and ZIP entry names — are replaced with a placeholder before printing, preventing terminal-escape injection and NUL confusion. Grep-style line format: `s3://bucket/key[!zipEntry]:lineNo: text`. JSONL with exact escaped values is the machine-readable path, staged in §13.

## 8. Application-ID Discovery *(replaces v1.1, per review C-01)*

v1.1 extracted `application_\d+_\d+` only from the object **key** — which fails precisely where discovery matters: step logs, whose keys carry no application ID, combined with `-l` first-hit exit, could complete a discovery run reporting *no* application at all.

v2 extracts IDs from three sources, in priority order:

1. **the object key** (container logs — free, checked once);
2. **the matching line** itself;
3. **preceding context** — the most recent ID seen anywhere earlier in the object. Per-line ID scanning is enabled only when the key lacks an ID, so container-log scans pay nothing for it.

Termination rules:

- **default (no `-l`)**: ordinary grep semantics; IDs are collected opportunistically from all three sources as matches occur;
- **`-l`**: stop at the first content match; ID discovery is best-effort (documented as such);
- **`-l -discover-apps`**: implements the review's rule — after the first match, if no ID has yet been found, keep **reading** (without printing further matches) until an ID appears or the object ends: *stop when (match found AND ID found) or EOF.*

The run summary prints the deduplicated ID set, turning a broad discovery scan into inputs for cheap known-application queries.

## 9. Resource Budget Model

The review's central architectural point: worker count alone bounds none of the resources that matter. v2 makes each budget explicit and independently configurable:

| Resource | Bound by | Default envelope |
|---|---|---|
| Download parallelism / open response bodies | `-workers` | 16 |
| RAM per worker | line buffer (`-max-line-size`) + fixed read buffers | ~4.3 MiB × workers |
| ZIP concurrency | `-zip-workers` semaphore | 2 |
| Temp disk | `zip-workers × -max-size` | 256 MiB |
| Archive expansion | `-max-uncompressed-object-size` cumulative counter | 512 MiB per ZIP |
| ZIP entry count | `-max-zip-entries` | 10 000 |
| Scheduler memory | `-smallest-first-window` descriptors | 5 000 |
| Queued output | bounded result channel | workers × 8 lines |
| stderr volume | `-max-warnings` | 100 |
| Wall clock | `-object-timeout`, `-overall-timeout` | off |

Every budget violation is visible: a counter increments and, where an object was affected, it is classified as partially scanned or skipped — never silently absorbed.

## 10. Failure Policy, Error Classification, Exit Codes

Per-object errors warn (subject to the warning cap) and the run continues. Errors are classified into named counters rather than lumped (H-05): `accessDenied` (including KMS denials), `notFound`, `changedAfterListing`, `archivedSkipped`, `corrupt`, `timeout`, `other`.

Exit codes (H-08), stable and documented:

```
0    completed; one or more matches; no object errors or partial scans
1    completed; no matches
2    fatal usage, configuration, credential, or listing error
3    completed, but with ≥1 object error or partially scanned object
130  interrupted (SIGINT/SIGTERM); summary is still printed
```

The final summary separates fully scanned, partially scanned, skipped (with reasons), and failed objects, plus: listed, survived-filters, matched objects, matched lines, compressed bytes downloaded, oversized lines, and the per-class error counts.

## 11. CLI Specification

```
-bucket string                  required
-prefix string                  required unless -allow-whole-bucket-scan
-allow-whole-bucket-scan        explicit opt-in to empty-prefix enumeration
-key pattern                    object-key filter (client-side; cuts GETs, not LIST work)
-grep pattern                   content filter; omit → list-only mode (no downloads)
-F                              -key/-grep are fixed strings, not regex (M-10)
-i                              case-insensitive matching
-ext list                       e.g. .gz,.log (case-insensitive)
-after / -before time           RFC3339 or YYYY-MM-DD (UTC midnight);
                                after inclusive, before exclusive; vs LastModified
-max-size MiB                   compressed object cap (default 128; 0 = unlimited)
-max-zip-entries N              default 10000
-max-uncompressed-object-size MiB   cumulative ZIP expansion budget (default 512)
-max-line-size MiB              oversized-line truncation boundary (default 4)
-max-matches N                  per object; whole ZIP = one object (0 = unlimited)
-l                              names only; first-hit exit; best-effort IDs
-discover-apps                  with -l: read on until an application ID is found
-smallest-first                 windowed approximate size ordering
-smallest-first-window N        default 5000
-workers N                      1–256 (default 16)
-zip-workers N                  1–workers (default 2)
-object-timeout / -overall-timeout durations (0 = none)
-request-payer requester        requester-pays buckets
-expected-bucket-owner id       cross-account safety check
-sanitize-output bool           default true
-max-warnings N                 default 100
-region string                  AWS region override
```

Validation (M-03) is fail-fast with exit 2: worker/window/size ranges checked, `after < before` enforced, `-discover-apps` requires `-l`, malformed extension lists rejected; `0` means "unlimited/disabled" exactly where the help text says so. Regex mode uses Go RE2 semantics — no lookaround or backreferences — which is documented alongside `-F`.

## 12. AWS Integration (H-05)

Required IAM: `s3:ListBucket` on the bucket (prefix-conditioned where policy allows) and `s3:GetObject` on the log prefix; `kms:Decrypt` on the bucket key when SSE-KMS is in use. A least-privilege policy example ships in the README. Credentials resolve through the standard chain (environment, `AWS_PROFILE`, instance/task roles); `-profile` selects a named shared-config profile via `WithSharedConfigProfile`, taking precedence over `AWS_PROFILE` and leaving the chain otherwise untouched, so SSO, `credential_process`, and `role_arn` profiles are the SDK's job; a direct `-role-arn` assume-role flow remains deferred. `-expected-bucket-owner` guards against cross-account bucket confusion; `-request-payer requester` supports requester-pays buckets. Archive storage classes are handled at listing time (§5.1).

## 13. Deferred Work (staged, per review §12)

JSONL output (`-format jsonl`, exact JSON-escaped values); `-order completion|key`; `-plan` dry-run mode (objects surviving filters, bytes selected, storage classes, estimated GETs); magic-byte and `Content-Encoding` detection plus explicit format override; `--binary-files without-match|text|skip`; context lines (`-C`); S3 Inventory manifest as a Phase-1 source; delimiter-based per-cluster listing fan-out; version-aware scanning on versioned buckets; `-role-arn`.

## 14. Cost and Performance Notes (qualified, H-10)

Request and retrieval prices vary by region, storage class, retrieval tier, and transfer path; consult current AWS pricing rather than this document. Throughput depends on object-size distribution, compression ratio, client CPU, network placement relative to the bucket, S3 latency, worker count, and downstream output speed. The cost levers are structural — filters, `-l`, `-max-matches`, and early exit minimize GETs and bytes — and **time to first useful result** is the primary performance metric, which is what windowed smallest-first and streaming dispatch optimize. Any published throughput figures must state their benchmark conditions.

## 15. Testing Strategy and Acceptance Criteria

**Unit**: every filter and its boundary values; date/timezone semantics; ID extraction from key, matching line, and preceding context; step-log discovery (key without ID); no-trailing-newline and CRLF lines; invalid UTF-8; oversized lines (match-before-truncation, match-hidden-in-tail counted as partial); concatenated gzip members; corrupt gzip/bzip2/zip; ZIP entry and expansion budgets; fixed-string and case-insensitive modes; cancellation while listing, queued, and mid-read; broken-pipe handling; warning suppression.

**Integration** (against a stub S3): pagination boundaries (999/1000/1001 keys); continuation-token failure; object deleted and object modified between LIST and GET (`If-Match` path); per-object access denial and KMS denial; requester-pays; archive classes; throttling; truncated responses; mixed formats and storage classes.

**Concurrency**: race detector clean; no goroutine leaks; exactly one close per channel; counters consistent under load; writer failure cancels workers; SIGINT never deadlocks and always prints the summary; temp files always removed; response bodies always closed.

**Performance**: time-to-first-result (primary); peak RSS vs the §9 envelope; temp-disk peak; bytes saved by early exit; connection-reuse impact of early body close (measured, not assumed); window-size sensitivity.

**Acceptance criteria for the first production release** — adopted from review §11: criteria 1–10 and 12–16 verbatim (resource budgets enforced and tested; no unbounded archive expansion; oversized lines never silently terminate scanning; discovery works from key, line, and context; empty-prefix scans require approval; partial scans visible in counters and exit status; broken stdout cancels cleanly; SIGINT leaks nothing and prints the summary; LIST/GET changes detected; IAM/KMS/requester-pays/archive documented; exit codes stable and tested; ordering documented; binary output safe by default). Criterion 11 (JSONL) is re-scheduled to the release that ships `-format`.

---

## Appendix A — Review Finding Disposition

| Finding | Disposition | Where |
|---|---|---|
| C-01 app-ID discovery vs step logs | Fixed: key + line + context sources; `-discover-apps` termination rule | §8 |
| C-02 ZIP memory model | Fixed: temp file, streamed entries, expansion/entry budgets, ZIP semaphore | §6.3, §9 |
| C-03 listing efficiency overstated | Fixed: honest semantics + `-allow-whole-bucket-scan` guard | §3.2 |
| C-04 fixed scanner limit | Fixed: reader-based iterator, truncate-and-continue, counters | §6.4 |
| H-01 smallest-first stall | Fixed: bounded min-heap window | §5.2 |
| H-02 workers own stdout | Fixed: writer stage, EPIPE → cancel | §7.1 |
| H-03 early-close semantics | Fixed: explicit close chain; sentinel unwind; reuse cost measured in perf tests | §6, §15 |
| H-04 timeouts/retries | Adopted: object/overall timeouts; no retry after output; partial-scan marking. Per-request header timeout deferred | §6.1 |
| H-05 IAM/KMS/payer/archive | Adopted: flags, error classes, archive skip, IAM docs. `-role-arn` deferred | §5.1, §10, §12 |
| H-06 LIST/GET races | Fixed: `If-Match` with listed ETag | §6.1 |
| H-07 folder markers | Fixed: `/`-suffix AND zero bytes | §5.1 |
| H-08 exit codes | Fixed: 0/1/2/3/130, documented and tested | §10 |
| H-09 binary/control chars | Adopted: sanitize-by-default. `--binary-files` modes deferred | §7.2, §13 |
| H-10 absolute claims | Fixed: qualified language; TTFR as primary metric | §14 |
| M-01 filter order | Adopted | §5.1 |
| M-02 date semantics | Adopted: UTC midnight, inclusive/exclusive, LastModified only | §5.1 |
| M-03 flag validation | Adopted | §11 |
| M-04 ZIP match semantics | Adopted: per object; ZIP = one object | §6.3 |
| M-05 JSONL | Deferred | §13 |
| M-06 output ordering | Adopted: completion order documented; `-order` deferred | §7.1, §13 |
| M-07 format detection | Partially: case-insensitive now; encoding/magic bytes deferred | §6.2, §13 |
| M-08 warning bounds | Adopted: `-max-warnings` + suppression summary | §7.1 |
| M-09 plan mode | Deferred | §13 |
| M-10 fixed strings | Adopted: `-F`; RE2 semantics documented | §11 |
