package main

import (
	"bufio"
	"bytes"
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
	rpprof "runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"

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
	FromFile         string
	BasePath         string
	ProfileTTL       time.Duration
	CleanupInterval  time.Duration
	MaxBodySize      int64
	MaxStorageBytes  int64
	RuntimeProfiling bool
	// VirtualFrames lists attribute keys rendered as virtual frames by default.
	VirtualFrames []string
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
	flag.StringVar(&cfg.FromFile, "from-file", cfg.FromFile, "Path to a JSONL file of proto-JSON encoded profiles to load at startup")
	virtualFrames := strings.Join(cfg.VirtualFrames, ",")
	flag.StringVar(&virtualFrames, "virtual-frames", virtualFrames, "Comma separated attribute keys rendered as virtual frames on top of each stack (e.g. service.name,thread.name)")
	flag.Parse()

	cfg.VirtualFrames = profiler.ParseKeyList(virtualFrames)

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
		cfg.BasePath = bp
	}

	if ff := getenv("FROM_FILE"); ff != "" {
		cfg.FromFile = ff
	}

	if vf := getenv("VIRTUAL_FRAMES"); vf != "" {
		cfg.VirtualFrames = profiler.ParseKeyList(vf)
	}

	return cfg
}

func main() {
	cfg := loadConfig()

	ttl := cfg.ProfileTTL
	if cfg.FromFile != "" {
		ttl = 0 // disable TTL eviction when data comes from a file
	}
	st := store.New(ttl, cfg.CleanupInterval, cfg.MaxStorageBytes)
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("Configuration loaded", "grpc_addr", cfg.GRPCAddr, "http_addr", cfg.HTTPAddr,
		"web_addr", cfg.WebAddr, "base_path", cfg.BasePath, "profile_ttl", cfg.ProfileTTL,
		"cleanup_interval", cfg.CleanupInterval, "max_body_size", cfg.MaxBodySize,
		"max_storage_bytes", cfg.MaxStorageBytes, "from_file", cfg.FromFile,
		"virtual_frames", cfg.VirtualFrames)

	metrics := &receiver.Metrics{}

	var wg sync.WaitGroup
	var initGroup sync.WaitGroup
	var grpcServer *grpc.Server
	var webServer *http.Server
	var otlpServer *http.Server

	if cfg.FromFile != "" {
		if err := loadFromFile(st, cfg.FromFile); err != nil {
			slog.Error("Failed to load profiles from file", "file", cfg.FromFile,
				"error", err)
			os.Exit(1)
		}
	} else {
		// ingest - grpc
		initGroup.Add(1)
		wg.Go(func() {
			defer initGroup.Done()
			grpcServer = startGRPCServer(st, metrics, cfg.GRPCAddr, &wg)
		})

		// ingest - http
		initGroup.Add(1)
		wg.Go(func() {
			defer initGroup.Done()
			otlpServer = startOTLPHTTPServer(st, metrics, cfg, &wg)
		})
	}

	// UI
	initGroup.Add(1)
	wg.Go(func() {
		defer initGroup.Done()
		webServer = startWebUIServer(st, metrics, cfg, &wg)
	})

	// Avoid race condition by waiting for servers to start
	initGroup.Wait()

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

// loadFromFile reads a JSON encoded file and tries to interpret it as ExportProfilesServiceRequest.
func loadFromFile(st *store.Store, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed opening %s: %v", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		req := &collectorprofiles.ExportProfilesServiceRequest{}
		if err := protojson.Unmarshal(line, req); err != nil {
			slog.Warn("Skipping unparseable line", "file", path, "line", line, "error", err)
			continue
		}
		st.Add(req.ResourceProfiles, req.Dictionary)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed reading %s: %v", path, err)
	}

	slog.Info("Loaded profiles from file", "file", path)

	return nil
}

