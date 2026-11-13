package receiver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/florianl/firepit/internal/store"
)

func TestNewReceiver(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	r := New(st)

	if r == nil {
		t.Fatal("Receiver should not be nil")
	}
}

func TestNewHTTPHandler(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	if handler == nil {
		t.Fatal("HTTP handler should not be nil")
	}
}

func TestHTTPHandlerInvalidMethod(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	req := httptest.NewRequest("GET", "/v1/profiles", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("Expected status 405, got %d", w.Code)
	}
}

func TestHTTPHandlerEmptyBody(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	req := httptest.NewRequest("POST", "/v1/profiles", bytes.NewReader([]byte("")))
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected status 400, got %d", w.Code)
	}
}

func TestHTTPHandlerValidRequest(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
		},
		Samples: []*profilespb.Sample{},
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	body := &collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{rp},
		Dictionary:       dict,
	}

	data, err := protojson.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/profiles", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	var resp collectorprofiles.ExportProfilesServiceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
}

func TestHTTPHandlerMalformedJSON(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	req := httptest.NewRequest("POST", "/v1/profiles", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected status 400 for malformed JSON, got %d", w.Code)
	}
}

func TestHTTPHandlerWithResourceAttributes(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "service.name", "my-service"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
		},
		Samples: []*profilespb.Sample{},
	}

	rp := &profilespb.ResourceProfiles{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{
					Key: "service.name",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "my-service"},
					},
				},
			},
		},
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	body := &collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{rp},
		Dictionary:       dict,
	}

	data, err := protojson.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/profiles", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}
}

func TestGRPCReceiverProfile(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	receiver := New(st)

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
		},
		Samples: []*profilespb.Sample{},
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	req := &collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{rp},
		Dictionary:       dict,
	}

	resp, err := receiver.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	if resp == nil {
		t.Fatal("Response should not be nil")
	}
}

func TestHTTPHandlerOptions(t *testing.T) {
	st := store.New(10*time.Minute, 5*time.Second, 0)
	defer st.Close()
	handler := NewHTTPHandler(st, 32*1024*1024)

	req := httptest.NewRequest("OPTIONS", "/v1/profiles", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("Expected status 405, got %d", w.Code)
	}
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHTTPHandlerContentEncoding(t *testing.T) {
	body, err := protojson.Marshal(&collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{{
			ScopeProfiles: []*profilespb.ScopeProfiles{{
				Profiles: []*profilespb.Profile{{TimeUnixNano: uint64(time.Now().UnixNano())}},
			}},
		}},
		Dictionary: &profilespb.ProfilesDictionary{StringTable: []string{"", strings.Repeat("x", 4096)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	zipped := gzipBytes(t, body)
	if len(zipped) >= len(body)/2 {
		t.Fatalf("test body does not compress: %d -> %d bytes", len(body), len(zipped))
	}

	tests := []struct {
		name        string
		encoding    string
		body        []byte
		maxBodySize int64
		wantStatus  int
		wantStored  int
	}{
		{"gzip", "gzip", zipped, 1 << 20, http.StatusOK, 1},
		{"identity", "identity", body, 1 << 20, http.StatusOK, 1},
		{"gzip header with plain body", "gzip", body, 1 << 20, http.StatusBadRequest, 0},
		{"truncated gzip", "gzip", zipped[:len(zipped)/2], 1 << 20, http.StatusBadRequest, 0},
		{"unsupported encoding", "br", body, 1 << 20, http.StatusUnsupportedMediaType, 0},
		// Compressed body is below the limit, decompressed is not
		{"decompressed body too large", "gzip", zipped, int64(len(body) - 1), http.StatusRequestEntityTooLarge, 0},
		{"decompressed body at limit", "gzip", zipped, int64(len(body)), http.StatusOK, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := store.New(10*time.Minute, time.Hour, 0)
			defer st.Close()
			handler := NewHTTPHandler(st, tt.maxBodySize)

			req := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", tt.encoding)
			w := httptest.NewRecorder()
			handler(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if count, _, _, _ := st.Stats(); count != tt.wantStored {
				t.Errorf("%d profiles stored, want %d", count, tt.wantStored)
			}
		})
	}
}

// BenchmarkReadBody measures reading a 4 MiB request body.
func BenchmarkReadBody(b *testing.B) {
	payload := make([]byte, 4<<20)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(payload))
		body, err := readBody(req, 32<<20)
		if err != nil || len(body) != len(payload) {
			b.Fatalf("read %d bytes: %v", len(body), err)
		}
	}
}

func TestReadBody(t *testing.T) {
	payload := []byte(`{"resourceProfiles":[]}`)
	for _, contentLength := range []int64{int64(len(payload)), -1, 5, 1 << 40} {
		req := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(payload))
		// Content-Length is only a hint.
		req.ContentLength = contentLength
		body, err := readBody(req, 1<<20)
		if err != nil || !bytes.Equal(body, payload) {
			t.Errorf("Content-Length %d: body = %q, err = %v", contentLength, body, err)
		}
	}
}
