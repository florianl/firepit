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
