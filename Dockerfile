# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git make curl

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Generate web assets (download D3 libraries)
RUN make generate

# Build the binary
RUN make build

# Final stage
FROM alpine:3.19

WORKDIR /app

# Install runtime dependencies (curl for health checks)
RUN apk add --no-cache ca-certificates curl

# Copy binary from builder (web assets are embedded)
COPY --from=builder /build/firepit .

# Create a non-root user for security
RUN addgroup -g 1000 firepit && \
    adduser -D -u 1000 -G firepit firepit && \
    chown -R firepit:firepit /app

USER firepit

# Expose ports
# 4317: gRPC receiver
# 4318: HTTP receiver
# 8080: Web UI
EXPOSE 4317 4318 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080 || exit 1

# Run firepit
CMD ["./firepit"]
