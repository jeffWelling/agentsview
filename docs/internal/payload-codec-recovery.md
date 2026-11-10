# Recover individual encoded payload values

Status: implemented reader and recovery support for an experimental value
format. Archive compression is not enabled. No application query, schema,
trigger, archive version, background sync or installed application changes.

`internal/payloadcodec` provides a bounded codec that future archive storage
work can use. `cmd/payloadrecover` independently inspects or recovers one
explicitly supplied detached value. Neither component opens an archive,
loads application configuration, reparses a source or changes existing rows.
This work has no dependency on the metadata-normalization experiments.

## What is lossless?

The codec preserves every byte, including NUL, invalid UTF-8 and empty text.
`DecodeValue` accepts the values returned by ordinary `database/sql` scans:
NULL stays NULL, a Go string stays text, and an encoded BLOB becomes a string.
A string that starts with the envelope marker is still ordinary text. Other
BLOB formats fail; this does not read the earlier `AVTXT` prototype format.
No SQLite callback is installed, including on the ordinary-text path.

`Encode` returns the original string for values shorter than 1,024 bytes or
larger than 64 MiB. It also keeps text when encoding would not save at least
32 bytes and one eighth of the original size. Large existing text is never
truncated to make it eligible. These rules determine whether a value is worth
compressing; they do not make a whole-archive savings claim.

An encoded value is individually readable. Recovering it does not expand other
values, an archive file or a search index. Search, export, mirrors and ordinary
application reads retain their existing behavior because the application does
not write or read this representation yet.

## What is the detached format?

All lengths use unsigned little-endian integers. The 48-byte header contains
`AVPZ0001` (8 bytes), total decoded length (8 bytes) and the SHA-256 of the
original bytes (32 bytes). Each chunk has an 8-byte header containing its
32-bit decoded and encoded lengths, followed by exactly one Zstd frame.
Nonfinal chunks decode to exactly 65,536 bytes; the last uses the remaining
length. There is no dictionary, skippable frame, concatenated frame or trailing
data. An empty envelope has no chunks and hashes the empty byte string.
Ordinary encoding keeps empty values as TEXT.

This is an experimental detached-value format, not a declaration that any
SQLite column or archive capability now accepts it.

## What bounds decoding work?

Each encoded value contains independent Zstd frames holding at most 64 KiB of
original bytes. The header's decoded size must fit both the caller's byte limit
and the 64 MiB format limit before an output string is allocated. Each chunk
must have the expected decoded size, a bounded encoded size, one complete frame,
a matching frame-content size, a checksum and no external dictionary.

The decoder additionally uses a hard output-cap limit for every frame. Checking
only the outer length is insufficient: a one-byte envelope can contain a frame
that advertises 8 MiB, or a frame whose body contradicts both declared lengths.
Both cases are regression-tested. Full-value SHA-256 detects changed bytes or
chunk reordering, in addition to the per-frame Zstd checksums. These checks detect
corruption; they do not authenticate an untrusted author who can replace both
content and checksums.

A shared `Codec` owns one through four workers. Each worker uses one encoder and
one decoder, with no codec goroutines. Waiting for a worker honors cancellation;
ordinary NULL and TEXT reads never wait for a codec worker or copy text bytes.
Cancellation is checked before each chunk and before publishing decoded bytes.
One codec operation can finish its current 64 KiB chunk before cancellation is
observed. This is a byte bound, not a wall-clock deadline.

`DecodeValue` returns no partial result after an error. Its final string can be
as large as the admitted value; the input BLOB is already owned by its caller.
`Recover` streams decoded chunks with fixed codec scratch and no retained
whole-value output. A caller's reader or writer must itself make progress or
honor cancellation: a context cannot interrupt arbitrary blocked I/O. A generic
`Recover` destination may contain a prefix after failure. Durable callers must
stage output and publish it only after successful verification.

Concurrency is bounded per shared codec instance, not across independently
created instances. Future archive integration must share the boundary owner,
apply aggregate batch budgets separately, and admit a value before SQLite loads
its BLOB. This library cannot prevent an earlier database query from fetching
an oversized cell.

## How do I inspect or recover a detached value?

The developer command accepts a regular file containing exactly one envelope.
It does not take a database path, choose a row or scan SQLite files. Use a
separately extracted value from an isolated experiment:

```sh
go run ./cmd/payloadrecover --input detached-value.bin --inspect
go run ./cmd/payloadrecover --input detached-value.bin \
  --output recovered-value.txt --max-bytes 8388608
```

Inspection prints sizes, chunk count and `verified: false`. It validates framing
without decoding content or verifying its checksums. Recovery verifies all
chunks and the whole-value checksum, writes a private staging file in the output
directory, flushes and closes it, then publishes the complete file with an
exclusive hard link. Existing output files are never replaced. The input is
never modified. Ordinary errors, corruption and cancellation remove staging
output; an abrupt process or machine failure can leave an unreferenced staging
file. No archive or provider data is automatically discovered or logged.

The publication step requires hard-link support on the destination filesystem.
Unsupported filesystems fail safely. Power-loss durability of the directory
entry is not promised. This command creates a recovery artifact; it does not
install or reverse an archive format.

## What remains before archive enablement?

