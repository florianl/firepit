package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/profiler"
	"github.com/florianl/firepit/internal/store"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		validate func(*Config)
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			validate: func(c *Config) {
				if c.GRPCAddr != ":4317" {
					t.Errorf("expected gRPC addr :4317, got %s", c.GRPCAddr)
				}
				if c.HTTPAddr != ":4318" {
					t.Errorf("expected HTTP addr :4318, got %s", c.HTTPAddr)
				}
				if c.WebAddr != ":8080" {
					t.Errorf("expected Web addr :8080, got %s", c.WebAddr)
				}
				if c.ProfileTTL != 5*time.Minute {
					t.Errorf("expected ProfileTTL 5m, got %v", c.ProfileTTL)
				}
				if c.CleanupInterval != 30*time.Second {
					t.Errorf("expected CleanupInterval 30s, got %v", c.CleanupInterval)
				}
			},
		},
		{
			name: "env vars override defaults",
			env: map[string]string{
				"GRPC_ADDR":         ":5000",
				"HTTP_ADDR":         ":5001",
				"WEB_ADDR":          ":9000",
				"PROFILE_TTL":       "10m",
				"CLEANUP_INTERVAL":  "1m",
				"MAX_BODY_SIZE":     "64000000",
				"MAX_STORAGE_BYTES": "1000000000",
			},
			validate: func(c *Config) {
				if c.GRPCAddr != ":5000" {
					t.Errorf("expected gRPC addr :5000, got %s", c.GRPCAddr)
				}
				if c.HTTPAddr != ":5001" {
					t.Errorf("expected HTTP addr :5001, got %s", c.HTTPAddr)
				}
				if c.WebAddr != ":9000" {
					t.Errorf("expected Web addr :9000, got %s", c.WebAddr)
				}
				if c.ProfileTTL != 10*time.Minute {
					t.Errorf("expected ProfileTTL 10m, got %v", c.ProfileTTL)
				}
				if c.CleanupInterval != 1*time.Minute {
					t.Errorf("expected CleanupInterval 1m, got %v", c.CleanupInterval)
				}
				if c.MaxBodySize != 64000000 {
					t.Errorf("expected MaxBodySize 64000000, got %d", c.MaxBodySize)
				}
				if c.MaxStorageBytes != 1000000000 {
					t.Errorf("expected MaxStorageBytes 1000000000, got %d", c.MaxStorageBytes)
				}
			},
		},
		{
			name: "virtual frames env var",
			env: map[string]string{
				"VIRTUAL_FRAMES": "service.name, thread.name,",
			},
			validate: func(c *Config) {
				if want := []string{"service.name", "thread.name"}; !slices.Equal(c.VirtualFrames, want) {
					t.Errorf("expected VirtualFrames %v, got %v", want, c.VirtualFrames)
				}
			},
		},
		{
			name: "invalid duration env var uses default",
			env: map[string]string{
				"PROFILE_TTL": "invalid",
			},
			validate: func(c *Config) {
				if c.ProfileTTL != 5*time.Minute {
					t.Errorf("expected ProfileTTL 5m, got %v", c.ProfileTTL)
				}
			},
		},
		{
			name: "invalid size env var uses default",
			env: map[string]string{
				"MAX_BODY_SIZE": "not-a-number",
			},
			validate: func(c *Config) {
				if c.MaxBodySize != 32*1024*1024 {
					t.Errorf("expected MaxBodySize 32MB, got %d", c.MaxBodySize)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				if v, ok := tt.env[key]; ok {
					return v
				}
				return ""
			}

			cfg := loadConfigFromEnv(getenv)
			tt.validate(&cfg)
		})
	}
}