func startGRPCServer(st *store.Store, metrics *receiver.Metrics, grpcAddr string, wg *sync.WaitGroup) *grpc.Server {
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		slog.Error("Failed to listen on gRPC address", "addr", grpcAddr, "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		// Close connections of peers that went away.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    defaultTimeouts.idle,
			Timeout: defaultTimeouts.readHeader,
		}),
	)
	collectorprofiles.RegisterProfilesServiceServer(grpcServer, receiver.New(st, metrics))

	slog.Info("gRPC server listening", "addr", grpcAddr)
	wg.Go(func() {
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
		}
	})

	return grpcServer
}

// serverTimeouts limits how long a client may hold a connection. A zero value
// disables the limit.
type serverTimeouts struct {
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
}

var defaultTimeouts = serverTimeouts{
	readHeader: 10 * time.Second,
	read:       time.Minute,
	write:      time.Minute,
	idle:       2 * time.Minute,
}

func newHTTPServer(addr string, handler http.Handler, timeouts serverTimeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: timeouts.readHeader,
		ReadTimeout:       timeouts.read,
		WriteTimeout:      timeouts.write,
		IdleTimeout:       timeouts.idle,
	}
}

// buildWebUIMux returns the web UI handler without receiver metrics.
func buildWebUIMux(st *store.Store, cfg Config) *http.ServeMux {
	mux, _ := buildWebUI(st, nil, cfg)
	return mux
}

// buildWebUI returns the web UI handler and a function to release its resources.
func buildWebUI(st *store.Store, metrics *receiver.Metrics, cfg Config) (*http.ServeMux, func()) {
	mux := http.NewServeMux()
	done := make(chan struct{})
	agg := profiler.NewAggregator(st)
	stop := sync.OnceFunc(func() {
		close(done)
		agg.Close()
	})
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
	indexHTML := bytes.ReplaceAll(indexBytes, []byte("FIREPIT_BASE_PATH"), []byte(cfg.BasePath))

	static, err := newStaticFiles(fsub, indexHTML)
	if err != nil {
		slog.Error("Failed to load web UI files", "error", err)
		os.Exit(1)
	}

	mux.Handle(base+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		static.serve(w, r, strings.TrimPrefix(r.URL.Path, base))
	}))

	// Cache uncompressed responses and compress per request
	cache := newResponseCache(st)
	mux.HandleFunc(base+"/api/flamegraph", compressResponse(cache.wrap(handleFlamegraph(st, agg, cfg.VirtualFrames))))
	mux.HandleFunc(base+"/api/flamescope", compressResponse(cache.wrap(handleFlamescope(st))))
	mux.HandleFunc(base+"/api/sandwich", compressResponse(cache.wrap(handleSandwich(st, agg, cfg.VirtualFrames))))
	mux.HandleFunc(base+"/api/sandwich-detail", compressResponse(cache.wrap(handleSandwichDetail(st, agg, cfg.VirtualFrames))))
	mux.HandleFunc(base+"/api/profiles", compressResponse(cache.wrap(handleProfiles(st))))
	mux.HandleFunc(base+"/api/resource-types", compressResponse(cache.wrap(handleResourceTypes(st))))
	mux.HandleFunc(base+"/api/resource-attributes", compressResponse(cache.wrap(handleResourceAttributes(st))))
	mux.HandleFunc(base+"/api/attribute-keys", compressResponse(cache.wrap(handleAttributeKeys(st))))
	mux.HandleFunc(base+"/api/config", compressResponse(handleConfig(cfg)))
	mux.HandleFunc(base+"/api/events", handleEvents(st, done, eventMinInterval, eventHeartbeat))

	mux.HandleFunc(base+"/metrics", handleMetrics(st, metrics, time.Now()))
	mux.HandleFunc(base+"/healthz", handleHealth)
	// Probes may not know the base path.
	if base != "" {
		mux.HandleFunc("/healthz", handleHealth)
	}

	prefix := base + "/debug/pprof"
	if cfg.RuntimeProfiling {
		mux.HandleFunc(prefix+"/", pprof.Index)
		mux.HandleFunc(prefix+"/cmdline", pprof.Cmdline)
		mux.HandleFunc(prefix+"/profile", pprof.Profile)
		mux.HandleFunc(prefix+"/symbol", pprof.Symbol)
		mux.HandleFunc(prefix+"/trace", pprof.Trace)
		// Register runtime profiles
		for _, profile := range rpprof.Profiles() {
			p := profile
			mux.Handle(prefix+"/"+p.Name(), pprof.Handler(p.Name()))
		}
	}

	if base != "" {
		mux.Handle("/", http.RedirectHandler(base+"/", http.StatusMovedPermanently))
	}

	return mux, stop
}

