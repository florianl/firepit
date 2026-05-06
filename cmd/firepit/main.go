package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip"

	"github.com/florianl/firepit/internal/profiler"
	"github.com/florianl/firepit/internal/receiver"
	"github.com/florianl/firepit/internal/store"

	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

//go:embed web
var webFS embed.FS

// validBasePathRE allows only characters that are safe in a URL path and in a JS
// single-quoted string literal.
var validBasePathRE = regexp.MustCompile(validBasePathChars)

const validBasePathChars = `^[/a-zA-Z0-9\-_.~]*$`

// Config holds the configuration for firepit
type Config struct {
	GRPCAddr         string
	HTTPAddr         string
	WebAddr          string
	BasePath         string
	ProfileTTL       time.Duration
	CleanupInterval  time.Duration
	MaxBodySize      int64
	MaxStorageBytes  int64
	RuntimeProfiling bool
}

// loadConfig loads configuration from environment variables and command-line flags
func loadConfig() Config {
	cfg := loadConfigFromEnv(os.Getenv)

	// Parse command-line flags (override environment variables)
	flag.StringVar(&cfg.GRPCAddr, "grpc-addr", cfg.GRPCAddr, "gRPC server address")
	flag.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "HTTP/OTLP server address")
	flag.StringVar(&cfg.WebAddr, "web-addr", cfg.WebAddr, "Web UI server address")
	flag.StringVar(&cfg.BasePath, "base-path", cfg.BasePath, "Base path prefix for the web UI (e.g. /firepit)")
	flag.DurationVar(&cfg.ProfileTTL, "profile-ttl", cfg.ProfileTTL, "Profile retention TTL")
	flag.DurationVar(&cfg.CleanupInterval, "cleanup-interval", cfg.CleanupInterval, "Cleanup interval")
	flag.Int64Var(&cfg.MaxBodySize, "max-body-size", cfg.MaxBodySize, "Maximum request body size in bytes")
	flag.Int64Var(&cfg.MaxStorageBytes, "max-storage-bytes", cfg.MaxStorageBytes, "Maximum total profile storage in bytes (0 = unlimited)")
	flag.BoolVar(&cfg.RuntimeProfiling, "pprof", false, "Serve runtime profiling data via http")
	flag.Parse()

	bp, err := normalizeBasePath(cfg.BasePath)
	if err != nil {
		slog.Error("Invalid base path", "error", err)
		os.Exit(1)
	}
	cfg.BasePath = bp
	return cfg
}

// normalizeBasePath ensures the base path has a leading slash and no trailing slash.
// It rejects any input not matching the regex validBasePathChars to prevent
// injection into the JS string literal in index.html.
func normalizeBasePath(p string) (string, error) {
	p = strings.TrimRight(p, "/")
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p != "" && !validBasePathRE.MatchString(p) {
		return "", fmt.Errorf("base path %q contains characters not matching the regex %s",
			p, validBasePathChars)
	}
	return p, nil
}

// loadConfigFromEnv loads configuration from environment variables
func loadConfigFromEnv(getenv func(string) string) Config {
	cfg := Config{
		GRPCAddr:        ":4317",
		HTTPAddr:        ":4318",
		WebAddr:         ":8080",
		ProfileTTL:      5 * time.Minute,
		CleanupInterval: 30 * time.Second,
		MaxBodySize:     32 * 1024 * 1024,  // 32 MB
		MaxStorageBytes: 500 * 1024 * 1024, // 500 MB
	}

	// Read from environment variables
	if addr := getenv("GRPC_ADDR"); addr != "" {
		cfg.GRPCAddr = addr
	}
	if addr := getenv("HTTP_ADDR"); addr != "" {
		cfg.HTTPAddr = addr
	}
	if addr := getenv("WEB_ADDR"); addr != "" {
		cfg.WebAddr = addr
	}

	if ttl := getenv("PROFILE_TTL"); ttl != "" {
		if d, err := time.ParseDuration(ttl); err == nil {
			cfg.ProfileTTL = d
		} else {
			slog.Warn("Invalid PROFILE_TTL", "error", err)
		}
	}

	if interval := getenv("CLEANUP_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			cfg.CleanupInterval = d
		} else {
			slog.Warn("Invalid CLEANUP_INTERVAL", "error", err)
		}
	}

	if size := getenv("MAX_BODY_SIZE"); size != "" {
		if s, err := strconv.ParseInt(size, 10, 64); err == nil {
			cfg.MaxBodySize = s
		} else {
			slog.Warn("Invalid MAX_BODY_SIZE", "error", err)
		}
	}

	if size := getenv("MAX_STORAGE_BYTES"); size != "" {
		if s, err := strconv.ParseInt(size, 10, 64); err == nil {
			cfg.MaxStorageBytes = s
		} else {
			slog.Warn("Invalid MAX_STORAGE_BYTES", "error", err)
		}
	}

	if bp := getenv("BASE_PATH"); bp != "" {
		normalized, err := normalizeBasePath(bp)
		if err != nil {
			slog.Warn("Invalid BASE_PATH, using empty base path", "error", err)
		} else {
			cfg.BasePath = normalized
		}
	}

	return cfg
}