func TestNormalizeBasePath(t *testing.T) {
	valid := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"/", ""},
		{"/firepit", "/firepit"},
		{"/firepit/", "/firepit"},
		{"firepit", "/firepit"},
		{"firepit/", "/firepit"},
		{"/my-app_v1.0~beta", "/my-app_v1.0~beta"},
	}
	for _, tt := range valid {
		got, err := normalizeBasePath(tt.input)
		if err != nil {
			t.Errorf("normalizeBasePath(%q) unexpected error: %v", tt.input, err)
		} else if got != tt.want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}

	invalid := []string{
		"/path'injection",
		`/path\injection`,
		"/path<script>",
		"/path>foo",
		`/path"foo`,
		"/path\nfoo",
		"/path foo",
		"/path;foo",
		"/path%2F",
	}
	for _, input := range invalid {
		_, err := normalizeBasePath(input)
		if err == nil {
			t.Errorf("normalizeBasePath(%q) expected error but got none", input)
		}
	}
}

func TestWebUIServerBasePath(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	tests := []struct {
		name         string
		basePath     string
		requestPath  string
		wantStatus   int
		wantLocation string
		// wantBody must be in the response body, which must not contain the placeholder.
		wantBody string
		gzip     bool
	}{
		{
			name:        "no base path: root returns 200",
			basePath:    "",
			requestPath: "/",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "no base path: api route returns 200",
			basePath:    "",
			requestPath: "/api/profiles",
			wantStatus:  http.StatusOK,
		},
		{
			name:         "base path: root redirects to base",
			basePath:     "/firepit",
			requestPath:  "/",
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "/firepit/",
		},
		{
			name:        "base path: prefixed root serves index with base path",
			basePath:    "/firepit",
			requestPath: "/firepit/",
			wantStatus:  http.StatusOK,
			wantBody:    "'/firepit'",
		},
		{
			name:        "base path: index.html has base path",
			basePath:    "/firepit",
			requestPath: "/firepit/index.html",
			wantStatus:  http.StatusOK,
			wantBody:    "'/firepit'",
		},
		{
			name:        "base path: prefixed api route returns gzip",
			basePath:    "/firepit",
			requestPath: "/firepit/api/profiles",
			wantStatus:  http.StatusOK,
			gzip:        true,
		},
		{
			name:         "base path: unprefixed api route redirects",
			basePath:     "/firepit",
			requestPath:  "/api/profiles",
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "/firepit/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{BasePath: tt.basePath}
			mux := buildWebUIMux(t, st, cfg)

			req := httptest.NewRequest(http.MethodGet, tt.requestPath, nil)
			if tt.gzip {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.wantLocation != "" {
				if loc := w.Header().Get("Location"); loc != tt.wantLocation {
					t.Errorf("Location = %q, want %q", loc, tt.wantLocation)
				}
			}
			if tt.wantBody != "" {
				if body := w.Body.String(); !strings.Contains(body, tt.wantBody) || strings.Contains(body, "FIREPIT_BASE_PATH") {
					t.Errorf("base path not applied to index.html")
				}
			}
			if enc := w.Header().Get("Content-Encoding"); tt.gzip && enc != "gzip" {
				t.Errorf("Content-Encoding = %q, want gzip", enc)
			}
		})
	}
}

