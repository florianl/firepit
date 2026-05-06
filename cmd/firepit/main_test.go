package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
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

func TestLoadConfigBasePath(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantPath string
	}{
		{"empty", map[string]string{}, ""},
		{"set with leading slash", map[string]string{"BASE_PATH": "/firepit"}, "/firepit"},
		{"set without leading slash", map[string]string{"BASE_PATH": "firepit"}, "/firepit"},
		{"set with trailing slash", map[string]string{"BASE_PATH": "/firepit/"}, "/firepit"},
		{"root slash only", map[string]string{"BASE_PATH": "/"}, ""},
		{"invalid chars fallback to empty", map[string]string{"BASE_PATH": "/fire'pit"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadConfigFromEnv(func(key string) string { return tt.env[key] })
			if cfg.BasePath != tt.wantPath {
				t.Errorf("BasePath = %q, want %q", cfg.BasePath, tt.wantPath)
			}
		})
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
			name:        "base path: prefixed root returns 200",
			basePath:    "/firepit",
			requestPath: "/firepit/",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "base path: prefixed api route returns 200",
			basePath:    "/firepit",
			requestPath: "/firepit/api/profiles",
			wantStatus:  http.StatusOK,
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
			mux := buildWebUIMux(st, cfg)

			req := httptest.NewRequest(http.MethodGet, tt.requestPath, nil)
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
		})
	}
}

func TestHandleFlamegraph(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	handler := handleFlamegraph(st)

	tests := []struct {
		name       string
		method     string
		query      string
		statusCode int
	}{
		{
			name:       "GET request",
			method:     http.MethodGet,
			query:      "",
			statusCode: http.StatusOK,
		},
		{
			name:       "GET with resourceType filter",
			method:     http.MethodGet,
			query:      "?resourceType=myservice",
			statusCode: http.StatusOK,
		},
		{
			name:       "POST not allowed",
			method:     http.MethodPost,
			statusCode: http.StatusMethodNotAllowed,
		},
		{
			name:       "PUT not allowed",
			method:     http.MethodPut,
			statusCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/flamegraph"+tt.query, nil)
			w := httptest.NewRecorder()

			handler(w, req)

			if w.Code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, w.Code)
			}

			if tt.statusCode == http.StatusOK {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %s", ct)
				}

				var result []interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Errorf("response is not valid JSON: %v", err)
				}
			}
		})
	}
}

func TestHandleProfiles(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	handler := handleProfiles(st)

	tests := []struct {
		name       string
		method     string
		statusCode int
	}{
		{
			name:       "GET request",
			method:     http.MethodGet,
			statusCode: http.StatusOK,
		},
		{
			name:       "POST not allowed",
			method:     http.MethodPost,
			statusCode: http.StatusMethodNotAllowed,
		},
		{
			name:       "DELETE not allowed",
			method:     http.MethodDelete,
			statusCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/profiles", nil)
			w := httptest.NewRecorder()

			handler(w, req)

			if w.Code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, w.Code)
			}

			if tt.statusCode == http.StatusOK {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %s", ct)
				}

				var result map[string]interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Errorf("response is not valid JSON: %v", err)
				}

				if _, ok := result["count"]; !ok {
					t.Error("response missing 'count' field")
				}
			}
		})
	}
}

func TestHandleResourceTypes(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	handler := handleResourceTypes(st)

	tests := []struct {
		name       string
		method     string
		statusCode int
	}{
		{
			name:       "GET request",
			method:     http.MethodGet,
			statusCode: http.StatusOK,
		},
		{
			name:       "POST not allowed",
			method:     http.MethodPost,
			statusCode: http.StatusMethodNotAllowed,
		},
		{
			name:       "PATCH not allowed",
			method:     http.MethodPatch,
			statusCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/resource-types", nil)
			w := httptest.NewRecorder()

			handler(w, req)

			if w.Code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, w.Code)
			}

			if tt.statusCode == http.StatusOK {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %s", ct)
				}

				var result map[string]interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Errorf("response is not valid JSON: %v", err)
				}
			}
		})
	}
}

func TestHandleHeatMap(t *testing.T) {
	st := store.New(5*time.Minute, 30*time.Second, 500*1024*1024)
	defer st.Close()

	handler := handleFlamescope(st)

	tests := []struct {
		name       string
		method     string
		query      string
		statusCode int
	}{
		{
			name:       "GET request",
			method:     http.MethodGet,
			query:      "",
			statusCode: http.StatusOK,
		},
		{
			name:       "GET with resourceType filter",
			method:     http.MethodGet,
			query:      "?resourceType=myservice",
			statusCode: http.StatusOK,
		},
		{
			name:       "POST not allowed",
			method:     http.MethodPost,
			statusCode: http.StatusMethodNotAllowed,
		},
		{
			name:       "PUT not allowed",
			method:     http.MethodPut,
			statusCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/heatmap"+tt.query, nil)
			w := httptest.NewRecorder()

			handler(w, req)

			if w.Code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, w.Code)
			}

			if tt.statusCode == http.StatusOK {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected Content-Type application/json, got %s", ct)
				}

				var result []interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Errorf("response is not valid JSON: %v", err)
				}
			}
		})
	}
}
