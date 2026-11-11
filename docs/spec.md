# Synthetic OTT Origin with On-Demand SCTE-35

Nov 6, 2026 · @gbchd

## Overview

A single Go service that hosts configurable live CMAF streams generated from a clock, serves them as HLS and/or DASH with an adaptive bitrate ladder, and inserts SCTE-35 ad breaks when you call its API. Every segment is a pure function of its number and the break schedule, so the origin holds no media state and can rebuild any segment at any time.

**What the viewer sees and hears**

- A picture at each rendition's resolution (25 fps by default): wall clock (HH:MM:SS) in the centre, global frame number top-left, "TIC" or "TOC" under the clock.
- A short beep at the start of each second: a high-pitched tic on odd seconds, a lower toc on even seconds.
- During an ad break, the same picture and audio on a red background instead of the normal one.

**Key decisions**

| Area | Decision |
| --- | --- |
| Container | CMAF fMP4, demuxed video and audio tracks, one set of segments for both protocols |
| Protocols | HLS (multivariant + media playlists) and/or DASH per stream (dynamic MPD, `SegmentTemplate` with `$Number$`) |
| Segments | Per stream (default 2 s), one closed GOP each, so every segment starts with an IDR, aligned across renditions |
| Ad signalling | Configurable per stream: DATERANGE, CUE-OUT/IN, OATCLS or Adobe tags in HLS; EventStream (binary or XML), in-band emsg v1 and optional multi-period in DASH |
| Ad boundaries | Snapped to segment boundaries; duration rounded up to whole segments |
| Encoding | ffmpeg for v1 (decided): one run per segment produces every rendition; the pure-Go encoder stays a later option behind the same `Encoder` interface |
| Library stack | mp4ff for boxes, scte35-go for splice\_info\_section, go-aac for audio in the pure-Go path |

Streams are created, configured, started and stopped through the API (see Streams); config lives in memory in v1, behind a repository interface ready for a database. Out of scope: DRM, low-latency (LL-HLS / chunked CMAF), and actual ad content. The red segments *are* the ad.

## Architecture

Three layers: control holds stream config, epochs and breaks; generation produces the media and the cue bytes; delivery packages, caches and serves them. The repositories are the only state that would need sharing between replicas, which is why they are highlighted.

&#91;embedded content: origin architecture · 3 layers, 11 components\]

A segment request flows bottom-up: the HTTP server asks the cache, which the producer has usually filled seconds earlier; on a miss, the packager builds the segment from the epoch runtime, the breaks, the encoder and the audio loop.

## Streams

The origin hosts any number of independent streams. Each one has an editable **spec** (formats, rendition ladder, audio, ad settings) and a read-only **status** (stopped, running or ended). Starting a stream freezes a snapshot of its spec into an **epoch**; everything served belongs to that epoch, so editing a running stream never changes what players see until the next start. Two settings are the exception and apply at once, because they never change media bytes: the live window and fault rules.

**Stream resource**

```text
Stream {
  id          "demo"                          // slug, immutable, used in URLs
  name        "Demo channel"
  spec {
    formats         ["hls", "dash"]           // at least one
    segmentSeconds  2                         // decimals allowed, e.g. 1.92 (see Timeline model)
    gopSeconds      2                         // keyframe interval; must divide the segment
    fps             25
    timezone        "UTC"                     // for the burnt-in clock
    overlay         { renditionLabel: true }  // burn "720p · 3000 kbit/s" into each rendition
    video [                                   // the ladder, 1-12 renditions, codec per rendition
      { id: "1080p-av1",  codec: "av1",  width: 1920, height: 1080, bitrateKbps: 3000 },
      { id: "1080p-hevc", codec: "hevc", width: 1920, height: 1080, bitrateKbps: 3500 },
      { id: "1080p",      codec: "h264", width: 1920, height: 1080, bitrateKbps: 5000 },
      { id: "720p",       codec: "h264", width: 1280, height: 720,  bitrateKbps: 3000 },
      { id: "360p",       codec: "h264", width: 640,  height: 360,  bitrateKbps: 800, profile: "main" }
    ]
    audio [                                   // 1-8 renditions, 48 kHz
      { id: "en",    lang: "en", name: "English",     channels: 2, bitrateKbps: 128, default: true },
      { id: "fr",    lang: "fr", name: "Français",    channels: 2, bitrateKbps: 128, ticHz: 1500, tocHz: 900 },
      { id: "en-51", lang: "en", name: "English 5.1", channels: 6, bitrateKbps: 384, codec: "ac3" }
    ]
    subtitles [                               // 0-8 text tracks (see Subtitles and captions)
      { id: "sub-en", lang: "en", name: "English",  format: "webvtt", default: true },
      { id: "sub-fr", lang: "fr", name: "Français", format: "imsc1" }
    ]
    captions        { cea608: { channel: "CC1", lang: "en" } }   // optional, inside the video
    dash    { addressing: "timeline-time" }   // see Manifests, DASH segment addressing
    ads {
      leadSeconds       8
      command           "splice_insert"       // splice_insert | time_signal
      segmentationType  "provider_placement_opportunity"   // time_signal only
      hls               ["daterange", "cueout"]            // daterange | cueout | oatcls | adobe
      dash              ["eventstream-bin", "inband"]      // eventstream-bin | eventstream-xml | inband
      dashMultiPeriod   false
      recurring         { everySeconds: 300, durationSeconds: 30, offsetSeconds: 60 }   // optional
    }
    faults  [                                 // optional (see Fault injection)
      { match: { kind: "segment", renditions: ["720p"] }, percent: 5, action: { status: 503 } }
    ]
    window  { dvrSeconds: 30, retainSeconds: 300 }   // live window; applies immediately, even while running
  }
  status {                                    // read-only
    state           "running"                 // stopped | running | ended
    epoch           { number: 4, ast: "2026-11-06T18:40:00Z", endSegment: null, spec: <snapshot> }
    pendingChanges  true                      // spec differs from the running snapshot
    audioAligned    false                     // true when every audio segment has the same length
  }
  version     12                              // optimistic concurrency, sent as ETag
  createdAt, updatedAt
}
```

All audio renditions above sit in the default group `main`. Adding a low-bitrate tier means a second group (for example `low`) holding a cheaper copy of each of these three tracks.

**Validation**

| Field | Rule |
| --- | --- |
| `id` | `[a-z0-9-]{1,40}`, unique |
| `formats` | Non-empty subset of `hls`, `dash`; a format not listed returns 404 on its manifests |
| `segmentSeconds` | 0.5-10 s, and segmentSeconds × fps must be a whole number of frames (1.92 at 25 fps = 48 frames: valid; 1.92 at 30 fps: refused) |
| `gopSeconds` | Defaults to segmentSeconds; must be a whole number of frames and divide the segment evenly, so every segment still starts with an IDR |
| `fps` | One of 24, 25, 30, 50, 60 (all divide 90 000 evenly) |
| `overlay.renditionLabel` | Boolean, default true; the label includes the codec |
| `video` | 1-12 renditions; even width and height, at most 1920×1080, 100-20 000 kbit/s; bitrates unique within each codec; stored grouped by codec, highest bitrate first |
| `video[].codec` | `h264` (default), `hevc` or `av1` |
| `video[].profile` | `h264`: `baseline`, `main` or `high` (default); `hevc`: `main` (default) or `main10`; `av1`: `main`, with `bitDepth` 8 (default) or 10. Level is derived from size, fps and bitrate |
| `audio` | 1-8 renditions; `lang` is a BCP 47 tag; `channels` 1, 2 or 6; `codec` `aac` (default), `ac3`, `eac3` or `opus`; bitrate 32-320 kbit/s for AAC and Opus, 192-640 for AC-3, 96-640 for E-AC-3; `group` defaults to `main` |
| `audio` per group | Exactly one `default: true`; HLS needs every group to offer the same (lang, channels, codec) tracks, only bitrates differ |
| `subtitles` | 0-8 tracks; `format` `webvtt` or `imsc1`; at most one `default: true` |
| `captions.cea608` | Optional; `channel` CC1-CC4, `lang` a BCP 47 tag |
| Rendition ids | Unique across video, audio and subtitles, since all share the `/live/{stream}/{rendition}/` namespace |
| `dash.addressing` | `number`, `timeline-time` (default) or `timeline-number`; anything else is refused, because it cannot work for live (see Manifests) |
| `ads.leadSeconds` | At least 2 segments' worth, at most 60 s |
| `ads.command` | `splice_insert` (default) or `time_signal`; `segmentationType` is required with `time_signal` and refused otherwise |
| `ads.hls` | Non-empty subset of `daterange`, `cueout`, `oatcls`, `adobe` when HLS is on; `cueout` and `oatcls` are exclusive, since OATCLS already writes CUE-OUT/IN |
| `ads.dash` | Non-empty subset of `eventstream-bin`, `eventstream-xml`, `inband` when DASH is on |
| `ads.recurring` | Optional; durationSeconds ≥ 1 segment; everySeconds ≥ durationSeconds + 2 segments; offsetSeconds ≥ leadSeconds |
| `faults` | 0-16 rules, each validated as in Fault injection |
| `window` | dvrSeconds from 3 segments to 24 h (default 30 s); retainSeconds from dvrSeconds to 24 h (default 300 s); both take effect immediately on a running stream |
| At `start` | Every video and audio codec must be available in the local ffmpeg build, otherwise 422 naming the missing encoder |

**Audio renditions**

Each audio rendition is a separate CMAF track with its own init segment and audio loop. Two independent axes decide how players see them:

- **Alternate tracks** differ by language, channel layout or codec. The player shows them in its audio menu.
- **Bitrate tiers** are the same track at different bitrates, put in different `group`s. Players pick one by bandwidth, as with video.

| Concept | HLS | DASH |
| --- | --- | --- |
| Alternate track | One `EXT-X-MEDIA:TYPE=AUDIO` per track in a group, with `LANGUAGE`, `NAME`, `CHANNELS`, `DEFAULT`/`AUTOSELECT` | One `AdaptationSet` per (lang, channels, codec), with `lang`, `Label`, `Role` (`main` for the default, `alternate` otherwise) and `AudioChannelConfiguration` |
| Bitrate tier | One `GROUP-ID` per group; each video rendition gets one `EXT-X-STREAM-INF` per audio group, so the ladder is video × groups | One `Representation` per bitrate inside the matching AdaptationSet; `group` is ignored |

The tracks are synthetic, so they need a way to be told apart by ear. Each rendition can set `tocHz` (Bip, even seconds) and `ticHz` (Bop, odd seconds). Defaults step up by language (1 500/480 Hz Bip/Bop for the first language, 2 000/640 Hz for the second, 2 500/800 Hz for the third, and so on), so switching tracks is audible. On 5.1 the beep moves to the next channel every second (L, R, C, LFE as a 60 Hz tone, Ls, Rs), then sounds on all channels for two seconds, an 8 s cycle that checks channel mapping on the receiving side.

**API**

| Method and path | Effect | Errors |
| --- | --- | --- |
| `POST /api/streams` | Create, state `stopped` | 409 id taken, 422 invalid |
| `GET /api/streams` | List with status |  |
| `GET /api/streams/{id}` | One stream, `ETag: "<version>"` | 404 |
| `PUT /api/streams/{id}` | Replace the spec; applies on next start | 428 without `If-Match`, 412 stale version, 422 invalid |
| `DELETE /api/streams/{id}` | Remove stream and its breaks | 409 while running |
| `POST /api/streams/{id}/start` | New epoch: snapshot spec, AST = now rounded up to an even second (or `{"ast": ...}`) | 409 if running |
| `POST /api/streams/{id}/stop` | End the epoch at the first unpublished segment boundary; state `ended` | 409 unless running |
| `GET /api/streams/{id}/status` | Live edge, current segment, active break |  |
| `/api/streams/{id}/breaks...` | The break endpoints from the SCTE-35 API section, scoped to the stream and its current epoch | 409 unless running |

**Storage behind interfaces**

```text
interface StreamRepository:
    create(stream)                      -> ErrExists
    get(id)                             -> stream | ErrNotFound
    list()                              -> []stream
    update(stream, expectedVersion)     -> ErrVersionMismatch     // compare-and-swap, version++
    delete(id, expectedVersion)

interface BreakRepository:
    add(streamId, epoch, break)         -> ErrOverlap
    list(streamId, epoch, fromSeg, toSeg) -> []break
    update(streamId, epoch, break)
```

