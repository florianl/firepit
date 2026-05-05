# 🔥 firepit

A lightweight OpenTelemetry (OTel) profile receiver with a non-persistent storage and visualizer that displays OTel profiles as interactive flame graphs in your browser.

## Features

- **Multi-Protocol Support**: Receive OTel profiles via gRPC (port 4317) and HTTP/JSON (port 4318)
- **Multiple Sample Types**: View separate flame graphs for CPU, memory, and other profile types
- **Multiple Views**: Interactive flame graph view and flamescope view for time-based performance analysis
- **Real-time Visualization**: Auto-refreshing flame graphs with pause/resume controls
- **Resource Filtering**: Filter flame graphs by resource attributes (e.g., service.name)
- **Frame Filtering**: Search and highlight specific functions within flame graphs
- **Data Export**: Export profiles as json enabling integration with other tools and data pipelines
- **Time-Based and Memory-Limited Storage**: Non-persistent in-memory storage with configurable retention windows and memory thresholds for lightweight operation

## Getting Started

Run firepit:

```
$ make docker-run
docker run -it --rm \
	-p 4317:4317 \
	-p 4318:4318 \
	-p 8080:8080 \
	firepit
2026/04/28 06:19:26 INFO Configuration loaded grpc_addr=:4317 http_addr=:4318 web_addr=:8080 base_path="" profile_ttl=5m0s cleanup_interval=30s max_body_size=33554432 max_storage_bytes=524288000
2026/04/28 06:19:26 INFO OTLP HTTP server listening addr=:4318
2026/04/28 06:19:26 INFO Web UI listening addr=:8080
2026/04/28 06:19:26 INFO Open browser to url=http://localhost:8080
2026/04/28 06:19:26 INFO gRPC server listening addr=:4317
```

Then open [http://localhost:8080](http://localhost:8080) in your browser.

Configure an OTel collector to report profiles to firepit:

```yaml
receivers:
  profiling: {}

exporters:
  otlp_grpc/firepit:
    endpoint: 127.0.0.1:4317
    tls:
      insecure: true
      insecure_skip_verify: true

service:
  pipelines:
    profiles:
      receivers: [profiling]
      exporters: [otlp_grpc/firepit]
```

## ⚠️ Development and Demo Use Only

Firepit is designed for **development and demo purposes only**. It is **not intended for production use**.

Key limitations:
- **No authentication**: The API and web UI are completely open with no access control
- **No persistence**: All profile data is stored in memory and will be lost on restart

Use firepit in isolated development environments or protected demo settings only. Do not expose it to untrusted networks or use it to store sensitive profiling data.