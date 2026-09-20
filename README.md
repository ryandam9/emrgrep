# emrgrep

A resource-budgeted concurrent scanner for EMR/YARN logs stored in S3.

`emrgrep` scans an S3 prefix and reports object keys and matching content
lines, grep-style. It rejects as much work as possible before any download
using listing metadata, streams `.gz`/`.bz2`/plain objects, handles `.zip`
under explicit disk and expansion budgets, and reports the YARN application
IDs behind matches — even when the ID is absent from the object key (step
logs). See [docs/design.md](docs/design.md) for the full design.

## Install

```
go install github.com/ryandam9/emrgrep/cmd/emrgrep@latest
```

or build from a checkout:

```
go build ./cmd/emrgrep
```

## Usage

```
emrgrep -bucket <bucket> -prefix <prefix> -grep <pattern> [flags]
```

Grep-style matches go to stdout; diagnostics, progress, and the final
summary go to stderr. Omit `-grep` for list-only mode (no downloads).
See [Examples](#examples) below for the common workflows — known
application, unknown-application discovery, time windows, progress
reporting, and scripting with exit codes.

### Flags

```
-bucket string                  required unless a cluster flag derives it
-prefix string                  required unless -allow-whole-bucket-scan or a cluster flag
-cluster-name string            EMR cluster name; resolves the RUNNING/WAITING cluster,
                                reads its S3 log destination, scopes the scan to it
-cluster-id string              EMR cluster ID (j-...); same scoping, any cluster state
-app-id string                  YARN application ID; lists only
                                .../containers/<app-id>/ (a server-side prefix — no key search)
-allow-whole-bucket-scan        explicit opt-in to empty-prefix enumeration
-key pattern                    object-key filter (client-side; cuts GETs, not LIST work)
-grep pattern                   content filter; omit for list-only mode (no downloads)
-category name                  named pattern from the config file's patterns mapping;
                                resolves to -grep so regexes never need typing
-cat                            no pattern: download and print entire logs line by line
                                (default without a pattern is listing file names only)
-download                       store the app's log files locally, as-is, under
                                ~/logscan/<yyyy-mm-dd>/<app-id>/ (requires -app-id)
-F                              -key/-grep are fixed strings, not regex
-i                              case-insensitive matching
-ext list                       e.g. .gz,.log (case-insensitive)
-after / -before time           RFC3339 or YYYY-MM-DD (UTC midnight);
                                after inclusive, before exclusive; vs LastModified
-max-size MiB                   compressed object cap (default 128; 0 = unlimited)
-max-zip-entries N              default 10000
-max-uncompressed-object-size MiB   cumulative ZIP expansion budget (default 512)
-max-line-size MiB              oversized-line truncation boundary (default 4)
-max-matches N                  per object; whole ZIP = one object (0 = unlimited);
                                needs -grep/-category/-cat
-max-total-matches N            stop the whole run after N matches (0 = unlimited);
                                needs -grep/-category/-cat
-l                              names only; first-hit exit; best-effort IDs
-discover-apps                  with -l: read on until an application ID is found
-smallest-first                 windowed approximate size ordering
-smallest-first-window N        default 5000
-workers N                      1-256 (default 16)
-zip-workers N                  1-workers (default 2)
-object-timeout / -overall-timeout   durations (0 = none)
-request-payer requester        requester-pays buckets
-expected-bucket-owner id       cross-account safety check
-sanitize-output bool           default true
-md                             write a Markdown report to ~/logscan/<yyyy-mm-dd>/<app-id>.md:
                                matched file names + matches grouped per file (needs -app-id, -grep)
-max-warnings N                 default 100
-region string                  AWS region override
-profile string                 named profile from ~/.aws/config and ~/.aws/credentials;
                                overrides $AWS_PROFILE (default: standard credential chain)
-progress duration              status line to stderr every interval, e.g. 2s (0 = off)
-verbose                        log listing pages and per-object scan starts (stderr)
-color auto|always|never        colorize results (default auto: only on a terminal)
-config file                    YAML defaults file (default: ~/.config/emrgrep/
                                config.yaml or .yml if present); CLI wins
-group                          key-as-heading output (default: on when stdout is a
                                terminal; -group=false forces classic flat lines)
```

Regex patterns use Go RE2 semantics: no lookaround, no backreferences.
Use `-F` for fixed strings.

### Examples

Each example shows the command, what it prints, and why.

#### Grep a prefix for a pattern

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/ -grep 'ERROR|Exception'
```

Matches print to stdout in grep style — key, line number, matched line —
in completion order (whichever object finishes first prints first):

```
s3://my-emr-logs/logs/j-1ABC2DEF3GHI4/steps/s-2TAB55V93BXKQ/stderr.gz:44: 26/07/20 09:14:02 ERROR Client: Application diagnostics message: User class threw exception
s3://my-emr-logs/logs/j-1ABC2DEF3GHI4/containers/application_1700000000000_0042/container_01_000001/stderr.gz:812: org.apache.spark.sql.AnalysisException: Table or view not found
```

On a terminal, the key is magenta, the line number green, and the text
that matched (`ERROR`, `Exception`) bold red. The run ends with a
summary on stderr:

```
---
emrgrep: completed in 42.318s
  listed 18211, survived filters 18195
  filtered out: 16 folder markers
  scanned to EOF 18195, stopped early by request 0, partially scanned 0
  matched objects 2, matched lines 2
  downloaded 461.5 MiB (483920114 compressed bytes)
```

Exit code 0: matches found, nothing skipped or failed.

#### Scope to a known application

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/ \
  -app-id application_1700000000000_0042 -grep 'ERROR' -i
```

EMR stores an application's container logs under the deterministic
path `<prefix>/<cluster-id>/containers/<app-id>/…`, so `-app-id`
appends `containers/<app-id>/` to the scan prefix and S3 lists **only
that application's objects** — no enumeration of the rest of the
cluster's keys, no client-side filtering. `-i` makes the content match
case-insensitive (`ERROR`, `error`, `Error`).

For layouts that don't follow this structure, `-key` remains available
as a client-side *object-key* filter: it still lists every key under
the prefix but downloads only the ones matching the pattern.

#### Name your regexes once, pick them by category

Patterns differ by the kind of application being scanned, and long
regexes are painful to retype and easy to mistype. Define them once in
the config file's `patterns` mapping — each name a regex or a list of
regexes that OR-combine — and select by name with `-category`:

```yaml
# ~/.config/emrgrep/config.yaml
cluster-name: hbase-prod
patterns:
  spark:
    - ERROR|Exception
    - Caused by
  oom: OutOfMemoryError|Container killed on request|exit code 137
  hbase: (ERROR|FATAL).*(regionserver|WAL)
```

```
emrgrep -app-id application_1700000000000_0042 -category spark
```

An unknown category is a fail-fast error that lists what the file
defines (`available: hbase, oom, spark`) — a typo never silently turns
a search into an unfiltered scan. `-category` with `-grep` on the
command line is a usage error; a CLI `-grep` overrides a file-provided
`category` standing default. Category patterns are always regular
expressions (`-F` does not apply); pattern regexes are validated when
the file is read, with file and line number.

#### No pattern: list the files, or dump the whole log

With no `-grep`/`-category` at all, the tool lists the application's
file names and downloads nothing — that is the default fallback. When
you want the actual log content, `-cat` downloads and prints entire
logs line by line, with the usual key/line prefixes, grouping, and
budgets:

```
emrgrep -app-id application_1700000000000_0042        # file names only
emrgrep -app-id application_1700000000000_0042 -cat   # full log content
```

`-cat` works as a config standing default too (`cat: true`): it
applies to patternless runs and steps aside whenever a pattern is in
play. Combining an explicit `-cat` with `-grep`/`-category` is a
usage error.

#### Download the full logs and keep them

When you have only an application ID and want the actual log files on
disk — no pattern, no reading in the terminal — `-download` stores
every object of the application locally:

```
emrgrep -app-id application_1700000000000_0042 -download
```

```
emrgrep: scanning s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/
emrgrep: downloading to ~/logscan/2026-08-05/application_1700000000000_0042
s3://…/container_01_000001/stderr.gz -> ~/logscan/2026-08-05/application_1700000000000_0042/container_01_000001/stderr.gz
…
```

Files land under `~/logscan/<yyyy-mm-dd>/<app-id>/` (same date
directory as `-md` reports), mirroring the S3 layout below the
application prefix. Objects are stored **as-is** — `.gz` stays
compressed (open with `zless`, or use `-cat` when you want the
decompressed content printed). The scan machinery still applies:
listing filters (`-ext`, `-after`/`-before`, `-max-size`), ETag
If-Match consistency, per-object error classification, and the final
summary (`downloaded N MiB`). A failed copy never leaves a partial
file behind. `-download` requires `-app-id` and cannot be combined
with `-grep`/`-category`/`-cat`, nor with `-l` or `-max-matches` —
it stores every object without reading any of them, so there is
nothing for a content-match selector or a match cap to act on.
`download: true` also works as a config standing default that applies
only to patternless app-scoped runs.

#### Save the run as a Markdown report

```
emrgrep -cluster-name hbase-prod -app-id application_1700000000000_0042 -grep 'ERROR' -md
```

`-md` writes `~/logscan/<yyyy-mm-dd>/<app-id>.md` when the run ends
(interrupted runs included) — reports group by run day, dated in local
time, and the Generated timestamp is local too. The report separates
matches per file: each matched file gets its own heading with only its
lines beneath it, regardless of whether the screen used grouped or flat
format, followed by the run summary:

````markdown
# emrgrep — application_1700000000000_0042

- **Generated**: 2026-08-04 20:30:00 AEST
- **Pattern**: `ERROR`
- **Scanned**: `s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/`
- **Files with matches**: 1

## Files with matches

```sh
s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/container_01_000001/stderr.gz
```

## Matches

### s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/container_01_000001/stderr.gz

```sh
      44: 26/07/20 09:14:02 ERROR Client: User class threw exception
     812: org.apache.spark.sql.AnalysisException: Table or view not found
```

## Run summary

```sh
emrgrep: scanning s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/
---
emrgrep: completed in 4.2s
  ...
```
````

`-md` requires `-app-id` (the report is named after the application)
and `-grep` (it records where a pattern was found). It works from the
config file too (`md: true`) for always-on reports — as a standing
default it applies only to runs that have both `-app-id` and `-grep`,
and is silently ignored otherwise, so the same config file still
serves list-only and cluster-wide scans.

Every match is held in memory until the report is written at the end
of the run, so the report lists at most 50 000 matches. A run that
reaches the cap says so on stderr and in the report's header, naming
the count it did not list — a truncated report can never be mistaken
for a complete one. If you hit it, narrow the pattern or bound the
scan itself with `-max-total-matches`.

`-md` with `-l` is legal but thin: `-l` reports object names rather
than lines, so the report lists the matched files and shows `(none)`
under Matches. Drop `-l` when you want the lines in the report.

#### Discover which application produced an error

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/ \
  -grep 'Table or view not found' -F -l -discover-apps -smallest-first
```

- `-F` — the pattern is a literal string, not a regex.
- `-l` — print only the names of matching objects, stop each object at
  its first match.
- `-discover-apps` — after a match, keep reading (without printing)
  until a YARN application ID appears, so step logs whose keys carry
  no ID still get attributed.
- `-smallest-first` — scan small objects first; step logs are tiny and
  usually contain the failure summary, so answers arrive in seconds.

stdout carries just the object names:

```
s3://my-emr-logs/logs/j-1ABC2DEF3GHI4/steps/s-2TAB55V93BXKQ/stderr.gz
```

and the summary turns the discovery into your next query:

```
  application IDs discovered (1):
    application_1700000000000_0042
```

#### Scan an EMR cluster by name — no bucket or prefix needed

```
emrgrep -cluster-name hbase-prod -app-id application_1700000000000_0042 -grep 'ERROR'
```

`-cluster-name` asks EMR for the RUNNING/WAITING cluster with that
name, reads its "Log destination in Amazon S3" (`LogUri`) from
`DescribeCluster`, and scopes the whole scan to
`s3://<log-bucket>/<log-prefix>/<cluster-id>/` — no `-bucket` or
`-prefix` required. With `-app-id` the scope narrows further to
`.../<cluster-id>/containers/<app-id>/`, so the whole run touches
nothing but that application's logs. The chosen scope is echoed to
stderr:

```
emrgrep: scanning s3://my-emr-logs/logs/j-1ABC2DEF3GHI4/containers/application_1700000000000_0042/
```

Details:

- **If several active clusters share the name, all of them are
  scanned**, newest first — the application under investigation may
  live on any of them. Each cluster resolves its own log destination,
  gets its own scope line and summary, and exit codes combine by
  severity (fatal > partial > matched > no matches). A cluster whose
  destination can't be resolved is reported and skipped without
  blocking the rest. `-max-total-matches` is a budget across all the
  clusters combined. Target exactly one with `-cluster-id`.
- `-cluster-id j-XXX` does the same scoping for any cluster state —
  terminated clusters' logs remain in S3 and their `LogUri` is still
  describable.
- Explicit `-bucket`/`-prefix` override the derived destination (the
  cluster ID is still appended), for logs replicated elsewhere.
- Requires `elasticmapreduce:ListClusters` (name resolution) and
  `elasticmapreduce:DescribeCluster` (log destination) IAM permissions.
  The EMR API is regional: the cluster is looked up in your profile's
  region, while the log bucket's own region is still auto-detected.

#### List objects without downloading anything

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/steps/
```

Omitting `-grep` is list-only mode: survivors of the metadata filters
print as `s3://bucket/key` lines and **no GetObject calls are made**.
The summary says `list-only mode: N objects reported, no downloads`.
Combine with `-key`, `-ext`, or a time window to preview exactly what
a content scan would download.

#### Restrict by time window

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/ \
  -after 2026-07-20 -before 2026-07-21 -grep 'ERROR'
```

Scans only objects whose S3 `LastModified` falls on 2026-07-20 (UTC):
date-only values mean UTC midnight, `-after` is inclusive, `-before`
exclusive. RFC3339 works for finer control:
`-after 2026-07-20T14:00:00+10:00`. Objects outside the window show up
as `filtered out: N outside time window` — filtered from listing
metadata, never downloaded.

#### Restrict by extension

```
emrgrep -bucket my-emr-logs -prefix logs/ -ext .gz,.log -grep 'OutOfMemoryError'
```

Only keys ending in `.gz` or `.log` (case-insensitive) are fetched;
everything else counts under `extension filtered`.

#### Cap the noise from repetitive logs

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/ \
  -grep 'Connection refused' -max-matches 3
```

At most 3 matching lines print per object, then the object is
abandoned (saving the remaining download). Such objects count as
`stopped early by request` in the summary — a deliberate cap, not an
error, so the exit code stays 0.

#### Stop the whole run after N matches

```
emrgrep -bucket my-emr-logs -prefix logs/ -grep 'OutOfMemoryError' -max-total-matches 20
```

Where `-max-matches` caps each object, `-max-total-matches` caps the
*run*: after exactly 20 matching lines have printed, listing stops, no
new objects are fetched, and in-flight objects wind down (counted as
`stopped early by request`, never as partial). The summary says so:

```
emrgrep: completed: -max-total-matches reached in 3.402s
```

Exit code 0 — a satisfied query, not an interruption. With `-l`, the
cap counts reported object names instead of lines. This is the "show
me a few examples and stop spending money" lever: combine with
`-smallest-first` to sample matches from many small objects cheaply.

#### Group matches under each object

When keys are deep (`a/b/c/d/e/.../app.log`), repeating the full path
on every matching line drowns the content. Grouped output prints each
object's key once as a heading with its matches indented below,
ripgrep-style — and it is the **default when stdout is a terminal**
(like `-color auto`). Piped or redirected output keeps the classic
single-line format for scripts; force either mode with `-group` /
`-group=false`.

```
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC/ -grep 'ERROR'
```

```
s3://my-emr-logs/logs/j-1ABC/containers/application_1700000000000_0042/container_01_000001/stderr.gz
      44: 26/07/20 09:14:02 ERROR Client: User class threw exception
     812: org.apache.spark.sql.AnalysisException: Table or view not found
   12581: 26/07/20 09:14:07 ERROR YarnScheduler: Lost executor 3

s3://my-emr-logs/logs/j-1ABC/steps/s-2TAB55V93BXKQ/stderr.gz
      12: ERROR: step failed
```

Line numbers print as a fixed 6-character right-aligned field (wider
only for files beyond 999,999 lines), so the matched text starts in
the same column within and across blocks.

ZIP entries keep their name per line (`  inner.log:3: text`). Each
object's block prints atomically when the object finishes, so blocks
from concurrent workers never interleave — the trade-off is that a
huge object's matches appear when it completes rather than line by
line (memory stays bounded: at most ~1 MiB is buffered per in-flight
object, after which a segment flushes early and the heading repeats).
`-group` requires `-grep` and doesn't combine with `-l`, whose output
is already one key per line.

#### Watch a long scan

A broad scan can be quiet for a long time: S3 returns listing pages of
~1,000 keys sequentially while workers download and scan concurrently,
and nothing prints until a match, a warning, or the final summary.
`-progress` makes the wait legible:

```
emrgrep -bucket my-bucket -allow-whole-bucket-scan -grep kyneton -i -progress 2s
```

A legend prints once before the first status line, so the columns are
self-explanatory right in the terminal:

```
emrgrep: progress columns:
    00:00:00  time since the run started (hh:mm:ss)
    listing   S3 is still enumerating keys; changes to "listed" when done
    keys      objects the S3 listing has found so far
    kept      objects that passed the filters and will be downloaded + scanned
    done      objects finished (fully scanned, stopped early, or failed)
    queue     objects still waiting or in flight (kept minus done)
    match     matching objects / matching lines found so far
    dl        compressed data downloaded from S3 so far
    err       objects that failed (classified in the final summary)
emrgrep: progress 00:00:10 listing  keys 42000     kept 3100      done 2905      queue 195     match 3/17         dl 1.2 GiB    err 0
emrgrep: progress 00:00:12 listing  keys 51000     kept 3810      done 3644      queue 166     match 5/29         dl 1.5 GiB    err 0
emrgrep: progress 00:00:14 listed   keys 58211     kept 4302      done 4302      queue 0       match 6/31         dl 1.7 GiB    err 0
```

Columns are fixed-width, so successive lines align and moving numbers
are easy to eyeball. `-verbose` goes further and logs each listing
page and each object as its scan starts:

```
emrgrep: listed page of 1000 keys (23000 so far, 812 survived filters)
emrgrep: scanning s3://my-bucket/notes/trip-log.txt (11.3 KiB)
```

Both write to stderr only — stdout stays pipeable — and neither counts
against `-max-warnings`.

#### Bound a scan in time

```
emrgrep -bucket my-emr-logs -prefix logs/ -grep 'ERROR' \
  -object-timeout 30s -overall-timeout 5m
```

`-object-timeout` abandons any single object after 30s (it becomes
`partially scanned` with a `timeout` error). `-overall-timeout` stops
the whole run at 5 minutes:

```
---
emrgrep: stopped: -overall-timeout exceeded in 5m0.007s
  ...
```

and exits 3 — a partial run, deliberately distinct from the 130 an
operator's Ctrl+C produces, so automation can tell them apart.

#### Force or forbid color

```
emrgrep ... -color always | less -R     # keep colors through a pager
emrgrep ... -color never > matches.txt  # explicit, though auto already
                                          # disables color for redirects
```

Default `auto` colors only when stdout is a terminal and honors
`NO_COLOR` and `TERM=dumb`; piped output is byte-identical to the
uncolored format.

#### Whole-bucket scans and cross-region buckets

```
emrgrep -bucket mellow.pictures -allow-whole-bucket-scan -grep kyneton -i
```

An empty prefix requires the explicit `-allow-whole-bucket-scan` flag,
because listing cost is proportional to the total key count. The
bucket's region is auto-detected (here `ap-southeast-4`) — no `-region`
needed even when your profile defaults elsewhere.

#### Pick credentials with a named profile

```
emrgrep -profile prod-emr -cluster-name hbase-prod -grep ERROR
```

`-profile` names a section of `~/.aws/config` and `~/.aws/credentials`
and is resolved by the AWS SDK itself, so static keys, `credential_process`,
`role_arn` chains, and SSO profiles all work. It takes precedence over
`AWS_PROFILE`; without it, credentials resolve through the standard
chain unchanged, so existing invocations and instance/task roles are
unaffected.

For an SSO profile, log in first — the profile alone is not a
credential:

```
aws sso login --profile prod-emr
emrgrep -profile prod-emr -cluster-name hbase-prod -grep ERROR
```

`-profile` and `-region` compose, and `-region` wins over the profile's
own region. That matters for the EMR lookup behind `-cluster-name` /
`-cluster-id`, which is regional and is *not* auto-detected: a cluster
outside the profile's default region needs `-region`.

```
emrgrep -profile prod-emr -region ap-southeast-2 \
  -cluster-name hbase-prod -grep ERROR
```

Bucket regions stay auto-detected either way. A profile name that does
not exist in the shared config fails before any scan work, exit 2:

```
emrgrep: loading AWS configuration: failed to get shared config profile, typo-emr
```

#### Requester-pays and cross-account safety

```
emrgrep -bucket shared-logs -prefix team/ -grep 'ERROR' \
  -request-payer requester -expected-bucket-owner 111122223333
```

`-request-payer requester` acknowledges you pay the request/transfer
costs on a requester-pays bucket. `-expected-bucket-owner` makes every
call fail unless the bucket belongs to that account — protection
against bucket-name squatting across accounts.

#### Keep standing defaults in a config file

When the cluster, patterns, and preferences rarely change and only
the application ID varies, put the constants in
`~/.config/emrgrep/config.yaml` (or `.yml`, or any file named with
`-config`):

```yaml
# ~/.config/emrgrep/config.yaml
profile: prod-emr
cluster-name: hbase-prod
i: true
progress: 2s
md: true
patterns:
  spark:
    - ERROR|Exception
    - Caused by
  oom: OutOfMemoryError|exit code 137
```

Then a scan is just the part that changes:

```
emrgrep -app-id application_1700000000000_0042 -category spark
```

Rules: YAML, with top-level keys named exactly as the CLI flags and a
`patterns` mapping for the named categories (a regex, or a list of
regexes that OR-combine; quote a regex if it starts with a
YAML-special character like `*` or `[`). **Any flag given on the
command line takes priority over the file** — `emrgrep -grep FATAL`
overrides the file for that run. Unknown keys and invalid values fail
fast with the file and line number. `config` and `version` cannot be
set from the file.

**Standing defaults are preferences, not demands.** A value that only
makes sense for some runs is dropped on the runs that cannot use it,
instead of failing them: `group`, `l`, `discover-apps`, `max-matches`,
`max-total-matches`, `md`, `cat`, and `download` all behave this way.
So a config file carrying `group: true` still leaves
`emrgrep -app-id application_..._0042` (a plain file listing) working,
and `l: true` does not turn every patternless run into a
`-l requires -grep` error. The same options typed on the command line
keep strict validation — asking for an impossible combination in the
command you just typed is a usage error worth reporting, not a
preference to ignore.

#### Use exit codes in scripts

```sh
emrgrep -bucket my-emr-logs -prefix logs/j-1ABC/ -grep 'ERROR' -F
case $? in
  0)   echo "errors found in logs" ;;
  1)   echo "clean: no matches" ;;
  3)   echo "matches may be incomplete: some objects failed or were partial" ;;
  2)   echo "the scan itself failed (usage/credentials/listing)" ;;
  130) echo "interrupted" ;;
esac
```

Exit 3 is the one to watch in automation: the scan *completed*, but at
least one object errored or was only partially scanned, so "no
matches" printed alongside exit 3 is not proof of absence.

### What filtering does and does not save

The prefix is the **only server-side filter** — and `-cluster-name`/
`-cluster-id` and `-app-id` work by *building* that prefix
(`.../<cluster-id>/containers/<app-id>/`), which is why they cut both
listing and download work. `-key`, `-ext`, `-max-size`,
and the time window run client-side against listing pages: they cut
`GetObject` calls, downloaded bytes, and decompression work — often by
orders of magnitude — but listing time remains proportional to the number
of keys under the prefix. Always scope the prefix to a cluster
(`.../j-<CLUSTER>/`) where you can. Whole-bucket enumeration requires
`-allow-whole-bucket-scan`.

### Output

Matches go to stdout in completion order (non-deterministic under
concurrency) and are flushed as they are found — the writer flushes
whenever its queue goes idle, so results appear promptly during a long
scan while bursts still batch efficiently. On a terminal, matches are
grouped under each object's key by default (see the `-group` example);
pipes and redirects get the flat single-line format below:

```
s3://bucket/key:lineNo: text
s3://bucket/key!zipEntry:lineNo: text
```

When stdout is a terminal, results are colored in GNU grep's palette:
object keys magenta, ZIP entry names cyan, line numbers green,
separators cyan, and every occurrence of the matched text within the
line bold red. `-color` controls this: `auto` (default) colors only on
a terminal and honors the `NO_COLOR` convention and `TERM=dumb`;
`always` forces color (e.g. into `less -R`); `never` disables it.
Piped or redirected output is byte-identical to the uncolored format
above. Colors are applied after sanitization, so escape sequences
inside scanned content can never masquerade as highlighting. When
stderr is a terminal, the summary is tinted too: the status line
green/yellow/red by outcome, and every count colored — neutral counts
cyan, good outcomes (scanned to EOF, matches) green, cautionary ones
(partial scans, filtered/oversized counts) yellow, errors red — with
zero values dimmed so the numbers that actually moved stand out.

Output is sanitized by default (`-sanitize-output=false` to disable):
C0/C1 control characters, DEL, invalid UTF-8 bytes, and deceptive Unicode
formatting (bidirectional overrides and isolates, zero-width characters,
line/paragraph separators, BOM) in content, keys, and ZIP entry names are
each replaced with `?` — tab is kept. Diagnostics go to stderr, capped by
`-max-warnings`. A summary is always printed to stderr — including after
interruption — separating objects scanned to EOF, stopped early by
request (`-l`, `-max-matches`), partially scanned, skipped (with
reasons), and failed, plus per-class error counts and discovered
application IDs.

### Exit codes

```
0    completed; one or more matches; no object errors or partial scans
1    completed; no matches
2    fatal usage, configuration, credential, or listing error
3    completed, but with >=1 object error or partially scanned object;
     also used when -overall-timeout expires or stdout fails mid-run
130  interrupted (SIGINT/SIGTERM); summary is still printed
```

`-h`/`-help` and `-version` exit 0. `-overall-timeout` deliberately maps
to 3, not 130: a configured deadline is a partial run, not an external
interruption, and automation can tell the two apart. An object cut off
by `-object-timeout` is counted as partially scanned with a `timeout`
error — never as fully scanned.

A *partially scanned* object means content may have been missed: an
oversized line was truncated, a ZIP budget aborted processing, or a stream
failed mid-download. Partial scans are never silent — they show in the
summary and in the exit status.

## Resource budgets

Every resource is bounded by an explicit, independently configurable
budget — worker count alone bounds none of the ones that matter:

| Resource | Bound by | Default |
|---|---|---|
| Download parallelism / open response bodies | `-workers` | 16 |
| RAM per worker | `-max-line-size` + fixed read buffers | ~4.3 MiB × workers |
| ZIP concurrency | `-zip-workers` | 2 |
| Temp disk | `zip-workers × -max-size` | 256 MiB |
| Archive expansion | `-max-uncompressed-object-size` (cumulative) | 512 MiB per ZIP |
| ZIP entry count | `-max-zip-entries` | 10 000 |
| Scheduler memory | `-smallest-first-window` descriptors | 5 000 |
| Queued output | bounded result channel | workers × 8 lines |
| stderr volume | `-max-warnings` | 100 |
| Wall clock | `-object-timeout`, `-overall-timeout` | off |

## AWS setup

Credentials resolve through the standard chain (environment variables,
`AWS_PROFILE`, instance/task roles). `-profile NAME` selects a named
profile explicitly and takes precedence over `AWS_PROFILE` — see
[Pick credentials with a named profile](#pick-credentials-with-a-named-profile).
Least-privilege IAM policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-emr-logs",
      "Condition": { "StringLike": { "s3:prefix": "logs/*" } }
    },
    {
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::my-emr-logs/logs/*"
    }
  ]
}
```

When the bucket uses SSE-KMS, also grant `kms:Decrypt` on the bucket key.
KMS denials are reported under the `accessDenied` error class.

- **Cross-region buckets work automatically.** When `-region` is not
  given, the bucket's region is discovered up front (via `HeadBucket`,
  whose response names the bucket region even on wrong-region or
  access-denied probes) and used for all requests — so a bucket in,
  say, `ap-southeast-4` scans correctly from a client configured for
  another region. An explicit `-region` always wins and skips the
  probe; if a region mismatch still surfaces, the listing error says
  to rerun with `-region <bucket-region>`.
- `-expected-bucket-owner <account-id>` guards against cross-account
  bucket confusion.
- `-request-payer requester` supports requester-pays buckets.
- Archive handling is restore-aware: listing requests the
  `RestoreStatus` attribute, so a Glacier/Deep Archive object with a
  readable restored copy **is scanned**; unrestored (or
  restore-in-progress) objects are skipped at listing time and counted
  (`archivedSkipped`). Glacier Instant Retrieval objects are scanned
  normally. Objects in the Intelligent-Tiering archive tiers keep a
  readable-looking storage class; their `InvalidObjectState` GET
  failures are classified as `archivedUnavailable`.
- Error classes in the summary: `accessDenied` (including KMS),
  `notFound`, `changedAfterListing`, `corrupt`, `timeout`,
  `archivedUnavailable`, `throttled` (SlowDown et al.), `other`.

## Behavior guarantees

- **LIST/GET consistency**: every `GetObject` carries `If-Match` with the
  listed ETag. An object that changed in between is counted
  (`changedAfterListing`) and skipped — metadata and content are never
  mixed across versions.
- **No retry after output**: once scanning has begun, a mid-stream failure
  marks the object partially scanned and is never retried, so matches are
  never duplicated.
- **Folder markers**: only keys ending in `/` **and** zero bytes are
  skipped as markers; non-empty `/`-suffixed objects are scanned.
- **Broken pipes**: if stdout fails (`| head`, closed redirect), the run
  cancels promptly and reports interruption instead of a misleading
  success summary.

## Application-ID attribution

Attribution is per match, not per object: at each matching line the ID
is resolved from (in priority order) the object key, the matching line
itself, or the most recent preceding ID in the same stream. One object
can therefore contribute several application IDs, and an unrelated ID
appearing after a match is never attributed to it. Each ZIP entry gets
its own context, so IDs cannot leak between entries (the outer key's ID,
when present, applies to all entries). With `-l -discover-apps`, reading
continues silently after the first match until an ID appears or the
object ends.

## Development

```
make            # fmt + vet + race tests + build + install
go test -race ./...
```

CI (GitHub Actions) runs gofmt, `go mod tidy` drift check, `go vet`,
the race-enabled test suite, a build, and `govulncheck` on every push
and pull request.

The test suite covers every filter boundary, oversized-line and exact
line-limit behavior, truncated streams, ZIP budgets (including the
exact-expansion boundary), gzip multistream, per-match application-ID
attribution and ZIP-entry isolation, object/overall timeout accounting,
restore-aware archive filtering, pagination boundaries, LIST/GET races
(`If-Match`), error classification, warning suppression, cancellation,
and broken-pipe handling — against a stub S3, with the race detector
clean, plus fuzz targets for the line iterator, sanitizer, and ID
extraction.