func TestAPIMethods(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()
	mux := buildWebUIMux(t, st, Config{})

	tests := []struct {
		method     string
		path       string
		wantStatus int
		// wantField is a field the JSON object in the response must have.
		wantField string
	}{
		{http.MethodGet, "/api/flamegraph", http.StatusOK, ""},
		{http.MethodPost, "/api/flamegraph", http.StatusMethodNotAllowed, ""},
		{http.MethodPut, "/api/flamegraph", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/api/flamescope", http.StatusOK, ""},
		{http.MethodPut, "/api/flamescope", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/api/sandwich", http.StatusOK, ""},
		{http.MethodPost, "/api/sandwich", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/api/sandwich-detail?functionName=main", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/api/profiles", http.StatusOK, "count"},
		{http.MethodDelete, "/api/profiles", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/api/resource-attributes", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/api/attribute-keys", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/api/config", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/api/events", http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			// The event stream does not respond with JSON.
			if strings.HasPrefix(tt.path, "/api/events") {
				return
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var result any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("response is not valid JSON: %v\n%s", err, w.Body.String())
			}
			if tt.wantField != "" {
				if obj, ok := result.(map[string]any); !ok || obj[tt.wantField] == nil {
					t.Errorf("response has no %q field: %s", tt.wantField, w.Body.String())
				}
			}
		})
	}
}

func TestFlamegraphOptions(t *testing.T) {
	defaults := []string{"service.name"}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"no parameter uses default", "", []string{"service.name"}},
		{"empty parameter disables virtual frames", "?virtualFrames=", nil},
		{"parameter overrides default", "?virtualFrames=thread.name,process.pid", []string{"thread.name", "process.pid"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/flamegraph"+tt.query, nil)
			if got := flamegraphOptions(req, defaults).VirtualFrameKeys; !slices.Equal(got, tt.want) {
				t.Errorf("VirtualFrameKeys = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleConfig(t *testing.T) {
	handler := handleConfig(Config{VirtualFrames: []string{"service.name"}})

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var result struct {
		VirtualFrames []string `json:"virtualFrames"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if want := []string{"service.name"}; !slices.Equal(result.VirtualFrames, want) {
		t.Errorf("virtualFrames = %v, want %v", result.VirtualFrames, want)
	}
}

func TestHandleAttributeKeys(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	handler := handleAttributeKeys(st)

	req := httptest.NewRequest(http.MethodGet, "/api/attribute-keys", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var result []string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if result == nil || len(result) != 0 {
		t.Errorf("expected empty JSON array, got %s", w.Body.String())
	}
}

// TestWebUIDoesNotBuildHTMLFromData checks that the web UI assigns only
// constant strings as HTML, as profile data is untrusted.
func TestWebUIDoesNotBuildHTMLFromData(t *testing.T) {
	var sources []byte
	for _, name := range []string{"index.html", "app.js", "lib.js"} {
		content, err := webFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, content...)
	}

	unsafe := []*regexp.Regexp{
		regexp.MustCompile(`\b(innerHTML|outerHTML)\s*\+?=\s*[^;]*(\+|\$\{)`),
		regexp.MustCompile(`\b(innerHTML|outerHTML)\s*\+?=\s*[^'"\s]`),
		regexp.MustCompile(`\.insertAdjacentHTML\(`),
		regexp.MustCompile(`\.html\(\s*[^)\s]`),
		regexp.MustCompile(`document\.write\(`),
	}
	allowed := regexp.MustCompile(`\.html\(null\)`)

	// Split by statement as they may span multiple lines.
	statements := strings.Split(string(sources), ";")
	for _, stmt := range statements {
		stmt = allowed.ReplaceAllString(stmt, "")
		for _, re := range unsafe {
			if m := re.FindString(stmt); m != "" {
				t.Errorf("web UI builds HTML dynamically: %q", strings.Join(strings.Fields(m), " "))
			}
		}
	}
}

// addFilterTestProfiles stores three profiles, each sampling a distinct function.
func addFilterTestProfiles(st *store.Store) {
	dict := &profilespb.ProfilesDictionary{
		StringTable:   []string{"", "samples", "api-1", "api-2", "db-3"},
		FunctionTable: []*profilespb.Function{{}, {NameStrindex: 2}, {NameStrindex: 3}, {NameStrindex: 4}},
		LocationTable: []*profilespb.Location{
			{},
			{Lines: []*profilespb.Line{{FunctionIndex: 1}}},
			{Lines: []*profilespb.Line{{FunctionIndex: 2}}},
			{Lines: []*profilespb.Line{{FunctionIndex: 3}}},
		},
		StackTable: []*profilespb.Stack{{}, {LocationIndices: []int32{1}}, {LocationIndices: []int32{2}}, {LocationIndices: []int32{3}}},
	}
	var rps []*profilespb.ResourceProfiles
	for i, service := range []string{"api", "api", "db"} {
		rps = append(rps, &profilespb.ResourceProfiles{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}},
				{Key: "process.pid", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i + 1)}}},
			}},
			ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{{
				SampleType:   &profilespb.ValueType{TypeStrindex: 1},
				TimeUnixNano: uint64(time.Now().UnixNano()),
				Samples:      []*profilespb.Sample{{StackIndex: int32(i + 1), Values: []int64{1}}},
			}}}},
		})
	}
	st.Add(rps, dict)
}

