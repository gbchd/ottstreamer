# ottstreamer

A synthetic live OTT origin: it encodes a test picture (clock, frame counter, beeps) with ffmpeg and serves it as CMAF over HLS and DASH, with SCTE-35 ad breaks you insert on demand through an API. Ad segments turn red, and every cue style (HLS DATERANGE, CUE-OUT, OATCLS, Adobe; DASH EventStream, in-band emsg, multi-period) carries the same break. The design is in [docs/spec.md](docs/spec.md), the package rules in [AGENTS.md](AGENTS.md).

## Prerequisites

- Go 1.27
- ffmpeg and ffprobe, with libx264 and the drawtext filter (`ffmpeg -filters | grep drawtext`)

## Run locally

```sh
ORIGIN_API_KEYS=dev:write:secret ORIGIN_LISTEN=127.0.0.1:18080 go run ./cmd/origin
```

`go run ./cmd/origin -h` lists the other settings; each flag has an `ORIGIN_*` environment override.

Create a stream, start it, and insert a 10 s break:

```sh
curl -s -X POST localhost:18080/api/streams -H 'Authorization: Bearer secret' -H 'Content-Type: application/json' -d '{
  "id": "demo", "name": "Demo",
  "spec": {
    "formats": ["hls", "dash"],
    "video": [
      {"id": "720p", "width": 1280, "height": 720, "bitrateKbps": 3000},
      {"id": "360p", "width": 640, "height": 360, "bitrateKbps": 800}
    ],
    "audio": [{"id": "en", "lang": "en", "name": "English", "channels": 2, "bitrateKbps": 128, "default": true}]
  }
}'
curl -s -X POST localhost:18080/api/streams/demo/start -H 'Authorization: Bearer secret'
curl -s -X POST localhost:18080/api/streams/demo/breaks -H 'Authorization: Bearer secret' -H 'Content-Type: application/json' -d '{"duration": 10}'
```

Fetch the manifests and one segment (the first segment is published 2 s after the start):

```sh
curl -s localhost:18080/live/demo/master.m3u8
curl -s localhost:18080/live/demo/720p.m3u8
curl -s localhost:18080/live/demo/manifest.mpd
curl -s -o init.mp4 localhost:18080/live/demo/720p/init.mp4
curl -s -o seg.m4s localhost:18080/live/demo/720p/1.m4s
cat init.mp4 seg.m4s > 720p-1.mp4 && ffprobe -v error -count_frames -show_entries stream=codec_name,width,height,nb_read_frames 720p-1.mp4
```

Play `http://localhost:18080/live/demo/master.m3u8` or `.../manifest.mpd` in any HLS or DASH player; `/live` is open to every origin. Stop with `POST /api/streams/demo/stop`, or SIGTERM the process.

Every change and every break or signal the live edge crosses goes to the stream's event log (the last 10 000 events per stream, kept after a delete):

```sh
curl -s 'localhost:18080/api/streams/demo/events?types=break.started,break.ended' -H 'Authorization: Bearer secret'
curl -sN localhost:18080/api/streams/demo/events/live -H 'Authorization: Bearer secret'   # Server-Sent Events
curl -s localhost:18080/api/streams/demo/timeline -H 'Authorization: Bearer secret'       # markers, breaks and events around now
```

Operations: `/healthz` answers while the process serves; `/readyz` answers 503 with the failing checks (repository, ffmpeg and its pinned version `ORIGIN_FFMPEG_VERSION`, tmp dir, producers more than 2 segments late, shutdown); `/metrics` is Prometheus. Logs are JSON on stdout, one `http request` line per request with its `request_id` (`X-Request-Id`, kept or generated).

## Development

- The API is defined in `api/openapi.yaml`; after editing it, regenerate the server with `go generate ./...`.
- The gate, run before every commit:

```sh
test -z "$(gofmt -l .)" && go vet ./... && go build ./... && golangci-lint run ./... && go test -race ./...
```

`go test -short ./...` skips the tests that run ffmpeg.
