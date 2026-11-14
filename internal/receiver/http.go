package receiver

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

var gzipReaderPool = sync.Pool{
	New: func() any {
		return new(gzip.Reader)
	},
}

// errBodyTooLarge is returned if the decompressed body exceeds the size limit.
var errBodyTooLarge = errors.New("request body too large")

// readBody reads the request body and decompresses it if gzip encoded.
// The decompressed size is limited to maxBodySize as well.
func readBody(r *http.Request, maxBodySize int64) ([]byte, error) {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return readAll(r.Body, r.ContentLength, maxBodySize)
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

// readAll reads rd to EOF and preallocates the buffer if length is known.
func readAll(rd io.Reader, length, maxBodySize int64) ([]byte, error) {
	if length <= 0 || length > maxBodySize {
		return io.ReadAll(rd)
	}
	var buf bytes.Buffer
	// MinRead extra avoids a regrow when ReadFrom hits EOF.
	buf.Grow(int(length) + bytes.MinRead)
	_, err := buf.ReadFrom(rd)
	return buf.Bytes(), err
}

// NewHTTPHandler returns a handler for OTLP/HTTP JSON requests.
func NewHTTPHandler(s *store.Store, maxBodySize int64, metrics *Metrics) http.HandlerFunc {
	invalid := func(m *Metrics) *atomic.Uint64 { return &m.httpRejectedInvalid }
	tooLarge := func(m *Metrics) *atomic.Uint64 { return &m.httpRejectedLarge }
	accepted := func(m *Metrics) *atomic.Uint64 { return &m.httpAccepted }

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			metrics.count(invalid)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		defer r.Body.Close()

		if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" && enc != "gzip" {
			metrics.count(invalid)
			w.Header().Set("Accept-Encoding", "gzip")
			http.Error(w, "Unsupported content encoding", http.StatusUnsupportedMediaType)
			return
		}

		body, err := readBody(r, maxBodySize)
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok || errors.Is(err, errBodyTooLarge) {
				metrics.count(tooLarge)
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			metrics.count(invalid)
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		req := &collectorprofiles.ExportProfilesServiceRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			metrics.count(invalid)
			http.Error(w, "Failed to parse request: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.Add(req.ResourceProfiles, req.Dictionary)
		metrics.count(accepted)

		// An empty ExportProfilesServiceResponse reports full success.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}\n")
	}
}
