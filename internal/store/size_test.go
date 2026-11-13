package store

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"testing"

	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// sizeTestRequest returns a request resembling a system-wide profiler export.
func sizeTestRequest() *collectorprofiles.ExportProfilesServiceRequest {
	const (
		numFunctions = 1000
		numStacks    = 800
		numProfiles  = 8
	)
	rng := rand.New(rand.NewPCG(1, 2))
	strVal := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	dict := &profilespb.ProfilesDictionary{
		StringTable:   []string{"", "samples", "count", "profile.frame.type", "thread.name"},
		MappingTable:  []*profilespb.Mapping{{}, {MemoryStart: 0x400000, MemoryLimit: 0x800000, FilenameStrindex: 1}},
		FunctionTable: []*profilespb.Function{{}},
		LocationTable: []*profilespb.Location{{}},
		StackTable:    []*profilespb.Stack{{}},
		LinkTable:     []*profilespb.Link{{}, {TraceId: make([]byte, 16), SpanId: make([]byte, 8)}},
		AttributeTable: []*profilespb.KeyValueAndUnit{
			{},
			{KeyStrindex: 3, Value: strVal("native")},
			{KeyStrindex: 4, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 2}}},
		},
	}
	for f := range numFunctions {
		dict.StringTable = append(dict.StringTable,
			fmt.Sprintf("github.com/example/project/pkg%d.(*Type%d).Method%d", f%40, f%97, f),
			fmt.Sprintf("/src/project/pkg%d/file%d.go", f%40, f))
		n := int32(len(dict.StringTable))
		dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{NameStrindex: n - 2, FilenameStrindex: n - 1})
		dict.LocationTable = append(dict.LocationTable, &profilespb.Location{
			MappingIndex:     1,
			Address:          uint64(0x400000 + 16*f),
			AttributeIndices: []int32{1},
			Lines:            []*profilespb.Line{{FunctionIndex: int32(f + 1), Line: int64(1 + f%500)}},
		})
	}
	for range numStacks {
		stack := &profilespb.Stack{}
		for range 10 + rng.IntN(50) {
			stack.LocationIndices = append(stack.LocationIndices, int32(1+rng.IntN(numFunctions)))
		}
		dict.StackTable = append(dict.StackTable, stack)
	}

	req := &collectorprofiles.ExportProfilesServiceRequest{Dictionary: dict}
	for p := range numProfiles {
		profile := &profilespb.Profile{
			SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
			PeriodType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
			Period:       50_000_000,
			TimeUnixNano: 1_700_000_000_000_000_000,
			ProfileId:    make([]byte, 16),
		}
		for range 250 {
			sample := &profilespb.Sample{
				StackIndex:       int32(1 + rng.IntN(numStacks)),
				AttributeIndices: []int32{2},
			}
			if rng.IntN(2) == 0 {
				sample.Values = []int64{int64(1 + rng.IntN(20))}
			} else {
				for range 1 + rng.IntN(4) {
					sample.TimestampsUnixNano = append(sample.TimestampsUnixNano, 1_700_000_000_000_000_000+rng.Uint64N(5_000_000_000))
				}
			}
			profile.Samples = append(profile.Samples, sample)
		}
		req.ResourceProfiles = append(req.ResourceProfiles, &profilespb.ResourceProfiles{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: strVal(fmt.Sprintf("service-%d", p))},
				{Key: "host.name", Value: strVal("host.example.com")},
				{Key: "process.pid", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(1000 + p)}}},
			}},
			ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{profile}}},
		})
	}
	return req
}

// estimatedSize returns the size the store accounts for req.
func estimatedSize(req *collectorprofiles.ExportProfilesServiceRequest) int64 {
	s := New(0, 1<<40, 0)
	defer s.Close()
	s.Add(req.ResourceProfiles, req.Dictionary)
	return s.totalBytes
}

// heapInUse returns the bytes of live heap objects.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestSizeMatchesHeapUsage checks the estimate against the measured heap usage.
func TestSizeMatchesHeapUsage(t *testing.T) {
	original := sizeTestRequest()
	wire, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	jsonWire, err := protojson.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	decoders := map[string]func() *collectorprofiles.ExportProfilesServiceRequest{
		// gRPC receiver
		"protobuf": func() *collectorprofiles.ExportProfilesServiceRequest {
			req := &collectorprofiles.ExportProfilesServiceRequest{}
			if err := proto.Unmarshal(wire, req); err != nil {
				t.Fatal(err)
			}
			return req
		},
		// HTTP receiver
		"json": func() *collectorprofiles.ExportProfilesServiceRequest {
			req := &collectorprofiles.ExportProfilesServiceRequest{}
			if err := protojson.Unmarshal(jsonWire, req); err != nil {
				t.Fatal(err)
			}
			return req
		},
	}

	for name, decode := range decoders {
		t.Run(name, func(t *testing.T) {
			const copies = 20
			requests := make([]*collectorprofiles.ExportProfilesServiceRequest, copies)

			before := heapInUse()
			for i := range requests {
				requests[i] = decode()
			}
			actual := float64(heapInUse()-before) / copies

			estimated := float64(estimatedSize(requests[0]))
			runtime.KeepAlive(requests)

			t.Logf("wire size %d, heap %.0f, estimated %.0f (%.2fx of heap, wire is %.2fx of heap)",
				len(wire), actual, estimated, estimated/actual, float64(len(wire))/actual)
			if ratio := estimated / actual; ratio < 0.85 || ratio > 1.15 {
				t.Errorf("estimated size is %.2fx of the heap usage, want within 15%%", ratio)
			}
		})
	}
}

// TestSizeAccountsSharedDataOnce checks that the dictionary is accounted once
// regardless of how profiles are grouped.
func TestSizeAccountsSharedDataOnce(t *testing.T) {
	req := sizeTestRequest()
	separate := estimatedSize(req)

	// Move all profiles into the first ScopeProfiles.
	grouped := proto.Clone(req).(*collectorprofiles.ExportProfilesServiceRequest)
	first := grouped.ResourceProfiles[0]
	for _, rp := range grouped.ResourceProfiles[1:] {
		first.ScopeProfiles[0].Profiles = append(first.ScopeProfiles[0].Profiles, rp.ScopeProfiles[0].Profiles...)
	}
	grouped.ResourceProfiles = grouped.ResourceProfiles[:1]
	together := estimatedSize(grouped)

	dict := dictionarySize(req.Dictionary)
	if diff := separate - together; diff < 0 || diff > dict/10 {
		t.Errorf("size of separately grouped profiles = %d, of profiles in one group = %d; "+
			"want them to differ by the resources only (dictionary: %d)", separate, together, dict)
	}
	if separate < dict || separate > 3*dict {
		t.Errorf("size = %d, want the dictionary of %d bytes to be accounted exactly once", separate, dict)
	}
}

func TestAllocSize(t *testing.T) {
	tests := map[uintptr]int64{0: 0, 1: 8, 8: 8, 9: 16, 33: 48, 1025: 1152, 32768: 32768, 32769: 40960, 100000: 106496}
	for n, want := range tests {
		if got := allocSize(n); got != want {
			t.Errorf("allocSize(%d) = %d, want %d", n, got, want)
		}
	}
}