func TestHandleFlamegraphResourceFilters(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	addFilterTestProfiles(st)
	handler := handleFlamegraph(st, newTestAggregator(t, st), nil)

	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"api-1", "api-2", "db-3"}},
		{"?resourceType=service.name:api", []string{"api-1", "api-2"}},
		{"?resourceType=process.pid:3", []string{"db-3"}},
		{"?resourceType=service.name:api&resourceType=process.pid:2", []string{"api-2"}},
		{"?resourceType=service.name:db&resourceType=process.pid:2", nil},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/api/flamegraph"+tt.query, nil))

		var graphs []struct {
			Root struct {
				Children []struct {
					Name string `json:"name"`
				} `json:"children"`
			} `json:"root"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &graphs); err != nil || len(graphs) != 1 {
			t.Fatalf("%q: unexpected response %s: %v", tt.query, w.Body.String(), err)
		}
		var got []string
		for _, child := range graphs[0].Root.Children {
			got = append(got, child.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q: functions = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestHandleResourceAttributes(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	handler := handleResourceAttributes(st)

	get := func() string {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/api/resource-attributes", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}

	if got := get(); got != "[]" {
		t.Errorf("empty store: body = %s, want []", got)
	}

	addFilterTestProfiles(st)
	want := `[{"key":"process.pid","values":["1","2","3"]},{"key":"service.name","values":["api","db"]}]`
	if got := get(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// buildWebUIMux returns the web UI handler without receiver metrics, released with the test.
func buildWebUIMux(t testing.TB, st *store.Store, cfg Config) *http.ServeMux {
	t.Helper()
	mux, stop := buildWebUI(st, nil, cfg)
	t.Cleanup(stop)
	return mux
}

// newTestAggregator returns an Aggregator for st that is closed with the test.
func newTestAggregator(t testing.TB, st *store.Store) *profiler.Aggregator {
	t.Helper()
	agg := profiler.NewAggregator(st)
	t.Cleanup(agg.Close)
	return agg
}

// Aggregated handlers must match flame graphs built from scratch.
func TestAggregatedHandlersMatchProfiler(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	mux := buildWebUIMux(t, st, Config{})

	get := func(path string, v any) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d: %s", path, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type = %q", path, ct)
		}
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: invalid JSON: %v\n%s", path, err, w.Body.String())
		}
	}
	// normalize sorts children by name.
	var normalize func(n *profiler.FlameNode) *profiler.FlameNode
	normalize = func(n *profiler.FlameNode) *profiler.FlameNode {
		slices.SortFunc(n.Children, func(a, b *profiler.FlameNode) int { return strings.Compare(a.Name, b.Name) })
		for _, child := range n.Children {
			normalize(child)
		}
		return n
	}
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// An empty store has no sample types.
	var empty []any
	get("/api/flamegraph", &empty)
	if len(empty) != 0 {
		t.Errorf("flame graphs of an empty store: %v", empty)
	}

	for round := range 3 {
		addTestProfiles(st, 2, time.Now())

		for _, query := range []string{"", "?virtualFrames=service.name", "?resourceType=service.name:none"} {
			opts := profiler.Options{}
			if strings.Contains(query, "virtualFrames") {
				opts.VirtualFrameKeys = []string{"service.name"}
			}
			filter := ""
			if strings.Contains(query, "resourceType") {
				filter = "service.name:none"
			}
			want := profiler.ToFlamegraph(profiler.FilterByResource(st.ProfileEntries("samples(count)"), []string{filter}), opts)

			var graphs []profiler.NamedFlamegraph
			get("/api/flamegraph"+query, &graphs)
			if len(graphs) != 1 || graphs[0].Type != "samples(count)" {
				t.Fatalf("round %d, %q: unexpected flame graphs %+v", round, query, graphs)
			}
			if got := encode(normalize(graphs[0].Root)); got != encode(normalize(want)) {
				t.Errorf("round %d, %q: flame graph differs from the one built from scratch", round, query)
			}

			var sandwiches []profiler.NamedSandwich
			get("/api/sandwich"+query, &sandwiches)
			wantSandwich := profiler.ToSandwich(want)
			if len(sandwiches) != 1 || sandwiches[0].Type != "samples(count)" {
				t.Fatalf("round %d, %q: unexpected sandwiches %+v", round, query, sandwiches)
			}
			if got, want := encode(sandwiches[0].Data.Functions), encode(wantSandwich.Functions); got != want {
				t.Errorf("round %d, %q: sandwich functions differ:\n got %s\nwant %s", round, query, got, want)
			}

			if len(wantSandwich.Functions) == 0 {
				continue
			}
			fn := wantSandwich.Functions[0].Name
			sep := "?"
			if query != "" {
				sep = "&"
			}
			var detail map[string]*profiler.SandwichGraphs
			get("/api/sandwich-detail"+query+sep+"functionName="+fn, &detail)
			wantDetail := profiler.ExtractSandwichGraphs(want, fn)
			got := detail["samples(count)"]
			if got == nil || got.Callers == nil || got.Callees == nil {
				t.Fatalf("round %d, %q: no sandwich detail for %s: %+v", round, query, fn, detail)
			}
			if encode(normalize(got.Callers)) != encode(normalize(wantDetail.Callers)) ||
				encode(normalize(got.Callees)) != encode(normalize(wantDetail.Callees)) {
				t.Errorf("round %d, %q: sandwich detail of %s differs", round, query, fn)
			}
		}
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sandwich-detail?functionName=missing", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown function: status = %d, want 404", w.Code)
	}
}

func TestWriteNamedJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeNamedJSON(w, "root", []namedJSON{
		{`alloc "space" <bytes>`, json.RawMessage(`{"name":"root","value":1}`)},
		{"cpu", json.RawMessage(`{"name":"root","value":2}`)},
	})
	var got []struct {
		Type string `json:"type"`
		Root struct {
			Value int `json:"value"`
		} `json:"root"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, w.Body.String())
	}
	if len(got) != 2 || got[0].Type != `alloc "space" <bytes>` || got[0].Root.Value != 1 || got[1].Type != "cpu" || got[1].Root.Value != 2 {
		t.Errorf("got %+v", got)
	}

	w = httptest.NewRecorder()
	writeNamedJSON(w, "root", nil)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("no items: %s, want []", got)
	}
}

