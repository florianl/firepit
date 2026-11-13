# firepit

`firepit` is the main binary of the firepit project. It starts three servers concurrently:

- a **gRPC** endpoint for receiving OTel profiles (default `:4317`)
- an **HTTP/OTLP** endpoint for receiving OTel profiles over HTTP (default `:4318`)
- a **Web UI** for browsing and visualizing collected profiles as flame graphs (default `:8080`)

Profiles are kept in memory and automatically evicted once they exceed the configured retention TTL or storage limit.

## Configuration

All options can be set via command-line flags or environment variables. Flags take precedence over environment variables.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-grpc-addr` | `GRPC_ADDR` | `:4317` | gRPC server listen address |
| `-http-addr` | `HTTP_ADDR` | `:4318` | HTTP/OTLP server listen address |
| `-web-addr` | `WEB_ADDR` | `:8080` | Web UI server listen address |
| `-base-path` | `BASE_PATH` | `""` | URL path prefix for the Web UI (e.g. `/firepit`). Must contain only `[/a-zA-Z0-9\-_.~]`. |
| `-profile-ttl` | `PROFILE_TTL` | `5m` | How long to keep a profile in memory after it was received |
| `-cleanup-interval` | `CLEANUP_INTERVAL` | `30s` | How often the background cleanup job runs to evict expired profiles |
| `-max-body-size` | `MAX_BODY_SIZE` | `33554432` (32 MiB) | Maximum request body size in bytes for HTTP/OTLP ingestion |
| `-max-storage-bytes` | `MAX_STORAGE_BYTES` | `524288000` (500 MiB) | Maximum total in-memory profile storage in bytes; `0` disables the limit |
| `-virtual-frames` | `VIRTUAL_FRAMES` | `""` | Comma separated attribute keys (e.g. `service.name,thread.name`) that are preselected as virtual frames in the flame graph view. See [Virtual frames](#virtual-frames). |
| `-pprof` | — | `false` | Expose Go runtime profiling data under `<base-path>/debug/pprof` |

## Usage

```
firepit [flags]
```

Example — listen on non-default ports and keep profiles for 10 minutes:

```
firepit -grpc-addr :14317 -http-addr :14318 -web-addr :9090 -profile-ttl 10m
```

The same configuration via environment variables:

```
GRPC_ADDR=:14317 HTTP_ADDR=:14318 WEB_ADDR=:9090 PROFILE_TTL=10m firepit
```

## Virtual frames

Virtual frames are synthetic frames, derived from resource or sample attributes, that are placed on top of each stack in the flame graph view. They group the flame graph by attribute values, e.g. by `service.name` and then by `thread.name`. Each virtual frame is named `<key>=<value>`. If an attribute is missing, its value is shown as `<unset>`. If a key is set both as a sample attribute and as a resource attribute, the sample attribute is used.

Virtual frames are selected in the Web UI with the *Virtual frames* dropdown, in the order they should be nested. By default none are selected; `-virtual-frames` / `VIRTUAL_FRAMES` sets a preselection.

The `/api/flamegraph`, `/api/sandwich` and `/api/sandwich-detail` endpoints accept a `virtualFrames` query parameter with a comma separated list of keys. If the parameter is missing, the configured keys are used; if it is empty, no virtual frames are added.

## Observing firepit

The Web UI server exposes two endpoints about firepit itself:

- `<base-path>/healthz` answers `200 ok` while firepit is serving requests. It is also available as `/healthz` if a base path is set.
- `<base-path>/metrics` serves metrics in the Prometheus text format:

| Metric | Type | Description |
|--------|------|-------------|
| `firepit_receiver_requests_total{protocol,result}` | counter | Export requests by protocol (`grpc`, `http`) and result (`accepted`, `invalid`, `too_large`) |
| `firepit_store_requests_dropped_total` | counter | Accepted requests discarded for lacking a dictionary |
| `firepit_store_profiles` | gauge | Profiles currently stored |
| `firepit_store_sample_types` | gauge | Sample types of the stored profiles |
| `firepit_store_bytes` | gauge | Size accounted for the stored profiles |
| `firepit_store_limit_bytes` | gauge | Storage limit; `0` if unlimited |
| `firepit_store_profiles_added_total` | counter | Profiles added to the store |
| `firepit_store_profiles_removed_total{reason}` | counter | Profiles removed, by reason (`expired`, `evicted`) |
| `firepit_heap_objects_bytes` | gauge | Memory occupied by objects on the Go heap |
| `firepit_goroutines` | gauge | Number of goroutines |
| `firepit_start_time_seconds` | gauge | Start time of the process |
