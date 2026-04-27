# 🔥 firepit

A lightweight OpenTelemetry (OTel) profile receiver with a non-persistent storage and visualizer that displays OTel profiles as interactive flame graphs in your browser.

## Features

- **Multi-Protocol Support**: Receive OTel profiles via gRPC (port 4317) and HTTP/JSON (port 4318)
- **Multiple Sample Types**: View separate flame graphs for CPU, memory, and other profile types
- **Real-time Visualization**: Auto-refreshing flame graphs with pause/resume controls
- **Resource Filtering**: Filter flame graphs by resource attributes (e.g., service.name)
- **Frame Filtering**: Search and highlight specific functions within flame graphs
- **Data Export**: Export profiles as json enabling integration with other tools and data pipelines
- **Time-Based and Memory-Limited Storage**: Non-persistent in-memory storage with configurable retention windows and memory thresholds for lightweight operation

## ⚠️ Development and Demo Use Only

Firepit is designed for **development and demo purposes only**. It is **not intended for production use**.

Key limitations:
- **No authentication**: The API and web UI are completely open with no access control
- **No persistence**: All profile data is stored in memory and will be lost on restart

Use firepit in isolated development environments or protected demo settings only. Do not expose it to untrusted networks or use it to store sensitive profiling data.