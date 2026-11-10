# Measure conversation-history session references

Status: test-only storage experiment. No archive format, application query,
startup migration or installed application changes.

Conversation export retains history after messages disappear from a live
session. The current history table and several indexes repeat the full session
identifier for each retained message. This experiment lets maintainers measure
the space saved by storing an integer reference instead, together with the
cost of resolving that reference during reads.

The benchmark lives in
[`conversation_storage_layout_bench_test.go`](../../internal/db/conversation_storage_layout_bench_test.go).
It uses generated data in temporary databases. It never accepts an existing
archive path. Its preservation checks validate the experimental copy and
queries; they do not establish production migration safety.

## What is compared?

The baseline copies the current conversation-message table and index
definitions into benchmark-owned tables. The candidate replaces only
`session_id` with an integer reference. A permanent mapping table retains the
exact text identifier, including identifiers belonging only to removed history.
A logical read view joins the mapping back to the message rows. The other
message fields, including both digest projections, remain unchanged.
The session-change journal keeps its text keys in both layouts; it already
stores only one row per session.

The mapping has no dependency on live `sessions` rows. Its table and unique
text index count toward the candidate's storage. This is lossless identifier
normalization, not payload compression or deletion of old records. Reads
resolve only the requested records; they do not reconstruct the whole archive.

The experiment compares live-page reads, message lookup, source matching and
revision-feed reads. Physical-table mutation measurements cover metadata
updates and unchanged updates. These are isolated SQL operations, not complete
application requests or sync transactions.

## How do I reproduce it?

Use the repository's pinned Go toolchain and ordinary SQLite build settings:

```sh
CGO_ENABLED=1 GOFLAGS=-trimpath go test -tags fts5 ./internal/db \
  -run '^TestConversationStorageLayout' -count=1
CGO_ENABLED=1 GOFLAGS=-trimpath go test -tags fts5 ./internal/db -run '^$' \
  -bench '^BenchmarkConversationStorageLayout$' -benchmem -count=5
AGENTSVIEW_BENCH_CONVERSATION_ROWS=1000000 CGO_ENABLED=1 GOFLAGS=-trimpath \
  go test -tags fts5 ./internal/db -run '^$' \
  -bench '^BenchmarkConversationStorageLayout$' -benchmem -count=5
```

The default fixture has 100,000 history rows, with 100 rows per session and
20% marked removed. The row-count override accepts 1,000 through 2,000,000.
Increasing it adds sessions; it does not lengthen each session's history.
Identifiers are synthetic strings and native source identifiers are unique.
The source is generated once; the candidate is copied from it in bounded
batches before timing queries.
Fixture generation, copying, compaction and equality checks are excluded from
query timings. Keep other heavy workloads idle when comparing results.
Add `-v` to retain the logged query plans alongside the benchmark output.
Read timings include generic result materialization into Go values. They do
not measure the application's typed response assembly. Repeated queries use
one fixed target, so these are warm microbenchmarks rather than cold-device
or multi-user latency measurements.

The reported database-byte metric includes the same empty archive schema in
both layouts. It includes the candidate mapping and indexes, but excludes
temporary copy space and transient journal files. It is a compacted synthetic
database size, not the size reduction of an existing user's archive.

## What did the recorded run show?

Measured on 2026-11-09 against baseline
`4bf12972e19486a59788692b0c832fff0002668e`, using macOS arm64, Go 1.27.3,
the pinned SQLite driver 1.14.52 and the commands above. Both sizes completed
all 60 samples without assertion failures. The query plans retained indexed
lookups and revision-range scans; the candidate added mapping lookups.

| History rows | Text-key fixture bytes | Integer-reference fixture bytes | Reduction |
| --- | ---: | ---: | ---: |
| 100,000 | 54,374,400 | 35,033,088 | 35.6% |
| 1,000,000 | 532,017,152 | 339,550,208 | 36.2% |

The following values are medians of five Go benchmark `ns/op` samples,
converted to microseconds per operation. They are not request-latency
percentiles. Each timed update changes a same-length digest; the row is reset
outside timing before each sample so repeated samples cannot start with an
accidental no-op.

| Operation | 100k text | 100k refs | 1M text | 1M refs |
| --- | ---: | ---: | ---: | ---: |
| Live page | 92.205 | 92.976 | 93.227 | 93.240 |
| Message ID | 7.706 | 7.911 | 7.736 | 8.086 |
| Source match | 6.601 | 6.767 | 6.701 | 6.837 |
| Revision feed | 163.652 | 174.421 | 163.944 | 175.115 |
| Physical update | 33.544 | 32.513 | 33.396 | 32.934 |
| Physical no-op update | 7.153 | 6.694 | 7.337 | 6.707 |

At one million rows, revision-feed cost increased 6.8% and message lookup
increased 4.5%. Live-page cost was approximately unchanged in this profile.
The physical-write measurements exclude mapping lookup/creation and the rest
of an application transaction. Runs used a fixed layout order and an
uncontrolled OS cache; small differences are not evidence of statistical
significance. These results justify further investigation, not automatic
enablement or a whole-archive saving claim.

The focused preservation checks, formatting and Go vet passed. The broader
database suite had one unrelated failing case:
`TestFrictionDayBounds/America/Havana/2026-11-01`. It reproduced with the new
file excluded on the unchanged baseline: host time-zone data disagreed with
the fixture's expected UTC offset. That case passed when using the pinned
Go toolchain's time-zone database. No time-zone expectations were changed.

## What must a production proposal still prove?

Space savings alone are insufficient. The compatibility view adds work to
reads, and the application has direct writes and schema probes that cannot
automatically use the new representation. Before enabling this layout:

1. Measure realistic history distributions, short and long identifiers,
   archives with few messages per session, cold reads and concurrent sync.
   Record query plans and latency as well as space. A mapping can cost more
   than it saves when identifiers are short or rarely repeated.
2. Preserve all logical message fields, opaque identifiers, retained deletion
   history, row revisions, publication counters, archive/database identities,
   initialization state and existing export cursors. Keep cold archives cold.
   The ordinary archive rebuild helpers intentionally create new revisions;
   they cannot serve as representation-only migration helpers.
3. Update every reader, writer, trigger and schema probe. Queries through the
   logical view must still use revision, live-ordinal and source indexes.
   Resolve session references per batch and write physical tables directly;
   do not rely on an `INSTEAD OF` trigger to emulate existing UPSERTs.
4. Perform explicit staged conversion under maintenance ownership. Copy in
   bounded batches and verify logical equivalence before installation. The
   existing compaction verifier requires an unchanged schema and cannot
   validate this transformed candidate unchanged. Never install a candidate
   after its authoritative source has changed.
5. Demonstrate interruption, restart, insufficient-space handling and recovery
   using isolated archives. Add a storage-format fence independent of parser
   versions, reject unknown formats, and prevent incompatible writers from
   remaining open across installation.
6. Reverse from the current normalized records, including writes made after
   conversion. Restoring a pre-conversion backup would lose those writes.
   Verify ordinary export, resync, archive copies, deletion, content-policy
   changes and mirror behavior in both representations.

These requirements belong to the production feature and its focused review.
This benchmark supplies repeatable evidence for deciding whether that work is
worth its space, performance and compatibility costs.
