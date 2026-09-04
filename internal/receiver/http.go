package receiver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

var (
	requestPool = sync.Pool{
		New: func() any {
			return &collectorprofiles.ExportProfilesServiceRequest{}
		},
	}
	responsePool = sync.Pool{
		New: func() any {
			return &collectorprofiles.ExportProfilesServiceResponse{}
		},
	}
)

func NewHTTPHandler(s *store.Store, maxBodySize int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		defer r.Body.Close()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		req := requestPool.Get().(*collectorprofiles.ExportProfilesServiceRequest)
		defer func() {
			req.Reset()
			requestPool.Put(req)
		}()
		if err := protojson.Unmarshal(body, req); err != nil {
			http.Error(w, "Failed to parse request: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.Add(req.ResourceProfiles, req.Dictionary)

		resp := responsePool.Get().(*collectorprofiles.ExportProfilesServiceResponse)
		defer func() {
			resp.Reset()
			responsePool.Put(resp)
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}