func startWebUIServer(st *store.Store, metrics *receiver.Metrics, cfg Config, wg *sync.WaitGroup) *http.Server {
	mux, stop := buildWebUI(st, metrics, cfg)

	// Runtime profiling endpoints stream for as long as the client asks for.
	timeouts := defaultTimeouts
	timeouts.write = 0
	server := newHTTPServer(cfg.WebAddr, mux, timeouts)
	server.RegisterOnShutdown(stop)

	slog.Info("Web UI listening", "addr", cfg.WebAddr)
	slog.Info("Open browser to", "url", "http://localhost"+cfg.WebAddr+cfg.BasePath)

	wg.Go(func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Web UI server error", "error", err)
		}
	})

	return server
}

func startOTLPHTTPServer(st *store.Store, metrics *receiver.Metrics, cfg Config, wg *sync.WaitGroup) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/profiles", receiver.NewHTTPHandler(st, cfg.MaxBodySize, metrics))

	server := newHTTPServer(cfg.HTTPAddr, mux, defaultTimeouts)

	slog.Info("OTLP HTTP server listening", "addr", cfg.HTTPAddr)

	wg.Go(func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("OTLP HTTP server error", "error", err)
		}
	})

	return server
}

// flamegraphOptions builds the flame graph options for r. The virtualFrames
// query parameter, if present, overrides the configured default keys.
func flamegraphOptions(r *http.Request, defaultVirtualFrames []string) profiler.Options {
	keys := defaultVirtualFrames
	if q := r.URL.Query(); q.Has("virtualFrames") {
		keys = profiler.ParseKeyList(q.Get("virtualFrames"))
	}
	return profiler.Options{VirtualFrameKeys: keys}
}

// namedJSON pairs a sample type with data that is already encoded as JSON.
type namedJSON struct {
	sampleType string
	data       json.RawMessage
}

// writeNamedJSON writes items as a JSON array of {"type", field} objects.
// The data is written as is, as encoding/json would validate it once more.
func writeNamedJSON(w http.ResponseWriter, field string, items []namedJSON) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		sampleType, _ := json.Marshal(item.sampleType)
		buf.WriteString(`{"type":`)
		buf.Write(sampleType)
		buf.WriteString(`,"` + field + `":`)
		buf.Write(item.data)
		buf.WriteByte('}')
	}
	buf.WriteString("]\n")
	w.Write(buf.Bytes())
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeEncodingError reports that a response could not be encoded.
func writeEncodingError(w http.ResponseWriter, err error) {
	slog.Error("Failed to encode response", "error", err)
	writeJSONError(w, http.StatusInternalServerError, "Failed to encode response")
}

// getJSON returns a handler that serves next for GET requests with a JSON content type.
func getJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		next(w, r)
	}
}

func handleFlamegraph(st *store.Store, agg *profiler.Aggregator, defaultVirtualFrames []string) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		filters := r.URL.Query()["resourceType"]
		opts := flamegraphOptions(r, defaultVirtualFrames)

		types := st.SampleTypes()
		graphs := make([]namedJSON, 0, len(types))
		for _, t := range types {
			root, err := agg.FlamegraphJSON(t, filters, opts)
			if err != nil {
				writeEncodingError(w, err)
				return
			}
			graphs = append(graphs, namedJSON{t, root})
		}

		writeNamedJSON(w, "root", graphs)
	})
}

func handleFlamescope(st *store.Store) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		filters := r.URL.Query()["resourceType"]

		types := st.SampleTypes()
		maps := make([]profiler.NamedFlamescope, 0, len(types))
		for _, t := range types {
			entries := st.ProfileEntries(t)
			entries = profiler.FilterByResource(entries, filters)
			hm := profiler.ToHeatMap(entries)
			maps = append(maps, profiler.NamedFlamescope{Type: t, Data: hm})
		}

		json.NewEncoder(w).Encode(maps)
	})
}

