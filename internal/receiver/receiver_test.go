package receiver

import (
	"bytes"
	"compress/gzip"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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

// exportRequest returns a request with one profile and the given resource attributes.
func exportRequest(attrs ...*commonpb.KeyValue) *collectorprofiles.ExportProfilesServiceRequest {
	return &collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{{
			Resource: &resourcepb.Resource{Attributes: attrs},
			ScopeProfiles: []*profilespb.ScopeProfiles{{
				Profiles: []*profilespb.Profile{{SampleType: &profilespb.ValueType{TypeStrindex: 1}}},
			}},
		}},
		Dictionary: &profilespb.ProfilesDictionary{StringTable: []string{"", "cpu"}},
	}
}

func TestHTTPHandler(t *testing.T) {
	marshal := func(req *collectorprofiles.ExportProfilesServiceRequest) string {
		data, err := protojson.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	serviceName := &commonpb.KeyValue{
		Key:   "service.name",
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "my-service"}},
	}

	tests := []struct {
		name          string
		method        string
		body          string
		wantStatus    int
		wantStored    int
		wantResources []string
	}{
		{"GET", http.MethodGet, "", http.StatusMethodNotAllowed, 0, nil},
		{"OPTIONS", http.MethodOptions, "", http.StatusMethodNotAllowed, 0, nil},
		{"empty body", http.MethodPost, "", http.StatusBadRequest, 0, nil},
		{"malformed JSON", http.MethodPost, "not json", http.StatusBadRequest, 0, nil},
		{"valid", http.MethodPost, marshal(exportRequest()), http.StatusOK, 1, nil},
		{"resource attributes", http.MethodPost, marshal(exportRequest(serviceName)), http.StatusOK, 1, []string{"service.name:my-service"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := store.New(10*time.Minute, time.Hour, 0)
			defer st.Close()

			req := httptest.NewRequest(tt.method, "/v1/profiles", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			NewHTTPHandler(st, 32<<20, nil)(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if w.Code == http.StatusOK {
				var resp collectorprofiles.ExportProfilesServiceResponse
				if err := protojson.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Errorf("invalid response %q: %v", w.Body.String(), err)
				}
			}
			if count, _, _, _ := st.Stats(); count != tt.wantStored {
				t.Errorf("%d profiles stored, want %d", count, tt.wantStored)
			}
			if got := st.ResourceTypes(); !slices.Equal(got, tt.wantResources) {
				t.Errorf("resource types = %v, want %v", got, tt.wantResources)
			}
		})
	}
}

func TestGRPCReceiverProfile(t *testing.T) {
	st := store.New(10*time.Minute, time.Hour, 0)
	defer st.Close()

	if _, err := New(st, nil).Export(context.Background(), exportRequest()); err != nil {
		t.Fatalf("Export failed: %v", err)
	}
	if count, _, _, _ := st.Stats(); count != 1 {
		t.Errorf("%d profiles stored, want 1", count)
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
			handler := NewHTTPHandler(st, tt.maxBodySize, nil)

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

func TestMetricsCountRequests(t *testing.T) {
	st := store.New(10*time.Minute, time.Hour, 0)
	defer st.Close()
	metrics := &Metrics{}

	handler := NewHTTPHandler(st, 64, metrics)
	for _, body := range []string{`{}`, `{}`, `{`, `{"resourceProfiles":[` + strings.Repeat(" ", 100) + `]}`} {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/profiles", strings.NewReader(body)))
	}
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/profiles", nil))

	if _, err := New(st, metrics).Export(context.Background(), &collectorprofiles.ExportProfilesServiceRequest{}); err != nil {
		t.Fatal(err)
	}

	want := map[string]uint64{"grpc accepted": 1, "http accepted": 2, "http invalid": 2, "http too_large": 1}
	got := map[string]uint64{}
	for _, c := range metrics.Requests() {
		got[c.Protocol+" "+c.Result] = c.Count
	}
	if !maps.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}

	// Receivers work without metrics.
	var none *Metrics
	if none.Requests() != nil {
		t.Error("nil Metrics reports requests")
	}
	NewHTTPHandler(st, 64, nil)(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/profiles", strings.NewReader(`{}`)))
}