- **v1, in memory:** maps guarded by a `sync.RWMutex`, deep copies in and out so callers never share pointers with the store, version bumped on every write.
- **Later, a database:** tables `streams(id, spec jsonb, status jsonb, version, created_at, updated_at)` and `breaks(stream_id, epoch, id, start_seg, end_seg, ...)`. `update` becomes `UPDATE ... WHERE id = $1 AND version = $2`. Breaks are keyed by epoch, so a restart starts with a clean schedule.
- **Why it scales out cleanly:** media is a pure function of (epoch snapshot, segment number, breaks), so any replica builds identical bytes, as long as every replica runs the same pinned ffmpeg build. Only config, status and breaks need sharing; caches stay per replica. Replicas learn about changes by re-reading with a 1 s TTL, or via change notifications (for example Postgres `LISTEN/NOTIFY`).

**Runtime, separate from config**

A `StreamManager` owns one in-process `Runtime` per running or ended stream: the epoch snapshot, init segments per rendition, the encoded audio loop, the segment cache and a producer goroutine. On boot it loads every stream in state `running` from the repository and rebuilds its runtime; because AST is stored, the stream continues at exactly the right segment. Nothing in `Runtime` is ever persisted.

**Lifecycle**

&#91;embedded content: stream lifecycle · 3 states\]

Stop ends the stream at the frozen horizon, so segments already announced or pre-encoded are still delivered; players then see a clean end of stream.

## Encoder choice

**Decision: v1 uses ffmpeg (Option A).** There is no general-purpose pure-Go H.264 encoder, but there is a pure-Go AAC encoder, and this picture is simple enough for a small purpose-built H.264 writer (Option B). That stays a later option behind the same `Encoder` interface, with ffmpeg as its reference in tests.

**What exists today**