func handleProfiles(st *store.Store) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		count, minTime, maxTime, ok := st.Stats()

		info := map[string]any{
			"count": count,
		}

		if ok {
			timeRange := fmt.Sprintf("%s - %s", minTime.Format(time.RFC3339), maxTime.Format(time.RFC3339))
			info["timeRange"] = timeRange
		}

		json.NewEncoder(w).Encode(info)
	})
}

func handleResourceTypes(st *store.Store) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		types := st.ResourceTypes()

		json.NewEncoder(w).Encode(types)
	})
}

// resourceAttributeValues holds the values seen for a resource attribute key.
type resourceAttributeValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// handleResourceAttributes lists the filterable resource attributes grouped by key.
func handleResourceAttributes(st *store.Store) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		// Attributes are sorted by key
		grouped := []resourceAttributeValues{}
		for _, attr := range st.ResourceAttributes() {
			if n := len(grouped); n == 0 || grouped[n-1].Key != attr.Key {
				grouped = append(grouped, resourceAttributeValues{Key: attr.Key})
			}
			last := &grouped[len(grouped)-1]
			last.Values = append(last.Values, attr.Value)
		}

		json.NewEncoder(w).Encode(grouped)
	})
}

func handleAttributeKeys(st *store.Store) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		keys := st.AttributeKeys()
		if keys == nil {
			keys = []string{}
		}

		json.NewEncoder(w).Encode(keys)
	})
}

func handleConfig(cfg Config) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		virtualFrames := cfg.VirtualFrames
		if virtualFrames == nil {
			virtualFrames = []string{}
		}

		json.NewEncoder(w).Encode(map[string]any{"virtualFrames": virtualFrames})
	})
}

func handleSandwich(st *store.Store, agg *profiler.Aggregator, defaultVirtualFrames []string) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		filters := r.URL.Query()["resourceType"]
		opts := flamegraphOptions(r, defaultVirtualFrames)

		types := st.SampleTypes()
		sandwiches := make([]namedJSON, 0, len(types))
		for _, t := range types {
			var data json.RawMessage
			var err error
			// root is only valid within the callback.
			agg.WithFlamegraph(t, filters, opts, func(root *profiler.FlameNode) {
				data, err = json.Marshal(profiler.ToSandwich(root))
			})
			if err != nil {
				writeEncodingError(w, err)
				return
			}
			sandwiches = append(sandwiches, namedJSON{t, data})
		}

		writeNamedJSON(w, "data", sandwiches)
	})
}

func handleSandwichDetail(st *store.Store, agg *profiler.Aggregator, defaultVirtualFrames []string) http.HandlerFunc {
	return getJSON(func(w http.ResponseWriter, r *http.Request) {
		filters := r.URL.Query()["resourceType"]
		functionName := r.URL.Query().Get("functionName")
		sampleType := r.URL.Query().Get("sampleType")
		opts := flamegraphOptions(r, defaultVirtualFrames)

		if functionName == "" {
			writeJSONError(w, http.StatusBadRequest, "functionName parameter required")
			return
		}

		types := st.SampleTypes()
		if sampleType != "" {
			types = []string{sampleType}
		}

		result := make(map[string]any)
		foundInAnyType := false
		for _, t := range types {
			var sandwich *profiler.SandwichGraphs
			agg.WithFlamegraph(t, filters, opts, func(root *profiler.FlameNode) {
				sandwich = profiler.ExtractSandwichGraphs(root, functionName)
			})
			result[t] = sandwich
			if sandwich.Callers != nil || sandwich.Callees != nil {
				foundInAnyType = true
			}
		}

		if !foundInAnyType {
			writeJSONError(w, http.StatusNotFound, "Function not found")
			return
		}

		if err := json.NewEncoder(w).Encode(result); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to encode response"})
			return
		}
	})
}
