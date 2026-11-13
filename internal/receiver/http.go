package receiver

import (
	"compress/gzip"
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
	gzipReaderPool = sync.Pool{
		New: func() any {
			return new(gzip.Reader)
		},
	}
)

// errBodyTooLarge is returned if the decompressed body exceeds the size limit.
var errBodyTooLarge = errors.New("request body too large")

// readBody reads the request body and decompresses it if gzip encoded.
// The decompressed size is limited to maxBodySize as well.
func readBody(r *http.Request, maxBodySize int64) ([]byte, error) {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return io.ReadAll(r.Body)
	}

	zr := gzipReaderPool.Get().(*gzip.Reader)
	defer gzipReaderPool.Put(zr)
	if err := zr.Reset(r.Body); err != nil {
		return nil, err
	}
	defer zr.Close()

	body, err := io.ReadAll(io.LimitReader(zr, maxBodySize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBodySize {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func NewHTTPHandler(s *store.Store, maxBodySize int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		defer r.Body.Close()

		if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" && enc != "gzip" {
			w.Header().Set("Accept-Encoding", "gzip")
			http.Error(w, "Unsupported content encoding", http.StatusUnsupportedMediaType)
			return
		}

		body, err := readBody(r, maxBodySize)
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok || errors.Is(err, errBodyTooLarge) {
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