1. Define every compressed column and its storage-boundary owner. Preserve NULL,
   empty and classified dialogue semantics, opaque identities and tombstones.
   Cover direct writes, set-based imports, attached-database copies, maintenance,
   policy changes and mirrors, not only ordinary Go insertion methods.
2. Preserve native SQL handling for uncompressed rows. Admit compressed rows
   using small trusted size metadata before fetching payloads. Verify metadata
   and content together; do not slice a large BLOB merely to read its header.
3. Preserve search candidate selection, FTS text, ordering and exact results.
   Arbitrary regexes without useful literals may inspect every candidate; bound
   memory and cancellation without silently dropping matches. Measure CPU and
   allocation costs for search, readers, export and ongoing sync.
4. Implement explicit maintenance ownership and staged conversion and reversal.
   Preserve all identities, row revisions, publication counters, active/cold
   state and cursors. Verify logical equivalence and reject installation after
   the authoritative source changes. Exercise interruption and recovery before
   any live enablement.
5. Add a storage capability fence independent of parser versions, reject unknown
   formats before writes, preserve capability bits through every version writer
   and rebuild, and exclude already-open incompatible writers at installation.
   Bit `1 << 25` is reserved in the delivery plan; this branch sets no bit and
   teaches no archive opener to accept one.
6. Reverse from current encoded records, including writes after conversion.
   Validate supported platform packaging and recovery and measure realistic
   distributions before claiming a storage or latency benefit.

These are release and integration requirements, not a dependency on merging a
prerequisite PR. The independent draft can be evaluated with the other changes
on an aggregate branch while application enablement remains off.

## How is this verified?

The focused tests exercise literal byte preservation, NULL/text behavior,
corruption, unknown/concatenated frames, caller limits, the exact 64 MiB boundary,
blocked-worker cancellation, text bypass, cancellation between chunks and output
failures. The recovery command tests protect existing files, retain its input
and publish nothing on failed verification. The fuzz target checks that accepted
values obey their caller limit and the independently recomputed full checksum.

```sh
GOPROXY=off GOFLAGS=-trimpath CGO_ENABLED=1 go test \
  ./internal/payloadcodec ./cmd/payloadrecover
GOPROXY=off GOFLAGS=-trimpath CGO_ENABLED=1 go test -race \
  ./internal/payloadcodec ./cmd/payloadrecover
GOPROXY=off GOFLAGS=-trimpath go test ./internal/payloadcodec \
  -run '^$' -fuzz '^FuzzDecodeValue$' -fuzztime=5s -parallel=2
GOPROXY=off GOFLAGS=-trimpath go test ./internal/payloadcodec \
  -run '^$' -bench 'Benchmark(PayloadCodec|RejectForgedFrame)$' \
  -benchmem -benchtime=100ms -count=5
```

The benchmark compares plain text, bounded chunked decoding and recovery,
encoding, and a local recreation of the earlier whole-value codec algorithm.
The historical baseline uses a 1 MiB window and the default encoder level;
the new encoder uses a 64 KiB window and the fastest level. The baseline does
not simulate old SQLite callbacks, triggers or archive queries. Inputs are
synthetic 128 KiB and 1 MiB structured text; fixture creation is outside timing.
Neither these timings nor encoded-value byte counts measure archive-file space,
query latency, WAL costs, concurrent sync or production migration safety.

### Recorded synthetic results

Measured on 2026-11-09 with macOS arm64, Go 1.27.3 and the pinned
`github.com/klauspost/compress` 1.20.3. All 65 benchmark samples completed.
The tables report medians of five 100 ms samples, not latency percentiles.
They include benchmark assertion overhead and use a fixed comparison order.
The OS cache and machine background activity were not controlled.

| Original bytes | Chunked value bytes | Historical whole-value bytes |
| ---: | ---: | ---: |
| 131,072 | 4,470 | 4,540 |
| 1,048,576 | 33,983 | 25,352 |

| Operation | Input | Chunked microseconds/op | Historical microseconds/op | Chunked allocated bytes/op | Historical allocated bytes/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| Decode to string | 128 KiB | 119.462 | 57.913 | 131,504 | 271,098 |
| Decode to string | 1 MiB | 910.266 | 426.150 | 1,049,527 | 2,107,775 |
| Encode | 128 KiB | 157.291 | 104.136 | 7,880 | 204,824 |
| Encode | 1 MiB | 1,269.586 | 789.410 | 144,331 | 1,581,080 |

Streaming recovery to `io.Discard` took 111.324 microseconds and allocated
180 bytes per operation for 128 KiB; for 1 MiB it took 899.576 microseconds
and allocated 214 bytes. Ordinary TEXT decoded with zero allocations for both
sizes. Rejecting a forged one-byte envelope containing an 8 MiB frame took
0.618 microseconds and allocated 288 bytes per operation after worker warmup.

Allocated bytes per operation are not peak memory or resident memory. These
runs reuse codec workers and already hold the input; the streaming destination
discards output. A caller retaining the decoded string necessarily owns its
full size. Worker initialization and retained codec scratch are outside these
warm operation measurements.

The bounded format costs CPU and, for this 1 MiB fixture, more stored bytes
than the historical whole-value codec. It substantially reduces allocation
and adds enforceable output limits, incremental cancellation and detached
recovery. These results support evaluating that tradeoff; they do not justify
silently enabling compression on application reads.
