package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