func main() {
	cfg := loadConfig()

	st := store.New(cfg.ProfileTTL, cfg.CleanupInterval, cfg.MaxStorageBytes)
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("Configuration loaded", "grpc_addr", cfg.GRPCAddr, "http_addr", cfg.HTTPAddr,
		"web_addr", cfg.WebAddr, "base_path", cfg.BasePath, "profile_ttl", cfg.ProfileTTL,
		"cleanup_interval", cfg.CleanupInterval, "max_body_size", cfg.MaxBodySize,
		"max_storage_bytes", cfg.MaxStorageBytes)

	var wg sync.WaitGroup
	var grpcServer *grpc.Server
	var webServer *http.Server
	var otlpServer *http.Server

	// ingest - grpc
	wg.Add(1)
	go func() {
		defer wg.Done()
		grpcServer = startGRPCServer(st, cfg.GRPCAddr)
	}()

	// ingest - http
	wg.Add(1)
	go func() {
		defer wg.Done()
		otlpServer = startOTLPHTTPServer(st, cfg)
	}()

	// UI
	wg.Add(1)
	go func() {
		defer wg.Done()
		webServer = startWebUIServer(st, cfg)
	}()

	<-ctx.Done()
	slog.Info("Shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if grpcServer != nil {
		slog.Info("Stopping gRPC server")
		grpcServer.GracefulStop()
	}

	if webServer != nil {
		slog.Info("Stopping Web UI server")
		if err := webServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("web server shutdown", "error", err)
		}
	}

	if otlpServer != nil {
		slog.Info("Stopping OTLP HTTP server")
		if err := otlpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("OTLP server shutdown", "error", err)
		}
	}

	// Wait for all components to terminate.
	wg.Wait()

	slog.Info("Shutdown complete")
}

func startGRPCServer(st *store.Store, grpcAddr string) *grpc.Server {
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Error("Failed to listen on gRPC address", "addr", grpcAddr, "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	collectorprofiles.RegisterProfilesServiceServer(grpcServer, receiver.New(st))

	slog.Info("gRPC server listening", "addr", grpcAddr)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
		}
	}()

	return grpcServer
}

func buildWebUIMux(st *store.Store, cfg Config) *http.ServeMux {
	mux := http.NewServeMux()
	base := cfg.BasePath

	fsub, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("Failed to create sub filesystem", "error", err)
		os.Exit(1)
	}

	indexBytes, err := webFS.ReadFile("web/index.html")
	if err != nil {
		slog.Error("Failed to read index.html", "error", err)
		os.Exit(1)
	}
	indexHTML := strings.ReplaceAll(string(indexBytes), "FIREPIT_BASE_PATH", cfg.BasePath)

	strippedFileServer := http.StripPrefix(base, http.FileServer(http.FS(fsub)))

	mux.Handle(base+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripped := strings.TrimPrefix(r.URL.Path, base)
		if stripped == "/" || stripped == "" || stripped == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, indexHTML)
			return
		}
		strippedFileServer.ServeHTTP(w, r)
	}))

	mux.HandleFunc(base+"/api/flamegraph", handleFlamegraph(st))
	mux.HandleFunc(base+"/api/flamescope", handleFlamescope(st))
	mux.HandleFunc(base+"/api/profiles", handleProfiles(st))
	mux.HandleFunc(base+"/api/resource-types", handleResourceTypes(st))

	if cfg.RuntimeProfiling {
		mux.HandleFunc(base+"/debug/pprof", pprof.Index)
		mux.HandleFunc(base+"/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc(base+"/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc(base+"/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc(base+"/debug/pprof/trace", pprof.Trace)
	}

	if base != "" {
		mux.Handle("/", http.RedirectHandler(base+"/", http.StatusMovedPermanently))
	}

	return mux
}

func startWebUIServer(st *store.Store, cfg Config) *http.Server {
	mux := buildWebUIMux(st, cfg)

	server := &http.Server{
		Addr:    cfg.WebAddr,
		Handler: mux,
	}

	slog.Info("Web UI listening", "addr", cfg.WebAddr)
	slog.Info("Open browser to", "url", "http://localhost"+cfg.WebAddr+cfg.BasePath)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Web UI server error", "error", err)
		}
	}()

	return server
}

func startOTLPHTTPServer(st *store.Store, cfg Config) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/profiles", receiver.NewHTTPHandler(st, cfg.MaxBodySize))

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
	}

	slog.Info("OTLP HTTP server listening", "addr", cfg.HTTPAddr)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("OTLP HTTP server error", "error", err)
		}
	}()

	return server
}

func handleFlamegraph(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		resourceType := r.URL.Query().Get("resourceType")

		types := st.SampleTypes()
		graphs := make([]profiler.NamedFlamegraph, 0, len(types))
		for _, t := range types {
			entries := st.ProfileEntries(t)
			entries = profiler.FilterByResourceType(entries, resourceType)
			root := profiler.ToFlamegraph(entries)
			graphs = append(graphs, profiler.NamedFlamegraph{Type: t, Root: root})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graphs)
	}
}

func handleFlamescope(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		resourceType := r.URL.Query().Get("resourceType")

		types := st.SampleTypes()
		maps := make([]profiler.NamedFlamescope, 0, len(types))
		for _, t := range types {
			entries := st.ProfileEntries(t)
			entries = profiler.FilterByResourceType(entries, resourceType)
			hm := profiler.ToHeatMap(entries)
			maps = append(maps, profiler.NamedFlamescope{Type: t, Data: hm})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(maps)
	}
}

func handleProfiles(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		count, minTime, maxTime, ok := st.Stats()

		info := map[string]interface{}{
			"count": count,
		}

		if ok {
			timeRange := fmt.Sprintf("%s - %s", minTime.Format(time.RFC3339), maxTime.Format(time.RFC3339))
			info["timeRange"] = timeRange
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	}
}

func handleResourceTypes(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		types := st.ResourceTypes()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types)
	}
}
