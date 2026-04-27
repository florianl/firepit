package receiver

import (
	"encoding/json"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

func NewHTTPHandler(s *store.Store, maxBodySize int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		req := &collectorprofiles.ExportProfilesServiceRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			http.Error(w, "Failed to parse request: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.Add(req.ResourceProfiles, req.Dictionary)

		resp := &collectorprofiles.ExportProfilesServiceResponse{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}
