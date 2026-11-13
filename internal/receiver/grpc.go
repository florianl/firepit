// Package receiver implements gRPC and HTTP handlers for receiving OTel profiles.
package receiver

import (
	"context"
	"sync/atomic"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

type ProfileReceiver struct {
	collectorprofiles.UnimplementedProfilesServiceServer
	store   *store.Store
	metrics *Metrics
}

// New returns a receiver for OTLP/gRPC.
func New(s *store.Store, metrics *Metrics) *ProfileReceiver {
	return &ProfileReceiver{store: s, metrics: metrics}
}

// Metrics counts the export requests handled by the receivers. A nil
// *Metrics counts nothing.
type Metrics struct {
	grpcAccepted        atomic.Uint64
	httpAccepted        atomic.Uint64
	httpRejectedInvalid atomic.Uint64
	httpRejectedLarge   atomic.Uint64
}

// RequestCount is the number of export requests by protocol and result.
type RequestCount struct {
	Protocol string // "grpc" or "http"
	Result   string // "accepted", "invalid" or "too_large"
	Count    uint64
}

// Requests returns the request counts.
func (m *Metrics) Requests() []RequestCount {
	if m == nil {
		return nil
	}
	return []RequestCount{
		{"grpc", "accepted", m.grpcAccepted.Load()},
		{"http", "accepted", m.httpAccepted.Load()},
		{"http", "invalid", m.httpRejectedInvalid.Load()},
		{"http", "too_large", m.httpRejectedLarge.Load()},
	}
}

func (m *Metrics) count(counter func(*Metrics) *atomic.Uint64) {
	if m != nil {
		counter(m).Add(1)
	}
}

func (r *ProfileReceiver) Export(ctx context.Context, req *collectorprofiles.ExportProfilesServiceRequest) (*collectorprofiles.ExportProfilesServiceResponse, error) {
	r.store.Add(req.ResourceProfiles, req.Dictionary)
	r.metrics.count(func(m *Metrics) *atomic.Uint64 { return &m.grpcAccepted })
	return &collectorprofiles.ExportProfilesServiceResponse{}, nil
}