// The web UI is split into several files. All files it refers to have to be
// embedded and served with a content type browsers accept for them.
func TestWebUIAssets(t *testing.T) {
	st := store.New(5*time.Minute, time.Hour, 0)
	defer st.Close()
	mux := buildWebUIMux(t, st, Config{BasePath: "/firepit"})

	read := func(name string) string {
		t.Helper()
		content, err := webFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatalf("%s is not embedded: %v", name, err)
		}
		return string(content)
	}

	// Files referred to by index.html and, as imports, by the scripts.
	refs := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(read("index.html"), -1)
	imports := regexp.MustCompile(`from '\./([^']+)'`)
	var files []string
	for _, ref := range refs {
		files = append(files, ref[1])
		if strings.HasSuffix(ref[1], ".js") && !strings.Contains(ref[1], "d3") {
			for _, imp := range imports.FindAllStringSubmatch(read(ref[1]), -1) {
				files = append(files, imp[1])
			}
		}
	}
	for _, want := range []string{"style.css", "app.js", "lib.js"} {
		if !slices.Contains(files, want) {
			t.Errorf("%s is not referred to by the web UI; found %v", want, files)
		}
	}

	for _, name := range files {
		if strings.Contains(name, "d3") {
			// Downloaded by "make generate" and not part of the repository.
			continue
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/firepit/"+name, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d", name, w.Code)
			continue
		}
		wantType := "text/css"
		if strings.HasSuffix(name, ".js") {
			// Browsers refuse to run modules served with another type.
			wantType = "text/javascript"
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("GET %s: Content-Type = %q, want %s", name, ct, wantType)
		}
	}

	// The page itself carries neither styles nor scripts, apart from the
	// base path the server fills in.
	index := read("index.html")
	if strings.Contains(index, "<style") || strings.Count(index, "<script>") != 1 {
		t.Error("index.html contains inline styles or scripts")
	}
}
