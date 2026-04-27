// Package receiver implements gRPC and HTTP handlers for receiving OTel profiles.
package receiver

import (
	"context"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
)

type ProfileReceiver struct {
	collectorprofiles.UnimplementedProfilesServiceServer
	store *store.Store
}

func New(s *store.Store) *ProfileReceiver {
	return &ProfileReceiver{store: s}
}

func (r *ProfileReceiver) Export(ctx context.Context, req *collectorprofiles.ExportProfilesServiceRequest) (*collectorprofiles.ExportProfilesServiceResponse, error) {
	r.store.Add(req.ResourceProfiles, req.Dictionary)
	return &collectorprofiles.ExportProfilesServiceResponse{}, nil
}