| Need | Library | Pure Go? | Notes |
| --- | --- | --- | --- |
| H.264 encode | [x264-go](https://pkg.go.dev/github.com/gen2brain/x264-go) | No (cgo) | Wraps libx264; GPL-2.0 |
| H.264 encode | [openh264-go](https://pkg.go.dev/github.com/Azunyan1111/openh264-go) | No (cgo) | Static OpenH264 bindings; MIT |
| H.264 encode | [lys-lib](https://pkg.go.dev/github.com/stubbedev/lys-lib) | Partly | No cgo, but encodes through VA-API / Vulkan hardware loaded at run time |
| H.264 bitstream | lys-lib `nal` package | Yes | NAL writer, Exp-Golomb, emulation prevention, parameter sets: a useful base for a custom encoder |
| AAC-LC encode | [go-aac](https://pkg.go.dev/github.com/tphakala/go-aac) | Yes | Port of FFmpeg's native AAC encoder; LGPL-2.1 |
| fMP4 / CMAF boxes | [mp4ff](https://pkg.go.dev/github.com/Eyevinn/mp4ff) | Yes | Init and media segments, emsg, AVC SPS/PPS parsing; MIT |
| SCTE-35 | [scte35-go](https://github.com/comcast/scte35-go) | Yes | Encode/decode up to SCTE 35 2022b, base64 and hex output |

**Option A: ffmpeg per segment (v1)**

- **One run per segment, all renditions.** For segment N, one ffmpeg process draws the frame once at the top rendition's size (`lavfi` colour source, `drawtext` for clock, frame number and TIC/TOC), then `split`s and `scale`s it to every rendition and encodes each with its own bitrate.
- **Outputs.** One fragmented MP4 per rendition, written to a per-segment tmpfs directory (`/dev/shm/<stream>/<epoch>/<N>/<rendition>.mp4`) or to extra pipes (`exec.Cmd.ExtraFiles`); Go parses each with mp4ff, rewrites `tfdt` and deletes the directory.
- **x264 settings per rendition.** `-b:v` = bitrate, `-maxrate` = bitrate, `-bufsize` = 2 × bitrate, profile and level from the spec, `-g` and `-keyint_min` = fps × gopSeconds, `-sc_threshold 0`, `-bf 0`, `-preset veryfast`. No B-frames keeps decode time = presentation time.
- **One init segment per rendition.** Fixed settings give identical SPS/PPS on every run, so the init segment is captured from the first run of the epoch and reused; normal and red segments share it.
- **Pre-generated, not on request.** A producer goroutine per running stream encodes segment N as soon as it is frozen (see Timeline model), several seconds before it is published, so players never wait for ffmpeg. A cache miss for an older segment in the window falls back to on-demand generation, deduplicated with `singleflight`.
- **Bounded CPU.** A global semaphore caps concurrent ffmpeg processes (default: number of CPUs ÷ 2). A producer that falls behind logs it and exposes a `segments_late_total` metric.
- **Audio is encoded once per epoch,** not per segment (AAC priming would click at every boundary): an 8 s loop with ffmpeg's native AAC encoder at the spec's bitrate and channel count, sliced per segment (see Content generation).
- **Determinism.** x264 output is deterministic for a given build and settings, so the Docker image pins one ffmpeg version; every replica then produces byte-identical segments.

**Option B: pure-Go "tile" encoder (v2, single binary)**

The frame is a flat background plus a few changing glyphs, which H.264 can carry with three macroblock (MB) kinds and no motion search, no transforms on changing content:

1. **I\_PCM** MBs carry raw pixels (384 bytes per 16×16 MB in 4:2:0). They have no prediction or entropy context, so each glyph cell can be pre-rendered once into a cache of MB payloads.
2. **P\_Skip** MBs copy the previous frame. With no motion vectors anywhere in the stream, the predicted vector is always zero, so skipped areas stay exactly as they were.
3. **I\_16x16 DC, zero residual** MBs fill a flat background in the IDR at a few bits each, predicting the colour from neighbours. Top-left MB and any MB right of or below a glyph are sent as I\_PCM to seed the right colour.

- Deblocking is switched off in the slice header, so cached glyph pixels decode exactly.
- Glyphs sit in MB-aligned cells (for example 32×48 for the frame number, 64×96 for the clock) and are rendered with `golang.org/x/image/font` and the bundled Go Mono font.
- A P frame re-sends only the cells whose character changed: usually the last one or two frame-number digits, about 2–5 KB per frame, a few hundred kbit/s.
- An IDR is mostly cheap background MBs plus every glyph cell as I\_PCM, roughly 100–200 KB, once per 2 s segment.
- Audio uses go-aac to encode the 8 s loop at startup; nothing else needs a codec.
- Cost: roughly 600–900 lines of careful bitstream code, validated against ffmpeg decoding (PSNR = infinite on glyph areas).

**Trade-off**

|  | ffmpeg (A) | Tile encoder (B) |
| --- | --- | --- |
| Deployment | Go binary + ffmpeg (Docker image) | One static binary, `CGO_ENABLED=0` |
| Time to first stream | 1–2 days | 1–2 weeks |
| CPU per segment | High (full encode) | Very low (byte copying) |
| Picture freedom | Anything ffmpeg can draw | MB-aligned glyphs on flat colour only |
| Licences | ffmpeg / x264 (GPL if x264 is used) | go-aac is LGPL-2.1; rest MIT/BSD |

**Codec choice per rendition**

Each video rendition picks its codec, and the one ffmpeg run per segment simply ends in a different encoder per output. Overlays, SCTE-35 and emsg don't change; the work is in encoder settings, codec strings, manifests and CPU budget.

| `codec` | ffmpeg encoder | Settings for clean CMAF segments | Sample entry | Example codec string | CPU vs x264 |
| --- | --- | --- | --- | --- | --- |
| `h264` (default) | `libx264` | `-bf 0 -sc_threshold 0 -g K -keyint_min K`, `open-gop=0` | `avc1` + `avcC` | `avc1.640028` | 1× |
| `hevc` | `libx265` | `-x265-params bframes=0:keyint=K:min-keyint=K:scenecut=0:open-gop=0:repeat-headers=0`, `-tag:v hvc1` (Apple players reject `hev1`) | `hvc1` + `hvcC` | `hvc1.1.6.L120.90` | Several times slower |
| `av1` | `libsvtav1` | `-svtav1-params keyint=K:scd=0`, `-preset 8` to `10` for live speed, keyframes forced at segment starts with `-force_key_frames` | `av01` + `av1C` | `av01.0.08M.08` | Slower still; preset matters most |

K = fps × gopSeconds.

- **Profiles:** `h264` baseline / main / high; `hevc` main or main10 (main10 adds `-pix_fmt yuv420p10le` on that branch only); `av1` main (8 or 10 bit). Level is derived from size, fps and bitrate.
- **Init segments** keep coming from ffmpeg's own output, so the sample entries need no hand-building. Codec strings are derived from them: AVC and HEVC parameter sets parsed with mp4ff, plus a tiny `av1C` reader for AV1 (4 bytes).
- **HLS:** every `EXT-X-STREAM-INF` carries its own `CODECS` string, and players skip variants they can't decode. Order the multivariant playlist by codec preference (AV1, then HEVC, then H.264), highest bitrate first within each.
- **DASH:** one video AdaptationSet per codec, since switching codecs inside an AdaptationSet isn't seamless. Each gets the same `InbandEventStream`, and a `SupplementalProperty urn:mpeg:dash:adaptation-set-switching:2016` links them for players that allow switching at segment boundaries.
- **CPU budget:** segments are independent ffmpeg runs, so the producer encodes up to `-producer-parallelism` segments of a stream at once (default 2). Only throughput has to keep up with real time, not the time per segment. The stream API reports a rough cost estimate per ladder, and `segments_late_total` shows when the host can't keep up.
- **Determinism:** x264 output is byte-identical for a given build and settings. x265 and SVT-AV1 with threading still need verifying; if they differ between runs, only multi-replica setups without a shared cache are affected.
- **Image and readiness:** the Docker image ships an ffmpeg build with `libx265` and `libsvtav1`. `/readyz` checks that every encoder used by a running stream is present, and `start` refuses a stream whose codecs the local ffmpeg lacks (422).
- **Knock-on:** the pure-Go encoder (Option B) stays H.264 only. CEA-608 captions go into an SEI for H.264 and HEVC, and into an ITU-T T.35 metadata OBU for AV1.

## Timeline model

Everything derives from one anchor, the availability start time (AST), and integer counters from it: no floating-point time, no per-process counters. The same segment number always yields the same bytes, across restarts and across HLS and DASH.

| Quantity | Definition (S = segmentSeconds, F = fps, from the epoch snapshot) |
| --- | --- |
| AST | Per stream epoch: set by `start`, default = now rounded up to an even second; stored, so a restarted server resumes the same timeline |
| Video timescale | 90 000 (90 000 / F ticks per frame; the same clock as SCTE-35 PTS) |
| Audio | 48 kHz, timescale 48 000; frame size A = 1 024 samples for AAC, 1 536 for AC-3 and E-AC-3, 960 (20 ms) for Opus |
| Segment | S × F frames; segment N covers frames \[N·S·F, (N + 1)·S·F) |
| Frame number shown | Global frame index f since AST (top-left counter), restarting at 0 each epoch |
| Clock shown | AST + f / F, floored to the second, HH:MM:SS in the spec's timezone |
| Tic or toc | Second of the shown clock: odd → tic, even → toc |
| Media time | Video `tfdt` of segment N = N · S · 90 000; SCTE-35 `pts_time` = media time mod 2^33 |
| Live edge | Last N with AST + (N + 1) · S ≤ now, capped at the epoch's end segment once stopped |
| Frozen horizon | live edge + ceil(leadSeconds / S): segments up to here can no longer change, so the producer encodes them early |
| Window | Manifests list the last dvrSeconds; segments older than retainSeconds return 404 |

Audio frames do not line up with segments (93.75 AAC or 62.5 AC-3 frames per 2 s). Audio segment N holds every frame whose start time falls in \[N·S, (N + 1)·S) s, with its `tfdt` set to that first frame's start. Segment durations vary by one frame and never drift; the DASH timeline modes describe them exactly.

**Choosing a segment duration**

Any duration that is a whole number of video frames works. Some durations are also a whole number of AAC (1 024-sample) and AC-3 (1 536-sample) frames, so every audio segment has the same length and `status.audioAligned` is true.

| fps | Audio-aligned durations (AAC and AC-3) | Examples |
| --- | --- | --- |
| 25, 50 | Multiples of 0.32 s | 1.6, 1.92, 3.2, 3.84, 6.4 s |
| 30, 60 | Multiples of 1.6 s | 1.6, 3.2, 4.8, 6.4 s |
| 24 | Multiples of 8 s | 8 s |

- Non-aligned durations (2 s, 6 s) stay valid and are the most common in the field; they exercise players' handling of uneven audio segments, which is worth testing too.
- Fractional durations don't change anything else: HLS writes `EXTINF:1.920` and `TARGETDURATION` = the duration rounded up, DASH timescales stay 90 000 and 48 000.
- Ad breaks are still whole segments: a 30 s break on 1.92 s segments becomes 16 segments = 30.72 s, and the signals carry 30.72 s.
- `gopSeconds` shorter than the segment (for example 0.96 s GOPs in 1.92 s segments) puts several IDRs per segment, for testing players and packagers that split on GOPs.

**Live window**

`window.dvrSeconds` sets how far back players can seek: how many segments the HLS playlists list and the DASH `timeShiftBufferDepth`. `window.retainSeconds` sets how long a segment stays fetchable after it leaves the window, so slow players and CDNs never hit a 404 at the window's edge.

| Setting | Default | Range |
| --- | --- | --- |
| `dvrSeconds` | 30 s | 3 segments to 24 h |
| `retainSeconds` | 300 s | dvrSeconds to 24 h |

- **Changes apply immediately.** The window never changes media bytes, so a `PUT` that changes only `window` (or fault rules) takes effect on the next manifest request instead of waiting for a restart, and is logged as `stream.window_changed`. Shrinking is instant. Growing reaches back only as far as the epoch's start; older segments are rebuilt on demand.
- **Per-request override.** `?dvr=<seconds>` on a multivariant playlist, media playlist or MPD sets the window for that player only, between 3 segments and retainSeconds. Like fault parameters, it is copied onto every URL the manifest writes.
- **Right after start**, the window is shorter than dvrSeconds until that much time has passed since AST.

What a long window costs:

| Where | Cost | Mitigation |
| --- | --- | --- |
| HLS playlists | About 80 bytes per segment with PDT: 1 h of 2 s segments is about 140 KB, fetched every 2 s by every player for each playlist it follows | gzip on playlists; above 10 min, PDT only on the first segment and at break boundaries; playlist delta updates (`EXT-X-SERVER-CONTROL:CAN-SKIP-UNTIL`, `_HLS_skip=YES` → `EXT-X-SKIP`), which hls.js and Apple players support |
| DASH `timeline-*` | Video stays one `<S>` entry; audio with non-aligned segment durations does not: with 2 s segments, AAC needs 2 entries per 4 segments and AC-3 one per segment (≈ 20-45 KB per audio track for 1 h) | Audio-aligned segment durations keep audio at one entry too; `number` addressing is constant size; MPD Patch later |
| DASH `number` | None: the MPD size doesn't depend on the window | — |
| Ad signalling | Every break overlapping the window stays listed (288 entries for a 5-min recurring schedule over 24 h) | None needed |
| Memory | None beyond the default cache: segments outside the cached span are rebuilt on demand | A player seeking far back costs one ffmpeg run per segment it fetches, so count a seeking viewer like an extra stream while it catches up |

For tests, every manifest and segment URL accepts `?now=<unix ms>` to pretend the wall clock is elsewhere, the same trick DASH-IF livesim2 uses.

## Content generation

A frame is fully described by three values: its frame index, the clock second it shows, and whether it falls inside an ad break. The renderer turns that description into pixels (ffmpeg path) or into a list of glyph cells (tile path).

**Picture layout (drawn at the top rendition's size, H = its height)**

| Element | Position | Size | Content |
| --- | --- | --- | --- |
| Background | Full frame | — | Dark grey (Y 40) normally; red (BT.709 ≈ Y 63, Cb 102, Cr 240) in an ad |
| Edge | Frame border | 2 px line | Solid light grey; shows cropping or bad scaling |
| Clock | Top-left, inset H/24 | Font H/12 | `HH:MM:SS.FF`, FF = frame within the second (00 to fps − 1) |
| Sweep disc | Centre | Diameter 0.37·H | A wedge sweeps clockwise from 12 o'clock and completes one turn per second. Colours #F0F0F0 and #666666 swap each second: an even second starts light with a dark wedge growing, an odd second the reverse |
| Frame number | Inside the disc, H/20 below centre | Font H/20, 10 digits, zero-padded | Global frame index f, black with a white outline so it reads on both disc colours |
| Bip/Bop flash | Inside the disc, H/10 above centre | Font H/14 | "Bip!" on frame 0 of an even second, "Bop!" on frame 0 of an odd second; absent on every other frame |
| Waveform strip | Full width, just above the colour bars | H/34 tall, 2 px line | On frame 0 of each second, the beep as it sounds in that frame (Hz ÷ fps cycles: 25 for Bip, 8 for Bop at 60 fps); a flat line on every other frame |
| Colour bars | Bottom-left, inset H/68 | 7 squares of H/34 | 75% SMPTE order: grey, yellow, cyan, green, magenta, red, blue |
| Rendition label | Bottom-right, inset h/24 of each rendition | Font h/18 of each rendition, on a semi-opaque box | "720p · 1280×720 · 3000 kbit/s" (when `overlay.renditionLabel` is on) |

Lower renditions are scaled from that one picture, so every rendition shows the same frame. A rendition at half the frame rate keeps every other frame, so its frame number and FF step by 2. The rendition label is the exception: it is drawn on each branch after scaling, so it is sharp at every size and tells you at a glance which rendition the player picked. Sizes relative to H keep the text readable down to 360p. The disc and the waveform are drawn with `geq` on small inputs (0.37·H square and W × H/34) and overlaid, so no per-pixel expression runs over the full frame. The layout follows Apple's bipbop test stream: the disc shows stalls and dropped frames, and the flash, strip and beep land on the same frame, so one screenshot reads A/V offset. If the pure-Go encoder comes later, it will need these cells snapped to the 16 px grid.

**Audio**

- At the start of each second: a sine burst exactly one frame of the top rendition long (48000/fps samples, rounded: 800 at 60 fps, 960 at 50, 1920 at 25), with 1 ms raised-cosine fades (no clicks, little spectral splatter) and −6 dBFS peak; silence otherwise. It starts on the frame that shows the Bip/Bop flash and the waveform strip.
- Bip (even second, `tocHz`) and Bop (odd second, `ticHz`) frequencies come from the rendition; first-language defaults are 1500 Hz and 480 Hz, and defaults step up per language so alternate tracks sound different. Same in and out of ad breaks.
- On 5.1 renditions the beep moves through the channels one per second (L, R, C, LFE as a 60 Hz tone, Ls, Rs), then sounds on all six channels for the next two seconds, so the channel cycle is 8 s. Stereo and mono beep on all channels. The LFE burst stays 100 ms long, because one frame holds only one cycle of 60 Hz.
- The pattern repeats every 8 s for every layout: the Bip/Bop alternation (2 s) and the 5.1 channel cycle (8 s) both fit it, and 8 s is a whole number of frames for both codecs (375 AAC, 250 AC-3). At each start, ffmpeg encodes three 8 s periods per audio rendition (aevalsrc → native `aac` or `ac3` encoder at the rendition's bitrate and channels), and the runtime keeps the middle period.
- This requires AST on an even second so the loop phase matches the clock's odd/even seconds; enforce it when validating the AST given to `POST /api/streams/{id}/start`. The 5.1 cycle is counted from AST, so the first L beep falls on AST and every 8 s after it.

**ffmpeg command sketch (one run per segment, three renditions)**

```text
ffmpeg -hide_banner -nostdin
  -f lavfi -i color=c=<bg>:s=1920x1080:r=25:d=2
  -f lavfi -i "color=c=black:s=400x400:r=25:d=2, format=yuva420p,
       geq=lum='if(lt(<ang>,<sweep>), if(<odd>,240,102), if(<odd>,102,240))':cb=128:cr=128
          :a='if(lt(hypot(X-200,Y-200),200),255,0)'"                        # sweep disc, 400 = 0.37·H
  -f lavfi -i "color=c=black:s=1920x32:r=25:d=2, format=yuva420p,
       geq=lum=230:cb=128:cr=128
          :a='if(lt(abs(Y-16-<beep>*13*sin(2*PI*<hz>*X/(W*<fps>))),1.5),255,0)'"  # waveform strip, 32 = H/34
  -filter_complex "
     [0:v][1:v] overlay=x=(W-w)/2:y=(H-h)/2 [d];
     [d][2:v]   overlay=x=0:y=H-H/68-H/34-h [e];
     [e] drawbox=x=0:y=0:w=iw:h=ih:t=2:c=0xD0D0D0,
         drawbox=x=h/68+0*h/34:y=h-h/68-h/34:w=h/34:h=h/34:t=fill:c=0xBFBFBF,
         ... 6 more bars, x step h/34: 0xBFBF00 0x00BFBF 0x00BF00 0xBF00BF 0xBF0000 0x0000BF,
         drawtext=fontfile=GoMono.ttf:text='%{pts:gmtime:<segEpoch>:%H\:%M\:%S}.%{eif:mod(n+<firstFrame>,<fps>):d:2}'
                  :x=h/24:y=h/24:fontsize=h/12:fontcolor=white,
         drawtext=...:text='%{eif:n+<firstFrame>:d:10}':x=(w-tw)/2:y=h/2+h/20:fontsize=h/20
                  :fontcolor=black:borderw=2:bordercolor=white,
         drawtext=...:text='Bip!':enable='<beep>*eq(<odd>,0)':x=(w-tw)/2:y=h/2-h/10-th:fontsize=h/14:fontcolor=0x404040,
         drawtext=...:text='Bop!':enable='<beep>*eq(<odd>,1)':x=(w-tw)/2:y=h/2-h/10-th:fontsize=h/14:fontcolor=white,
         split=3 [a][b][c];
     [a]               drawtext=...:text='1080p · 1920x1080 · 5000 kbit/s':<bottom-right, box=1> [a2];
     [b] scale=1280:720, drawtext=...:text='720p · 1280x720 · 3000 kbit/s':...  [b2];
     [c] scale=640:360,  drawtext=...:text='360p · 640x360 · 800 kbit/s':...    [c2]"
  -map [a2] -c:v libx264 -b:v 5000k -maxrate 5000k -bufsize 10000k -profile:v high <common> /dev/shm/.../1080p.mp4
  -map [b2] -c:v libx264 -b:v 3000k -maxrate 3000k -bufsize 6000k -profile:v high <common> /dev/shm/.../720p.mp4
  -map [c2] -c:v libx264 -b:v 800k  -maxrate 800k  -bufsize 1600k -profile:v main <common> /dev/shm/.../360p.mp4

<common> = -preset veryfast -g <fps × gopSeconds> -keyint_min <same> -sc_threshold 0 -bf 0
           -x264-params open-gop=0 -f mp4
           -movflags empty_moov+default_base_moof+frag_keyframe -frag_duration <segment µs>

<ff>    = n + <firstFrame>  (N in geq)      <odd>  = mod(floor(<ff>/<fps>),2)
<beep>  = eq(mod(<ff>,<fps>),0)            <hz>   = if(<odd>,<ticHz>,<tocHz>)
<sweep> = 2*PI*mod(<ff>,<fps>)/<fps>       <ang>  = mod(atan2(X-200,200-Y)+2*PI,2*PI)
```

`segEpoch` = AST + N·S seconds and `firstFrame` = N·S·F, so numbering and clock are exact without any state carried between runs. The Go side builds this argument list from the epoch snapshot; `-g`, frame rate and duration all follow the spec.

## Packaging

One CMAF track per file, built with mp4ff: one init segment per video and audio rendition, created when the epoch starts, and media segments pre-generated by the producer and cached. Normal and red segments share the init segment, so a break needs no discontinuity.

**URLs**

| Path | Content |
| --- | --- |
| `/live/{stream}/master.m3u8` | HLS multivariant playlist (404 if the stream has no `hls`) |
| `/live/{stream}/{rendition}.m3u8` | HLS media playlist of one video, audio or subtitle rendition |
| `/live/{stream}/manifest.mpd` | DASH MPD (404 if the stream has no `dash`); `?addressing=` overrides the addressing mode |
| `/live/{stream}/{rendition}/init.mp4` | CMAF header (`ftyp` + `moov`) |
| `/live/{stream}/{rendition}/{N}.m4s` | Media segment N (HLS, DASH `number` and `timeline-number`) |
| `/live/{stream}/{rendition}/t/{mediaTime}.m4s` | The same segment addressed by media time (DASH `timeline-time`) |
| `/live/{stream}/{subtitle}/{N}.vtt` | WebVTT subtitle segment for HLS |
| `/api/streams/...` | Stream, ad-break, fault, event and timeline API (see Streams, SCTE-35 API, Fault injection, Event log) |
| `/api/time` | Server UTC time for `UTCTiming`, open and CORS-enabled |
| `/ui/` | Web UI (see Web UI); `/` redirects here |
| `/healthz`, `/readyz`, `/metrics` | Liveness, readiness, Prometheus (see Operations) |

Every `/live` path returns 404 for a stream in state `stopped`, and for renditions that are not in the running epoch's snapshot.

**Init segments**

- Video: `avc1` sample entry, `avcC` from the encoder's SPS/PPS (High or Main profile, level 3.1), timescale 90 000, one per rendition at its own size.
- Audio: `mp4a` with `esds` holding the AudioSpecificConfig for AAC-LC, or ac-3 with dac3 for AC-3; 48 kHz, the rendition's channel count, timescale 48 000, and an elng box when the language is not a plain ISO 639-2 code.
- Brands: `cmfc`, `iso6`, `dash`. No edit list on video; on audio, an `elst` removing the 1 024-sample encoder priming is optional (players tolerate it on a live stream).

**Media segment layout**

```text
styp  (major cmfs, compatible cmfs/msdh/msix)
emsg* (video track only, v1, zero or more SCTE-35 messages)
moof
  mfhd  sequence_number = N + 1
  traf
    tfhd  default-base-is-moof, track_ID
    tfdt  v1, baseMediaDecodeTime = segment start in track timescale
    trun  data_offset, per-sample size, duration 3600 (video) or 1024 (audio),
          first-sample flags = sync (video IDR)
mdat  AVC length-prefixed NAL units / raw AAC access units
```

- With no B-frames, composition time = decode time, so `trun` needs no composition offsets.
- In-band SCTE-35 rides in every video rendition's track (identical emsg boxes), which is what SCTE 214-1 receivers look for; the audio track stays clean.
- Cache key = (stream, epoch, track or rendition, N, variant). The variant is the background colour plus the hash of emsg boxes it carries, so cancelling a break invalidates exactly the affected segments.

* **Bitrate padding:** every video segment is padded to its rendition's bitrate × duration with codec filler (H.264 filler NAL, HEVC FD\_NUT, AV1 padding OBU), because the synthetic picture otherwise encodes at 7-33 % of target (see Performance). The cache stores the real bytes plus the padded sample sizes; the filler is written while serving.

## Manifests

Manifests are rendered per request from the clock and the break schedule; nothing is stored. Both carry the same break, with the same event ID and the same SCTE-35 bytes, so HLS and DASH players can be compared side by side.

**HLS media playlist (one rendition; other renditions and audio differ only in paths and audio EXTINF)**

```text
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:812340
#EXT-X-MAP:URI="video/init.mp4"
#EXT-X-PROGRAM-DATE-TIME:2026-11-06T18:29:40.000Z
#EXTINF:2.000,
video/812340.m4s
...
#EXT-X-DATERANGE:ID="7",START-DATE="2026-11-06T18:30:00.000Z",PLANNED-DURATION=30.000,SCTE35-OUT=0xFC30...
#EXT-X-CUE-OUT:DURATION=30
#EXT-X-PROGRAM-DATE-TIME:2026-11-06T18:30:00.000Z
#EXTINF:2.000,
video/812350.m4s
#EXT-X-CUE-OUT-CONT:ElapsedTime=2,Duration=30
#EXTINF:2.000,
video/812351.m4s
...
#EXT-X-DATERANGE:ID="7",START-DATE="2026-11-06T18:30:00.000Z",END-DATE="2026-11-06T18:30:30.000Z",SCTE35-IN=0xFC30...
#EXT-X-CUE-IN
#EXTINF:2.000,
video/812365.m4s
```

- Every segment gets `EXT-X-PROGRAM-DATE-TIME`, so DATERANGE dates map to exact segments.
- Ad tags are identical in every media playlist of the stream (all video and audio renditions), so an ABR or audio-track switch mid-break loses nothing.
- `ads.hls` picks one or more cue styles (below). Tags live only in playlists, so `?cues=daterange,oatcls` can override the styles per request without touching segments.
- A break's opening tags stay in the playlist while any part of the break is inside the DVR window, even if its start has scrolled out.
- Audio EXTINF values are the real audio durations (for 2 s segments: 1.984 or 2.005 s with AAC, 1.984 or 2.016 s with AC-3); TARGETDURATION = segmentSeconds rounded up.
- Multivariant playlist: one `EXT-X-MEDIA:TYPE=AUDIO` per audio rendition, with `GROUP-ID` = its group, `LANGUAGE`, `NAME`, `CHANNELS` ("2", "6"), `DEFAULT=YES` and `AUTOSELECT=YES` on the group's default; then one `EXT-X-STREAM-INF` per (video rendition × audio group), highest bitrate first, with `BANDWIDTH` = (video + largest audio in the group) × 1.1, `AVERAGE-BANDWIDTH`, `CODECS` listing every audio codec in the group (for example `avc1.640028,mp4a.40.2,ac-3`), `RESOLUTION`, `FRAME-RATE` and `AUDIO="<group>"`; plus `EXT-X-INDEPENDENT-SEGMENTS`.
- After `stop`, media playlists end at the epoch's end segment with `EXT-X-ENDLIST` and keep being served until retainSeconds has passed.

**HLS cue styles (`ads.hls`)**

| Style | Tags written | Typical consumers |
| --- | --- | --- |
| `daterange` | `EXT-X-DATERANGE` with `SCTE35-OUT` at the start, same `ID` with `END-DATE` and `SCTE35-IN` at the end (`SCTE35-CMD` when `command` is `time_signal`) | Apple players, hls.js, Shaka, modern SSAI |
| `cueout` | `EXT-X-CUE-OUT:DURATION=`, `EXT-X-CUE-OUT-CONT:ElapsedTime=,Duration=` on every segment of the break, `EXT-X-CUE-IN` | Generic SSAI and ad-tracking tools |
| `oatcls` | `EXT-OATCLS-SCTE35:<base64 cue>` before `EXT-X-CUE-OUT:<duration>`, `CUE-OUT-CONT` carrying `SCTE35=<base64>`, `EXT-X-CUE-IN`; `EXT-X-ASSET:CAID=` when the cue has a UPID | AWS Elemental and MediaTailor workflows, other SSAI that read the raw cue |
| `adobe` | `EXT-X-SCTE35:CUE="<base64>",ID=,DURATION=,TIME=` at the start and a matching cue-in | Adobe Primetime-style consumers; attribute sets vary by vendor, so check against the target |

The same 30 s break (ID 7, segments 812 350 to 812 364) in each style:

```text
# daterange
#EXT-X-DATERANGE:ID="7",START-DATE="2026-11-06T18:30:00.000Z",PLANNED-DURATION=30.000,SCTE35-OUT=0xFC30...
...
#EXT-X-DATERANGE:ID="7",START-DATE="2026-11-06T18:30:00.000Z",END-DATE="2026-11-06T18:30:30.000Z",SCTE35-IN=0xFC30...

# cueout
#EXT-X-CUE-OUT:DURATION=30
#EXT-X-CUE-OUT-CONT:ElapsedTime=2,Duration=30        (before each later segment of the break)
#EXT-X-CUE-IN

# oatcls
#EXT-OATCLS-SCTE35:/DAlAAAAAAAAAP/wFAUAAAAHf+...
#EXT-X-CUE-OUT:30
#EXT-X-CUE-OUT-CONT:ElapsedTime=2,Duration=30,SCTE35=/DAlAAAAAAAAAP/wFAUAAAAHf+...
#EXT-X-CUE-IN

# adobe
#EXT-X-SCTE35:CUE="/DAlAAAAAAAAAP/wFAUAAAAHf+...",ID="7",DURATION=30,TIME=1794076200.0
#EXT-X-SCTE35:CUE="/DAgAAAAAAAAAP/wDwUAAAAHf...",ID="7"
```

**DASH MPD**

```text
<MPD type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011,urn:mpeg:dash:profile:cmaf:2019"
     availabilityStartTime="<epoch AST>" publishTime="<now>" minimumUpdatePeriod="PT<S>S"
     timeShiftBufferDepth="PT<dvr>S" suggestedPresentationDelay="PT<3S>S" minBufferTime="PT<S>S">
  <Period id="e<epoch>" start="PT0S">
    <EventStream schemeIdUri="urn:scte:scte35:2014:xml+bin" timescale="90000">
      <Event id="7" presentationTime="<startTicks>" duration="2700000">
        <scte35:Signal><scte35:Binary>/DAl...</scte35:Binary></scte35:Signal>
      </Event>
    </EventStream>

    <AdaptationSet contentType="video" mimeType="video/mp4" segmentAlignment="true"
                   startWithSAP="1" maxWidth="1920" maxHeight="1080" frameRate="25">
      <InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin" value=""/>
      <SegmentTemplate ...see DASH segment addressing.../>
      <Representation id="1080p" codecs="avc1.640028" width="1920" height="1080" bandwidth="5000000"/>
      <Representation id="720p"  codecs="avc1.64001f" width="1280" height="720"  bandwidth="3000000"/>
      <Representation id="360p"  codecs="avc1.4d401e" width="640"  height="360"  bandwidth="800000"/>
    </AdaptationSet>

    <!-- one audio AdaptationSet per (lang, channels, codec); bitrate tiers become Representations -->
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en" segmentAlignment="true">
      <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
      <Label>English</Label>
      <AudioChannelConfiguration schemeIdUri="urn:mpeg:mpegB:cicp:ChannelConfiguration" value="2"/>
      <SegmentTemplate .../>
      <Representation id="en" codecs="mp4a.40.2" audioSamplingRate="48000" bandwidth="128000"/>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="fr" segmentAlignment="true">
      <Role schemeIdUri="urn:mpeg:dash:role:2011" value="alternate"/>
      <Label>Français</Label> ... <Representation id="fr" .../>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en" segmentAlignment="true">
      <Role schemeIdUri="urn:mpeg:dash:role:2011" value="alternate"/>
      <Label>English 5.1</Label>
      <AudioChannelConfiguration schemeIdUri="tag:dolby.com,2014:dash:audio_channel_configuration:2011" value="F801"/>
      <SegmentTemplate .../>
      <Representation id="en-51" codecs="ac-3" audioSamplingRate="48000" bandwidth="384000"/>
    </AdaptationSet>

    <UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="/api/time"/>
  </Period>
</MPD>
```

After `stop`, the MPD gains `mediaPresentationDuration` = end of the epoch's last segment and drops `minimumUpdatePeriod`, which tells DASH players the live stream has ended.

**DASH segment addressing**

Three `SegmentTemplate` modes work for a live stream, and the origin offers exactly those three. `dash.addressing` in the spec sets the default; `?addressing=` on `manifest.mpd` overrides it per request, so one running stream can be played in all three modes side by side.

| Mode | Template | Segment URL | Live? | Trade-offs |
| --- | --- | --- | --- | --- |
| `timeline-time` (default) | `SegmentTimeline` + `media="$RepresentationID$/t/$Time$.m4s"` | `{r}/t/{mediaTime}.m4s` | Yes | Exact duration of every segment, so audio's uneven segments are described precisely. URLs carry media time and don't depend on `startNumber`, which makes this the most robust choice for multi-period ads and SSAI. Players learn new segments only by reloading the MPD (`minimumUpdatePeriod` = S). |
| `timeline-number` | `SegmentTimeline` + `media="$RepresentationID$/$Number$.m4s"`, `startNumber` = first segment in the window | `{r}/{N}.m4s` | Yes | Exact durations, and the same files HLS uses. `startNumber` moves with the window on every MPD update, which a few older players handle badly. |
| `number` | `duration="<S × timescale>"` + `media="$RepresentationID$/$Number$.m4s"`, `startNumber="0"`, no timeline | `{r}/{N}.m4s` | Yes | Simplest and smallest MPD; players compute the live segment from wall clock and AST, so the MPD barely needs reloading. Durations are nominal: audio segments are really ±1 frame off, which @duration allows, but precise clock sync becomes essential, so `UTCTiming` is mandatory. |
| `$Time$` with `@duration` | — | — | No | Invalid: `$Time$` needs a `SegmentTimeline` to say what the times are. |
| `$Time$` and `$Number$` in one template | — | — | No | The DASH spec forbids using both identifiers in the same template. |
| `SegmentList` | Explicit `SegmentURL` per segment | — | Not offered | Legal in a dynamic MPD but the whole list must be rewritten on every update, and player support for live is poor. |
| `SegmentBase` | One file + `sidx` index | — | No | On-demand only: a growing single file cannot be indexed up front. |
| Low-latency extras (`$SubNumber$`, `availabilityTimeOffset`, chunked CMAF) | — | — | Later | Out of scope for v1. |

**How the timeline modes describe each track** (2 s segments, window starting at segment 812 340):

```text
video, timescale 90000:  <S t="146221200000" d="180000" r="14"/>
AAC,   timescale 48000:  <S t="77984640000" d="96256" r="2"/><S d="95232"/><S d="96256" r="2"/>...  // 94,94,94,93 frames
AC-3,  timescale 48000:  <S t="77984640000" d="96768"/><S d="95232"/>...                          // 63,62 frames
```

- The MPD builder generates `<S>` entries from the same `audioFrameRange(n)` the packager uses, merging equal neighbours with `@r`, so MPD and segments cannot disagree.
- `@t` of the first entry = that segment's `tfdt`; with `timeline-number`, `startNumber` = that segment's N.
- Requests by time are mapped back to N: video N = t ÷ (S × 90 000); audio N is the segment whose first frame starts at t. Anything that doesn't land exactly on a segment start returns 404.

**DASH ad signalling (`ads.dash`, `ads.dashMultiPeriod`)**

| Style | What the origin writes | Typical consumers |
| --- | --- | --- |
| `eventstream-bin` | MPD `EventStream` `urn:scte:scte35:2014:xml+bin`, one `Event` per cue-out and cue-in with the base64 `scte35:Binary` | dash.js, Shaka, most SSAI |
| `eventstream-xml` | MPD `EventStream` `urn:scte:scte35:2013:xml`, the full `SpliceInfoSection` as XML (scte35-go emits it) | XML-oriented tools and older SSAI |
| `inband` | `InbandEventStream` in each video AdaptationSet plus emsg v1 `urn:scte:scte35:2013:bin` in the video segments | SCTE 214-1 receivers, players that read emsg |
| `dashMultiPeriod: true` | The break becomes its own Period, and the content after it a new Period, with `presentationTimeOffset` keeping media time continuous | SSAI and players that expect period-based ads |

- Old events drop out of the MPD with the time-shift window.
- `eventstream-*` and multi-period only change the MPD, so `?dashcues=` and `?periods=` can override them per request.
- `inband` changes the segments themselves (shared by HLS and DASH), so it is fixed for the epoch and can't be overridden per request.

## SCTE-35 API

A break is a span of whole segments \[startSeg, endSeg) plus an event ID; every signal, emsg and red frame is computed from that record. The API only ever schedules into the future, past the point any player could already have fetched, so published media never changes.

**Endpoints**

| Method and path (under `/api/streams/{id}`) | Body / effect | Response |
| --- | --- | --- |
| `POST .../breaks` | `{"duration": 30, "in": "10s"}` or `{"duration": 30, "at": "2026-11-06T18:30:00Z"}`; both optional except duration | 201 with the break record (ID, snapped start, end, base64 cue-out); 409 unless the stream is running |
| `GET .../breaks` | List upcoming, active and recent breaks of the current epoch | 200 |
| `GET .../breaks/{breakId}` | One break, including its cue-out and cue-in in base64 and hex | 200 / 404 |
| `DELETE .../breaks/{breakId}` | Not started: cancel. Active: end early at the next safe boundary | 200 with the updated record |

**Scheduling rules**

1. **Lead time.** Earliest allowed start = frozen horizon + 1 segment (see Timeline model), with `ads.leadSeconds` from the epoch snapshot (default 8 s): enough for the in-band announcement and for players that refresh manifests every few seconds.
2. **Snap.** Requested start is rounded up to the next segment boundary at or after max(requested, earliest). Duration is rounded up to whole segments; the record keeps both the requested and the real duration, and signals carry the real one.
3. **No overlap.** A request that overlaps an existing break returns 409 with the conflicting ID.
4. **Early return.** For an active break, the new end = max(first segment after the frozen horizon, start + 1 segment). Segments before that keep their red background, and a cue-in is signalled at the new end.
5. **Cancel.** For a break that has not started, emit a cancel cue (see Signals) in segments that are still to be produced; manifests stop showing it.
6. **Stop.** Stopping a stream cancels breaks that start after its end segment and cuts an active break at the end segment.
7. **Persistence.** Breaks go through the `BreakRepository`, keyed by stream and epoch: in memory in v1, a table later. With the AST also stored, a restarted server rebuilds exactly the same segments.
8. **Recurring breaks.** With `ads.recurring`, break k starts at AST + offsetSeconds + k × everySeconds, snapped to a segment boundary, and lasts durationSeconds. They are computed, not stored, so the schedule never runs out: IDs are derived (`r-<k>`, SCTE-35 event ID 0x80000000 + k; manual breaks count up from 1) and `GET .../breaks` lists the next few. Cancelling or ending one early stores an override record under its ID. A manual break that overlaps a recurring one returns 409, unless the request sets `"replaceRecurring": true`, which stores a cancel override for it.

**Signals (built with scte35-go)**

| Signal | Command | Key fields |
| --- | --- | --- |
| Cue-out (`command: splice_insert`) | `splice_insert` | event ID, out\_of\_network = 1, program splice, `pts_time` = start media time mod 2^33, break\_duration with auto\_return = 1, unique\_program\_id = 1 |
| Cue-in (`splice_insert`) | `splice_insert` | same event ID, out\_of\_network = 0, `pts_time` = end media time |
| Cue-out (`command: time_signal`) | `time_signal` + segmentation descriptor | `pts_time` = start; segmentation\_event\_id = break ID; type = start code of `segmentationType` (provider placement opportunity 0x34, provider ad 0x30, break 0x22, distributor placement opportunity 0x36); segmentation\_duration; segment\_num 1 of 1 |
| Cue-in (`time_signal`) | `time_signal` + segmentation descriptor | same event ID, matching end code (0x35, 0x31, 0x23, 0x37) |
| Cancel | either command | `splice_event_cancel_indicator` or `segmentation_event_cancel_indicator` = 1 |

`pts_adjustment` is 0 because media time is already the 90 kHz PTS. The same bytes go into the HLS `SCTE35-OUT`/`SCTE35-IN` attributes (hex), the MPD `scte35:Binary` (base64) and the emsg payload (binary).

**In-band emsg placement**

- emsg v1, scheme `urn:scte:scte35:2013:bin`, timescale 90 000, `presentation_time` = break start (or end for cue-in) in media time, `event_duration` = break duration (0 for cue-in), `id` = event ID.
- Repeated in every video segment from the one covering start − leadSeconds (or the first unpublished segment, if later) up to the segment before the break starts, so a player that joins late still sees it. Cue-in follows the same rule against the break end.
- This matches SCTE 214-1 and the approach DASH-IF [livesim2](https://github.com/Dash-Industry-Forum/livesim2/wiki/URL-Parameters) uses for its own SCTE-35 test streams.

## Subtitles and captions

Text tracks carry the same information as the picture, one cue per second, so you can check subtitle sync against the burnt-in clock at a glance. They are generated in pure Go (no ffmpeg) and cost almost nothing to produce.

**Cue content**

- One cue per second of the shown clock, back to back from second start to second end: `SUB · 18:30:05 · frame 1234 · Bip!` (frame = first frame of that second; Bip! on even seconds, Bop! on odd, as in the picture). The `SUB` prefix says the text comes from the WebVTT or IMSC1 track; 608 captions use `CC1` instead, so you can tell which path the player is showing.
- Inside an ad break the cue is prefixed with the break position, for example `[AD 12/30] SUB · 18:30:05 · frame 1234 · Bop!`.
- Each track adds its language name as a prefix (`FR ·`), so switching tracks is visible.
- Cues are cut at segment boundaries: a cue that crosses one is written into both segments with the matching start and end.

**Formats**

| `format` | HLS | DASH |
| --- | --- | --- |
| `webvtt` | Plain WebVTT segments `/{sub}/{N}.vtt`, each with `X-TIMESTAMP-MAP=MPEGTS:<segment start, 90 kHz, mod 2^33>,LOCAL:<segment start>`; `EXT-X-MEDIA:TYPE=SUBTITLES` | fMP4 segments with `wvtt` samples (ISO/IEC 14496-30) built with mp4ff; `AdaptationSet contentType="text" mimeType="application/mp4" codecs="wvtt"` |
| `imsc1` | fMP4 segments with `stpp` samples (one TTML document per segment), `CODECS="stpp.ttml.im1t"` on the `EXT-X-MEDIA` entry | Same fMP4 segments, `codecs="stpp.ttml.im1t"` |

- HLS: all subtitle tracks go in one group (`GROUP-ID="subs"`), with `LANGUAGE`, `NAME`, `DEFAULT`/`AUTOSELECT` from the spec and `FORCED=NO`; every `EXT-X-STREAM-INF` gains `SUBTITLES="subs"`.
- DASH: one AdaptationSet per subtitle track with `lang`, `Label` and `Role value="subtitle"`, using the same segment addressing mode as audio and video.
- Subtitle segments use the same numbering, timing and window as the other tracks, and fault rules can target them like any rendition.

**CEA-608 captions (`captions.cea608`)**

- The same text with a `CC1` prefix in place of `SUB` (`CC1 · 18:30:05 · frame 1234 · Bip!`), as pop-on captions on the chosen channel, carried inside the video: an SEI NAL unit (ATSC A/53 `GA94` `cc_data`) is added to each video sample in Go, after ffmpeg and before packaging.
- Captions sit on row 1, centred, so they never cover the top-left clock or the WebVTT/IMSC1 cue at the bottom. Each caption is shown on frame 0 of its second exactly; unlike bipbop's 608 track, which advances every 1.001 s and drifts, the timing never slips.
- Caption bytes are spread at the standard 608 field-1 rate and padded with null pairs, so every rendition carries identical captions.
- HLS: `EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID="cc",INSTREAM-ID="CC1",LANGUAGE="en"` and `CLOSED-CAPTIONS="cc"` on every variant. DASH: `<Accessibility schemeIdUri="urn:scte:dash:cc:cea-608:2015" value="CC1=eng"/>` on the video AdaptationSet.
- Needs a small 608 writer (pop-on: load, row/column, text pairs, end of caption), about 300 lines. Planned for v1.1, after WebVTT and IMSC1.

## Fault injection

Faults change how a response is delivered, never what the media contains, so they can be switched on and off while a stream runs. They come from two places: rules on the stream, for long-running tests, and query parameters on the manifest URL, for one player session.

**Rules**

```text
FaultRule {
  id        "slow-720p"
  match {
    kind        "segment"            // segment | init | playlist | mpd | master
    renditions  ["720p"]             // empty = all video, audio and subtitle renditions
    segments    { from: 0, to: null } // optional range of N
  }
  percent   10                        // share of matching requests hit
  mode      "sticky"                  // sticky: same segments fail on every retry and replica
                                      // transient: random per request, so a retry may succeed
  action    { delayMs: 3000 }         // one or more of the actions below
  ttlSeconds 600                      // optional; the rule removes itself afterwards
}
```

| Action | Applies to | Effect |
| --- | --- | --- |
| `status` | all | Reply with that code (404, 410, 500, 503, 504) and an empty body |
| `delayMs` | all | Wait before the first byte; combines with other actions |
| `throttleKbps` | all | Send the body at this rate |
| `truncatePercent` | segments, init | Send only this share of the body, then close the connection |
| `reset` | all | Close the connection without any response |
| `staleSeconds` | playlist, mpd | Serve the manifest as it was this many seconds ago |
| `gap` | segments | 404 for the matched range, plus `EXT-X-GAP` on those segments in HLS playlists |
| `clockSkewSeconds` | `/api/time`, `Date` header | Report a shifted wall clock, to test players' time sync |

- **Sticky selection** is `hash(stream, epoch, rule id, rendition, N) mod 100 < percent`, so every replica fails the same segments and tests are reproducible. **Transient** uses a random draw per request.
- **Live rule API:** `GET`/`PUT /api/streams/{id}/faults` reads or replaces the active rules immediately, without a restart; `DELETE` clears them. `spec.faults` only seeds the rules at each start. Active rules go through a `FaultRepository` keyed by stream, so later every replica shares them.
- Faults never apply to `/api` (apart from `/api/time` for clock skew), `/healthz`, `/readyz` or `/metrics`.

**Per-session faults in the URL**

A manifest requested with fault parameters copies the same query string onto every URL it writes: variant playlists, init and media segments, and the DASH `SegmentTemplate` `media` and `initialization` attributes. One player can then be tested without affecting anyone else on the stream.

| Parameter | Meaning |
| --- | --- |
| `fault=503` | Status to return |
| `faultOn=segment,playlist` | Kinds hit (default `segment`) |
| `faultRenditions=720p,en` | Renditions hit (default all) |
| `faultPercent=10` | Share hit, sticky (`faultMode=transient` for random) |
| `delay=2000`, `throttle=500`, `truncate=50`, `reset=1`, `stale=10` | The matching actions above |

Every injected fault increments `faults_injected_total{stream, rule, action}` and is logged with the request, so a test report can tell injected failures from real ones.

## Operations

What the service needs to run safely behind a browser and, later, as a microservice: CORS for web players, API keys for the control plane, health checks, metrics and clean shutdown. Playback stays open by default; only `/api` is protected.

**CORS**

| Paths | Headers |
| --- | --- |
| `/live/*`, `/api/time` | `Access-Control-Allow-Origin: *`; methods `GET, HEAD, OPTIONS`; allowed headers `Range, Origin, Accept`; exposed headers `Content-Length, Content-Range, Date` (dash.js reads `Date` for time sync) |
| `/api/*` | Origins from `ORIGIN_CORS_API_ORIGINS` (none by default); methods `GET, POST, PUT, DELETE, OPTIONS`; allowed headers `Authorization, Content-Type, If-Match`; exposed `ETag, Location` |

- Preflight `OPTIONS` answers 204 with `Access-Control-Max-Age: 86400`, before auth and fault rules run.
- Segments and init files support `Range` requests (206), since some players and CDNs use them.

**API keys**

- `Authorization: Bearer <key>` on every `/api` call except `/api/time`. Keys come from `ORIGIN_API_KEYS` as `name:role:key`, with role `read` (GET only) or `write` (everything). Missing or unknown key: 401; read key on a mutation: 403.
- Keys are compared in constant time and never logged; logs carry the key's name.
- Later, as a microservice: keys stored hashed (SHA-256) in the database, or JWTs checked by an API gateway; optional signed playback URLs per stream.

**Health**

| Endpoint | 200 when | Otherwise |
| --- | --- | --- |
| `/healthz` | The process is serving HTTP | No answer (the orchestrator restarts it) |
| `/readyz` | Repositories reachable (a DB ping later); ffmpeg found and its version matches the pinned one; tmp dir writable; no running stream more than 2 segments late | 503 with a JSON list of failing checks |

**Metrics (`/metrics`, Prometheus)**

| Metric | Labels |
| --- | --- |
| `http_requests_total`, `http_request_duration_seconds` | route, kind (manifest, init, segment, api), status |
| `segments_built_total`, `segment_build_seconds` | stream, track kind |
| `segments_late_total` | stream |
| `ffmpeg_running`, `ffmpeg_failures_total` | stream, reason |
| `cache_bytes`, `cache_hits_total`, `cache_misses_total` | — |
| `streams` | state |
| `ad_breaks_total`, `scte35_signals_total` | stream, source (manual, recurring), type |
| `faults_injected_total` | stream, rule, action |

Labels never include segment numbers or client data, so cardinality stays bounded by the number of streams and renditions.

**Logging, config and shutdown**

- JSON logs with `log/slog`: request ID (`X-Request-Id`, generated if absent), stream, rendition, N, status, duration, injected fault; ffmpeg's stderr tail on any failure. Segment access logs can be sampled.
- Configuration from flags with environment overrides: `ORIGIN_LISTEN`, `ORIGIN_API_KEYS`, `ORIGIN_FFMPEG`, `ORIGIN_TMP`, `ORIGIN_CACHE_BYTES`, `ORIGIN_MAX_FFMPEG`, `ORIGIN_CORS_API_ORIGINS`, later `ORIGIN_DSN`.
- On SIGTERM: `/readyz` turns 503, the server stops accepting connections and drains in-flight requests (10 s), producers are cancelled, ffmpeg children are killed by process group, and the tmp dir is cleaned. Streams stay `running` in the repository, so a restarted or other replica resumes them.

**Request pipeline**

```text
handler = recover
        > requestId + access log + metrics
        > cors                       // answers OPTIONS here
        > auth (only /api, except /api/time)
        > faults (only /live and /api/time)
        > route
```

## Event log

Every state change and notable runtime occurrence is appended to a per-stream event log. The log is the source of the UI timeline and doubles as an audit trail (who started the stream, who inserted that break). Media is still derived from config and breaks only; the log never feeds back into segments.

**Event shape**

```text
Event {
  id         "01JC9X..."               // ULID: sortable by time, unique across replicas
  streamId   "demo"
  epoch      4                          // null for stream-level events before a start
  at         "2026-11-06T18:30:00.000Z" // wall clock when it happened
  segment    812350                     // media position, when there is one
  type       "break.started"
  actor      "api:qa-team" | "system"   // API key name, or the server itself
  data       { breakId: "7", eventId: 7, durationSeconds: 30, source: "manual" }
}
```

**Event types**

| Group | Types | Emitted by |
| --- | --- | --- |
| Stream | `stream.created`, `stream.updated` (with a field diff), `stream.deleted`, `stream.started`, `stream.stopped`, `stream.expired` (ended → stopped) | Stream service, janitor |
| Breaks | `break.scheduled`, `break.cancelled`, `break.shortened`, `break.started`, `break.ended` | Break API when requested; producer when the live edge crosses start or end |
| Signals | `signal.announced` (first segment carrying the emsg), `signal.cue_out`, `signal.cue_in`, `signal.cancel`, with command, styles and the base64 cue | Producer |
| Faults | `faults.updated` (rule set changed), `fault.injected` (aggregated per rule and minute, with a count) | Fault API, fault middleware |
| Health | `segment.late`, `ffmpeg.failed` (with stderr tail), `segment.retried`, `runtime.resumed` (after a server restart) | Producer, stream manager |

Scheduled events like `break.started` are written when they actually happen (the live edge reaches the segment), not when they are scheduled, so the log reads as history. The UI draws future breaks from the break API instead.

**Storage and API**

- `EventRepository` with `append(event)` and `list(streamId, from, to, types, cursor, limit)`. v1: an in-memory ring buffer per stream (default 10 000 events, oldest dropped). Later: an append-only table indexed on (stream\_id, at).
- `GET /api/streams/{id}/events?from=&to=&types=&cursor=&limit=` returns a page, oldest first, with a cursor for the next one.
- `GET /api/streams/{id}/events/live` is a Server-Sent Events stream: it replays from `Last-Event-ID` (or `?since=`), then pushes each new event. The SSE event ID is the ULID, so a reconnecting browser resumes without gaps.
- `GET /api/streams/{id}/timeline?from=&to=` returns everything the timeline view needs in one call: the timeline markers (AST, live edge, frozen horizon, DVR window), breaks in range including future and recurring ones, and the events in range.
- In a multi-replica setup, only the replica that holds a stream's producer lease emits producer events, so nothing is logged twice. An in-process broadcaster feeds the SSE clients; later, Postgres `LISTEN/NOTIFY` fans out across replicas.

## Web UI

A small single-page app served by the same binary at `/ui/`, built only on the public JSON API. It adds no backend logic of its own, which keeps the binary self-contained today and lets the UI move to its own deployment once the origin becomes a microservice.

**Approach**

- Static files embedded with `go:embed`; `-ui=false` turns them off. `/` redirects to `/ui/`.
- No Node build step: plain ES modules with Preact + htm (about 15 KB), and vendored copies of hls.js and dash.js, so the UI works offline and on closed networks.
- Considered: Go templates + htmx. Rejected because it needs extra HTML endpoints next to the API; the SPA uses exactly what any other client uses, which also keeps the API honest.
- Auth: on first visit the UI asks for an API key, keeps it in `sessionStorage` and sends it as a Bearer token. Same origin as the API, so no CORS involved. With a `read` key, every action button is hidden.

**Pages**

| Page | Path | Content |
| --- | --- | --- |
| Streams | `/ui/` | One row per stream: name and id, state badge, uptime since AST, formats, ladder summary ("3 video · 3 audio · 2 subs"), live-edge segment, active or next break, health (late segments, active fault rules). Start and stop buttons. Refreshes every 5 s. |
| Stream detail | `/ui/streams/{id}` | Header with state, epoch, AST and a "restart to apply" banner when `pendingChanges` is true. Playback URLs with copy buttons, plus a builder for query options (addressing mode, cue styles, URL faults). Embedded player (hls.js or dash.js, switchable) with rendition, audio and subtitle pickers and a log of the ad events the player reports. The timeline (below). Break panel: insert now, in N s or at a time; cancel; end early. Fault panel: rule list, editor and presets ("5 % 503 on segments", "2 s delay on 720p"). Event table under the timeline, filterable by type. |
| Create / edit | `/ui/streams/new`, `/ui/streams/{id}/edit` | Form for the common fields: formats, segment duration with the audio-aligned values suggested for the chosen fps, ladder table (add and remove rows), audio and subtitle tracks, ad and recurring settings. A raw JSON tab for everything else. API validation errors (422) appear next to their fields; a 412 on save reloads the stream and shows what changed. |

**Live updates**

- The detail page subscribes to `/api/streams/{id}/events/live` (SSE). New events slide into the timeline and the event table; the live-edge marker advances every segment from the local clock, corrected by `/api/time`. The browser's built-in EventSource can't send an Authorization header, so the UI reads the SSE stream with fetch and a streaming body reader instead, keeping the Bearer key out of URLs and logs.
- The streams page polls `GET /api/streams` every 5 s, which is cheap and avoids one SSE connection per stream.

**Timeline view**

&#91;embedded content: stream timeline mockup · 5 lanes\]

- One lane per event group, a shared time axis, and three markers: the DVR window (what a player can seek to), the live edge (now) and the frozen horizon (the earliest point a new break can start).
- Zoom presets: 2 min, 30 min, whole epoch. The view keeps scrolling with the live edge until the user drags it back in time.
- Hover shows an event's details; clicking a break opens the break panel; clicking a signal shows the cue decoded to JSON by scte35-go, next to its base64 and the exact tags each style wrote.
- Drawn as SVG by the page itself from `GET /api/streams/{id}/timeline`, then kept current by the SSE stream.

## Pseudocode

Eleven small modules. Media modules are pure functions of (epoch snapshot, segment number, breaks); only the stream service, the repositories and the stream manager hold state. Suggested Go package names are in brackets. Module 6 (tile encoder) is kept for the later pure-Go option and is not part of v1.

**1. Startup \[main\]**

```text
main():
    cfg     = parseFlags()          // listen addr, ffmpeg path, maxFfmpeg, tmpDir, cacheBytes
    streams = MemoryStreamRepo()    // later: PostgresStreamRepo(cfg.dsn)
    breaks  = MemoryBreakRepo()     // later: PostgresBreakRepo(cfg.dsn)
    enc     = FfmpegEncoder(cfg.ffmpegPath, cfg.tmpDir, semaphore(cfg.maxFfmpeg))
    manager = StreamManager(streams, breaks, enc, LRU(cfg.cacheBytes))
    manager.resumeRunning()         // rebuild runtimes for streams stored as running or ended
    go manager.janitor()            // ended -> stopped once retainSeconds has passed
    svc     = StreamService(streams, breaks, manager)
    serveHTTP(routes(svc, manager))
```

**2. Timeline \[timeline\]**

```text
type Timeline { ast, S, F, leadSegs, dvrSegs, retain, endSeg }   // one per epoch; endSeg nil while running

framesPerSeg          = S * F
liveEdge(now)         = min(floor((now - ast) / S) - 1, endSeg ?? +inf)
frozenHorizon(now)    = min(liveEdge(now) + leadSegs, endSeg ?? +inf)
isAvailable(n, now)   = 0 <= n <= liveEdge(now) and now - segEnd(n) <= retain
firstFrame(n)         = n * framesPerSeg
videoTfdt(n)          = n * S * 90000
segStartTime(n)       = ast + n * S
segEnd(n)             = ast + (n + 1) * S
audioFrameRange(n, A) = [ceil(n * S * 48000 / A), ceil((n + 1) * S * 48000 / A))   // A = 1024 AAC, 1536 AC-3
videoSegAtTime(t)     = t mod (S * 90000) == 0 ? t / (S * 90000) : none
audioSegAtTime(t, A)  = n = floor(t / (S * 48000)); audioFrameRange(n, A).start * A == t ? n : none
```

**3. Schedule \[breaks\]**

```text
type Break { id, eventId, streamId, epoch, startSeg, endSeg, requestedDur, cancelled, recurring, createdAt }

add(rt, req, now):                                 // 409 unless rt is running
    earliest = rt.tl.frozenHorizon(now) + 1
    start    = max(earliest, segAtOrAfter(req.at or now + req.in))
    end      = start + ceil(req.duration / S)
    for r in recurringBetween(start, end) not cancelled:
        if not req.replaceRecurring: return Conflict(r.id)
        breaks.putOverride(r.id, cancelled = true)
    return breaks.add(rt.id, rt.epoch, Break{nextId(), start, end, req.duration})  // ErrOverlap -> 409

recurring(k):                                      // pure function of the snapshot
    start = segAtOrAfter(ast + offset + k * every)
    b = Break{id: "r-" + k, eventId: 0x80000000 + k, startSeg: start, endSeg: start + ceil(dur / S), recurring: true}
    return applyOverride(b, breaks.getOverride(b.id))   // cancel / early end, if any

breaksIn(fromSeg, toSeg) = stored breaks in range ∪ recurring(k) for every k whose break overlaps the range

endEarly(rt, id, now):
    b = find(id); require b active
    newEnd = max(rt.tl.frozenHorizon(now) + 1, b.startSeg + 1)
    b.recurring ? breaks.putOverride(id, endSeg = newEnd) : breaks.update(b with endSeg = newEnd)

cancel(rt, id, now): require b.startSeg > rt.tl.frozenHorizon(now); mark cancelled (record or override)

inAd(seg)        = any b in breaksIn(seg, seg + 1): not b.cancelled and b.startSeg <= seg < b.endSeg
signalsFor(seg)  = emsg list for segment seg (see SCTE-35 API, in-band placement)
```

**4. Frame model \[scene\]**

```text
frameSpec(f, inAd):
    t      = wallClock(f) in cfg.tz
    return { bg: inAd ? RED : GREY,
             counter: zeroPad(f, 10),
             clock:   format(t, "HH:MM:SS"),
             label:   (t.second odd) ? "TIC" : "TOC" }
```

**5. Encoder interface \[encode\]**

```text
interface Encoder:
    startEpoch(snap)             -> { initByRendition, audioLoopByRendition }
    encodeSegment(snap, n, inAd) -> map[videoRenditionId] samples   // every video rendition in one run

FfmpegEncoder.encodeSegment(snap, n, inAd):
    dir  = tmpDir/<stream>/<epoch>/<n>;  defer removeAll(dir)
    args = buildArgs(snap, bg = inAd ? RED : GREY,
                     firstFrame(n), segStartTime(n), outDir = dir)     // see Content generation
    sem.acquire(); defer sem.release()
    run("ffmpeg", args, timeout = 2 * S)          // stderr tail goes into the error
    out = {}
    for r in snap.video:
        frag = mp4ff.parse(read(dir/<r.id>.mp4))
        assert frag.sampleCount == S * F and frag.samples[0].isIDR
        out[r.id] = frag.samples
    return out

FfmpegEncoder.startEpoch(snap):
    first = run the same video command for segment 0 into a scratch dir
    init  = for each video rendition: ftyp + moov from its output, brands set to cmfc/iso6/dash
    for a in snap.audio (in parallel, bounded by the semaphore):
        loops[a.id] = AudioLoop.build(a)           // module 7
        init[a.id]  = audio init from loops[a.id].config (esds or dac3), lang, channels
    return { init, loops }
```

**6. Tile encoder core \[encode/tile\] (Option B)**

```text
startup:
    for each cellSize in layout, for each char in "0123456789:TICO ", for each bg in {GREY, RED}:
        pix = renderGlyph(char, cellSize, bg)             // x/image/font, Go Mono
        glyphCache[cellSize, char, bg] = splitIntoMBs(pix) // list of 384-byte I_PCM payloads

encodeSegment(n, specs):
    out  = []
    prev = nil
    for i, spec in specs:
        cells = layout.cells(spec)                         // [(mbX, mbY, glyph MBs)]
        if i == 0: out += idr(spec.bg, cells, idrPicId = n mod 65536)
        else:      out += pFrame(diff(prev, cells), frameNum = i)
        prev = cells
    return out

idr(bg, cells):
    w = BitWriter(); writeSliceHeader(w, I, deblock=off)
    for mb in raster order:
        if mb in cells or mb == (0,0) or leftOrTopIsGlyph(mb):
            w.ue(25)                                       // mb_type I_PCM in I slice
            w.alignZero(); w.bytes(pcmFor(mb, bg, cells))
        else:
            w.ue(mbTypeI16x16(predDC, cbpLuma=0, cbpChroma=0))
            w.ue(0)                                        // intra_chroma_pred_mode DC
            w.se(0)                                        // mb_qp_delta
            w.coeffTokenZero(nC = nCFromNeighbours(mb))    // empty Intra16x16 DC block
    return nal(IDR, rbspTrailing(w))

pFrame(changed):
    w = BitWriter(); writeSliceHeader(w, P, deblock=off)
    skip = 0
    for mb in raster order:
        if mb not in changed: skip++; continue
        w.ue(skip); skip = 0
        w.ue(5 + 25)                                       // I_PCM inside a P slice
        w.alignZero(); w.bytes(changed[mb])
    if skip > 0: w.ue(skip)
    return nal(nonIDR, rbspTrailing(w))
```

**7. Audio loop \[audio\]**

```text
build(a):                                           // per audio rendition, once per epoch
    A      = frameSize(a.codec)                     // aac 1024, ac3/eac3 1536, opus 960
    period = 8 * 48000 / A                          // 375 AAC, 250 AC-3/E-AC-3, 400 Opus frames
    pcm    = 3 × 8 s of beeps(a.ticHz, a.tocHz, 100 ms, 5 ms fades, a.channels,
                              rotate channel each second if a.channels == 6)
    aus    = ffmpeg native a.codec at a.bitrate (later: go-aac for AAC)
    return { config, A, frames: aus[period : 2 * period] }   // middle period, steady state

segment(n):
    [first, last) = audioFrameRange(n, A)
    return [frames[i mod period] for i in first..last-1], tfdt = first * A
```

**8. Packager \[pack\]**

```text
videoSegment(rt, rendition, n):
    key = (rt.id, rt.epoch, rendition, n, inAd(n), hash(signalsFor(n)))
    if cache.has(key): return cache.get(key)
    buildSegmentSet(rt, n)                        // fills every video rendition
    return cache.get(key)

buildSegmentSet(rt, n):                           // singleflight on (rt.id, rt.epoch, n)
    byRendition = enc.encodeSegment(rt.snap, n, inAd(n))
    for r, samples in byRendition:
        frag = mp4ff.NewFragment(seq = n + 1, trackId = 1)
        for e in signalsFor(n): frag.addEmsg(v1, scheme, 90000, e.pts, e.dur, e.id, e.cueBytes)
        for s in samples: frag.addSample(s, dur = 90000 / F, sync = s.isIDR)
        frag.setTfdt(videoTfdt(n))
        cache.put((rt.id, rt.epoch, r, n, inAd(n), hash(signalsFor(n))), styp + frag.encode())

audioSegment(rt, audioId, n):                     // cheap: no ffmpeg, built on request
    frames, tfdt = rt.loops[audioId].segment(n)
    frag = mp4ff.NewFragment(seq = n + 1, trackId = 1)
    for f in frames: frag.addSample(f, dur = A, sync = true)
    frag.setTfdt(tfdt); return styp + frag.encode()
```

**9. Manifests and HTTP \[manifest, server\]**

```text
routes:
    /api/streams...          -> StreamService handlers (module 10)
    /live/{stream}/...       -> rt = manager.runtime(stream), else 404

GET /live/{s}/master.m3u8:   require "hls"; EXT-X-MEDIA per audio rendition,
                             STREAM-INF per (video rendition × audio group)

GET /live/{s}/{r}.m3u8:
    require "hls" in rt.snap.formats and r is a video or audio rendition of the epoch
    now  = query.now or clock.now()
    last = rt.tl.liveEdge(now); first = max(0, last - rt.tl.dvrSegs + 1)
    emit header, MEDIA-SEQUENCE = first, MAP = "{r}/init.mp4"
    for n in first..last:
        for b in breaksStartingAt(n): emitStart(b, styles)    // styles = ?cues= or ads.hls; one writer per style
        if inAd(n) and n != b.startSeg: emitCont(b, n, styles)  // cueout and oatcls only
        for b in breaksEndingAt(n):   emitEnd(b, styles)
        emit PROGRAM-DATE-TIME(segStartTime(n)), EXTINF(real duration), "{r}/{n}.m4s"
    if rt.tl.endSeg == last: emit ENDLIST

GET /live/{s}/manifest.mpd:
    require "dash"
    mode = query.addressing or rt.snap.dash.addressing        // number | timeline-time | timeline-number
    for each AdaptationSet (video; audio per lang/channels/codec):
        switch mode:
            number:          SegmentTemplate(duration = S * ts, startNumber = 0, media = "$RepresentationID$/$Number$.m4s")
            timeline-number: SegmentTemplate(startNumber = first, media = "$RepresentationID$/$Number$.m4s", timeline(first..last))
            timeline-time:   SegmentTemplate(media = "$RepresentationID$/t/$Time$.m4s", timeline(first..last))
    timeline(first..last) = [S(t = tfdt(n), d = duration(n)) for n] merged with @r      // per track: video or audio A
    events = breaks overlapping the window; if ended: mediaPresentationDuration, no minimumUpdatePeriod

GET /live/{s}/{r}/{n}.m4s and /live/{s}/{r}/t/{t}.m4s:
    n = n, or videoSegAtTime(t) / audioSegAtTime(t, A); none -> 404
    if not rt.tl.isAvailable(n, now): 404
    Cache-Control: max-age = retain
    return r is video ? videoSegment(rt, r, n) : audioSegment(rt, r, n)

manifests: Cache-Control: max-age = S / 2
```

**10. Stream service \[streams\]**

```text
create(req):
    validate(req.spec)                                       // 422 with field errors
    repo.create(Stream{id, name, spec, status: {state: stopped}, version: 1})   // 409 if id taken

replace(id, spec, ifMatch):
    validate(spec); s = repo.get(id)
    s.spec = spec
    s.status.pendingChanges = s.state != stopped and spec != s.status.epoch.spec
    repo.update(s, expectedVersion = ifMatch)                // 412 on mismatch

start(id, req):
    s = repo.get(id); require s.state != running             // 409
    ast = req.ast ?? ceilToEvenSecond(now); require ast is on an even second
    if s.state == ended: manager.drop(id)
    s.status = {state: running, pendingChanges: false,
                epoch: {number: s.status.epoch.number + 1, ast, endSegment: nil, spec: deepCopy(s.spec)}}
    repo.update(s, s.version)
    if err = manager.launch(s): s.status.state = stopped; repo.update(...); return 500 + err

stop(id):
    s = repo.get(id); require s.state == running              // 409
    s.status.epoch.endSegment = manager.runtime(id).tl.frozenHorizon(now)
    s.status.state = ended
    repo.update(s, s.version)
    breaks.trimAfter(id, epoch, endSegment); manager.end(id, endSegment)

delete(id, ifMatch):
    require state != running; manager.drop(id); breaks.deleteAll(id); repo.delete(id, ifMatch)
```

**11. Stream manager and producer \[runtime\]**

```text
launch(stream):
    snap   = stream.status.epoch
    assets = enc.startEpoch(snap)                            // init segments + audio loop, ~1-2 s
    rt     = Runtime{id, epoch, snap, Timeline(snap), assets, ctx, cancel}
    runtimes[id] = rt                                        // map guarded by a mutex
    go produce(rt)

produce(rt):
    next = max(0, rt.tl.liveEdge(now) + 1)                   // older window segments: built on demand
    loop until ctx done:
        target = rt.tl.frozenHorizon(now)
        while next <= target: buildSegmentSet(rt, next); next++
        if rt.tl.endSeg != nil and next > rt.tl.endSeg: return
        sleepUntil(rt.tl.segEnd(liveEdge(now) + 1))           // when the horizon moves again

end(id, endSeg):     runtimes[id].tl.endSeg = endSeg        // producer finishes up to it, then exits
drop(id):            cancel ctx, purge cache entries of (id, epoch), delete runtimes[id]
resumeRunning():     for s in repo.list() where state in {running, ended}: launch(s)
janitor():           every 10 s: for ended rt past segEnd(endSeg) + retain:
                         drop(id); set state stopped (compare-and-swap on version)
```

## Performance

Video encoding is the only real cost: a 3-rung 1080p H.264 ladder needs about 0.55 CPU core per live stream, HEVC and AV1 rungs 0.75-1.6 core each. Delivery is bound by the network card, not the CPU. All figures below were measured on 2026-11-06 with the actual per-segment commands, on a 2-vCPU cloud Xeon (2.1 GHz, one thread per core, AVX-512) with ffmpeg 6.1.1. The picture is the same synthetic content the origin produces, so the numbers carry over; faster cores do proportionally better.

**Encoding cost per 2 s segment, 25 fps**

| Video setup (one ffmpeg run per segment, overlays included) | Wall time per segment | CPU seconds per segment | Cores per live stream |
| --- | --- | --- | --- |
| H.264 720p, single rendition, veryfast | 0.22 s | 0.28 | 0.14 |
| H.264 1080p, single rendition, veryfast | 0.42 s | 0.60 | 0.30 |
| H.264 ladder 1080p / 720p / 360p, veryfast | 0.70 s | 1.02-1.05 | 0.52 (0.55 under load) |
| H.264 ladder, superfast | 0.60 s | 0.91 | 0.46 |
| H.264 ladder, ultrafast | 0.47 s | 0.71 | 0.36 |
| HEVC 1080p, x265 ultrafast | 0.97 s | 1.56 | 0.78 |
| HEVC 1080p, x265 veryfast | 1.59 s | 2.81 | 1.41 |
| AV1 1080p, SVT-AV1 preset 12 | 1.06 s | 1.47 | 0.74 |
| AV1 1080p, SVT-AV1 preset 10 | 1.13 s | 1.88 | 0.94 |
| AV1 1080p, SVT-AV1 preset 8 | 1.86 s | 3.30 | 1.65 |
| ffmpeg start-up alone (1 frame) | 0.07 s | 0.06 | — |

- Running 1, 2, 4 and 8 ladder encodes at once on the 2 vCPUs gave 2.9, 3.6, 3.7 and 3.6 seconds of ladder per wall second: throughput saturates at the core count, as expected, and per-run overhead is negligible.
- Audio loops cost 0.25-0.43 CPU seconds per audio rendition (AAC, AC-3 5.1, Opus), once per start. Audio and subtitle segments, packaging, SCTE-35 and manifests cost no measurable CPU next to video.
- Every setup produces a segment well inside its 2 s, so one producer per stream keeps up; HEVC veryfast at 1.6 s is the tightest, which is where `-producer-parallelism` helps.

**Capacity per host at 70 % CPU**

| Live streams per host | 2 vCPU | 8 vCPU | 16 vCPU | 32 vCPU |
| --- | --- | --- | --- | --- |
| H.264 720p single (0.14 core) | 10 | 40 | 80 | 160 |
| H.264 3-rung ladder (0.55 core) | 2 | 10 | 20 | 40 |
| AV1 1080p preset 10 (0.94 core) | 1 | 5 | 11 | 23 |
| HEVC 1080p veryfast (1.41 core) | 1 | 4 | 8 | 16 |
| Spec example: AV1 + HEVC 1080p + H.264 ladder (≈ 2.9 cores) | 0 | 1 | 3 | 7 |

Start-to-playable time: AST rounding (up to 2 s) + first segment and audio loops (about 1 s) + one segment until it is published, so 3-5 s.

**Delivery**

Measured with a Go `net/http` server and a Go load generator on the same 2 vCPUs over loopback, so the server alone does better:

| Request | Rate |
| --- | --- |
| HLS media playlist, 15 segments with PDT and cue tags (2 KB), rendered per request | ≈ 42 000 req/s |
| DASH MPD, 3 SegmentTimelines and an event (1.4 KB), rendered per request | ≈ 42 000 req/s |
| 360p segment from cache (190 KB) | ≈ 22 000 req/s, 34 Gbit/s |
| 1080p segment from cache (1.16 MB) | ≈ 3 300 req/s, 31 Gbit/s |

A viewer makes 1-2 manifest and 2 segment requests every 2 s, so manifests allow tens of thousands of viewers before CPU matters. Bandwidth decides first: at 5.5 Mbit/s per 1080p viewer, a 10 Gbit/s card serves about 1 800 viewers and a 1 Gbit/s card about 180. Behind a CDN the origin only sees one request per segment per edge.

**Memory**

- Segment cache per stream ≈ total bitrate × seconds kept. The 3-rung H.264 ladder with three audio tracks is 9.4 Mbit/s: about 35 MB for a 30 s DVR window, 350 MB for the full 300 s retention. Adding the AV1 and HEVC rungs from the spec example brings it to about 60 MB per 30 s.
- Size the cache for Σ streams × (dvrSeconds + 2 × leadSeconds) × bitrate; older segments in the retention window are rebuilt on demand, one ffmpeg run for all renditions.
- With padding at serve time (below), the cache holds the real encoded bytes only, about 10× less for this content.

**Finding: the synthetic picture undershoots its bitrate**

Without help, x264 produced 87 KB for a 2 s 1080p segment at a 5 Mbit/s target (7 %), 72 KB at 720p / 3 Mbit/s (19 %) and 33 KB at 360p / 800 kbit/s (33 %); SVT-AV1 behaves the same. Every rendition would then download in a fraction of its advertised time, players would jump straight to the top rung, and ABR tests would mean nothing.

- x264 with `nal-hrd=cbr` and minrate = maxrate = bufsize = bitrate reaches 93-94 % of target (1 161 / 697 / 187 KB) at the same CPU cost (1.02 vs 1.05 CPU seconds).
- **Decision: pad in the packager instead, for every codec.** After ffmpeg, each segment is padded up to bitrate × duration with filler that decoders discard: filler-data NAL units (type 12) for H.264, FD\_NUT (type 38) for HEVC, padding OBUs for AV1. Sizes become exact and identical across codecs, `trun` carries the padded sizes, and the cache keeps only the real bytes and writes the filler while serving.
- x264's own CBR filler stays available as an encoder option for comparisons.

The benchmark scripts (encoding and delivery) can be rerun on target hardware before sizing a deployment.

## Testing, edge cases and next steps

Because segments are deterministic, most tests are golden-file tests driven by `?now=`: fix AST, fix the schedule, request a URL, compare bytes.

**Tests**

| Layer | How |
| --- | --- |
| Stream service | Validation table tests (audio groups, rendition id clashes, codecs and profiles, addressing values, recurring rules, fault rules); create / replace with stale `If-Match` (412) / start / stop / delete-while-running (409); `pendingChanges` flips correctly |
| Repositories | One contract test suite run against the memory repo now and the database repo later: CAS on version, deep copies, ErrNotFound / ErrExists |
| Timeline | Table tests for liveEdge, frozenHorizon, endSeg capping, window changes while running and the ?dvr= override, audioFrameRange for AAC, AC-3, E-AC-3 and Opus at each allowed S, and time → N mapping (exact hits only) |
| DASH addressing | For each mode: MPD validates; every `<S>` matches the segment's `tfdt` and duration; every templated URL resolves; `$Time$` URLs one tick off return 404 |
| Codecs | For H.264, HEVC and AV1: one IDR per GOP at segment starts, no B-frames or reordering, init sample entry and codec string match (`hvc1` tag for HEVC); x265 and SVT-AV1 run twice give identical bytes, or the result is recorded |
| Padding | Each padded segment is within 1 % of bitrate × duration; ffmpeg decodes padded and unpadded segments to identical frames |
| Schedule | Snapping, overlap 409, cancel vs early return, stop trimming future breaks; recurring breaks land at the right segments, overrides apply, `replaceRecurring` works |
| SCTE-35 | Decode every emitted cue with scte35-go; check event ID, `pts_time`, duration, CRC, for both `splice_insert` and `time_signal` |
| Subtitles | Cue text matches the clock and frame of its second; cues split cleanly at segment boundaries; `X-TIMESTAMP-MAP` and `tfdt` line up with video |
| Fault injection | Sticky selection identical across two server instances; each action observable with an HTTP client; query faults propagate into every manifest URL |
| Operations | CORS preflight on `/live` and `/api`; 401 / 403 rules; `/readyz` turns 503 when ffmpeg or a needed encoder is missing, or a producer lags |
| Performance | The encoding and delivery benchmarks from the Performance section run in CI on a fixed runner; a regression of more than 20 % fails the build |
| Segments | `mp4ff-info` structure checks; `ffprobe -show_frames` for timestamps, IDR on frame 0, equal sample counts and aligned `tfdt` across renditions |
| HLS | Apple `mediastreamvalidator`; hls.js demo page logging cue events, ABR, audio and subtitle switches |
| DASH | DASH-IF conformance validator; dash.js and Shaka in all three addressing modes, logging EventStream and emsg events, audio and subtitle switches |
| End to end | Script: create a 3-rendition, 2-language stream with subtitles, start, POST a 20 s break, poll manifests, assert red segments and signals line up in every rendition, stop, assert ENDLIST / mediaPresentationDuration |

**Edge cases to handle**

- A break requested while another is pending or active: 409, never merged silently.
- Cancel after the first emsg was published: emit a cancel cue in later segments; players that saw the first one need it.
- Early return and stop: the new end must be past the frozen horizon; otherwise published or pre-encoded segments would change.
- Editing a running stream: accepted, stored as the new spec, applied only on the next start; the response shows `pendingChanges: true`.
- ffmpeg failure or timeout: the producer retries that segment once, then logs and moves on; a player request for it returns 503 rather than a broken file.
- CPU exhaustion with many streams or big ladders: the ffmpeg semaphore queues work; `segments_late_total` shows when the box is too small. Lower presets or fewer renditions are the fix.
- 33-bit PTS wrap every \~26.5 h: `pts_time` is taken mod 2^33; media time in `tfdt` stays 64-bit.
- Request for a future or expired segment, or a rendition not in the epoch: 404, never a placeholder.
- Clock skew between origin and players: add `UTCTiming` (`urn:mpeg:dash:utc:http-iso:2014` pointing at `/api/time`) to the MPD.

**Next steps**

- [ ] Stream resource, validation, in-memory repositories and the stream API, with API keys and CORS from day one
- [ ] Timeline, stream manager and producer with the ffmpeg encoder (single H.264 rendition first, then the ladder, with rendition labels)
- [ ] Packager with bitrate padding, HLS and DASH manifests (all three addressing modes), `/live` routes
- [ ] Ad-break API, recurring breaks and all cue styles, scoped to streams
- [ ] Event log (ring buffer, list API, SSE, timeline endpoint)
- [ ] Web UI: streams list and stream detail with player and timeline first, then the create/edit form
- [ ] HEVC and AV1 renditions; E-AC-3 and Opus audio
- [ ] Subtitles (WebVTT, IMSC1)
- [ ] Fault injection (rules API and URL parameters), with UI presets
- [ ] Health, readiness, metrics, structured logs, graceful shutdown
- [ ] Golden tests with `?now=` and a fixed AST; repository contract tests; benchmarks in CI
- [ ] v1.1: CEA-608 captions
- [ ] Later: database repositories (Postgres) including events, change notifications, multiple replicas, UI as its own deployment
- [ ] Later: pure-Go encoder, DRM with test keys, low latency, HLS Interstitials, I-frame playlists

**Sources**

- [x264-go](https://pkg.go.dev/github.com/gen2brain/x264-go), [openh264-go](https://pkg.go.dev/github.com/Azunyan1111/openh264-go), [lys-lib](https://pkg.go.dev/github.com/stubbedev/lys-lib)
- [go-aac](https://pkg.go.dev/github.com/tphakala/go-aac)
- [mp4ff](https://pkg.go.dev/github.com/Eyevinn/mp4ff)
- [scte35-go](https://github.com/comcast/scte35-go)
- [livesim2 URL parameters and SCTE-35 behaviour](https://github.com/Dash-Industry-Forum/livesim2/wiki/URL-Parameters)

