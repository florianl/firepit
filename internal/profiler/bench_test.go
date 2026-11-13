package profiler

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/florianl/firepit/internal/store"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

// benchEntries returns profiles where each request shares one dictionary
// among its profiles, like a system-wide profiler does.
func benchEntries(requests, profilesPerRequest, samplesPerProfile int) []store.ProfileEntry {
	const (
		numFunctions = 2000
		numLocations = 3000
		numStacks    = 1500
	)
	sampleRng := rand.New(rand.NewPCG(1, 2))
	var entries []store.ProfileEntry

	for range requests {
		// Same seed: every request has its own copy of the same stacks.
		rng := rand.New(rand.NewPCG(3, 4))
		dict := &profilespb.ProfilesDictionary{
			StringTable: []string{"", "profile.frame.type", "native", "thread.name"},
			AttributeTable: []*profilespb.KeyValueAndUnit{
				{},
				{KeyStrindex: 1, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "native"}}},
			},
		}
		for t := range 8 {
			dict.StringTable = append(dict.StringTable, fmt.Sprintf("worker-%d", t))
			dict.AttributeTable = append(dict.AttributeTable, &profilespb.KeyValueAndUnit{
				KeyStrindex: 3,
				Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: int32(len(dict.StringTable) - 1)}},
			})
		}
		dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{})
		for f := range numFunctions {
			dict.StringTable = append(dict.StringTable,
				fmt.Sprintf("github.com/example/project/pkg%d.(*Type%d).Method%d", f%40, f%97, f),
				fmt.Sprintf("/src/project/pkg%d/file%d.go", f%40, f%211))
			n := int32(len(dict.StringTable))
			dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{NameStrindex: n - 2, FilenameStrindex: n - 1})
		}
		dict.LocationTable = append(dict.LocationTable, &profilespb.Location{})
		for l := range numLocations {
			dict.LocationTable = append(dict.LocationTable, &profilespb.Location{
				Address:          uint64(0x400000 + l*16),
				AttributeIndices: []int32{1},
				Lines:            []*profilespb.Line{{FunctionIndex: int32(1 + rng.IntN(numFunctions)), Line: int64(1 + rng.IntN(900))}},
			})
		}
		dict.StackTable = append(dict.StackTable, &profilespb.Stack{})
		// Stacks share common root prefixes
		bases := make([][]int32, 40)
		for i := range bases {
			bases[i] = make([]int32, 10+rng.IntN(15))
			for j := range bases[i] {
				bases[i][j] = int32(1 + rng.IntN(numLocations))
			}
		}
		for range numStacks {
			base := bases[rng.IntN(len(bases))]
			leaf := make([]int32, 5+rng.IntN(25))
			for j := range leaf {
				leaf[j] = int32(1 + rng.IntN(numLocations))
			}
			// location_indices are leaf-first
			dict.StackTable = append(dict.StackTable, &profilespb.Stack{LocationIndices: append(leaf, base...)})
		}

		for p := range profilesPerRequest {
			profile := &profilespb.Profile{TimeUnixNano: 1}
			for range samplesPerProfile {
				profile.Samples = append(profile.Samples, &profilespb.Sample{
					StackIndex:       int32(1 + sampleRng.IntN(numStacks)),
					AttributeIndices: []int32{int32(2 + sampleRng.IntN(8))},
					Values:           []int64{int64(1 + sampleRng.IntN(5))},
				})
			}
			entries = append(entries, store.ProfileEntry{
				Profile:    profile,
				Dictionary: dict,
				ResourceAttributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("svc-%d", p)}}},
				},
			})
		}
	}
	return entries
}

func BenchmarkToFlamegraph(b *testing.B) {
	entries := benchEntries(60, 10, 300)
	b.ReportAllocs()
	for b.Loop() {
		ToFlamegraph(entries, Options{})
	}
}

func BenchmarkToFlamegraphVirtualFrames(b *testing.B) {
	entries := benchEntries(60, 10, 300)
	opts := Options{VirtualFrameKeys: []string{"service.name", "thread.name"}}
	b.ReportAllocs()
	for b.Loop() {
		ToFlamegraph(entries, opts)
	}
}

func BenchmarkFlamegraphEncode(b *testing.B) {
	root := ToFlamegraph(benchEntries(60, 10, 300), Options{})
	graphs := []NamedFlamegraph{{Type: "samples(count)", Root: root}}
	b.ReportAllocs()
	for b.Loop() {
		if err := json.NewEncoder(io.Discard).Encode(graphs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToSandwich(b *testing.B) {
	root := ToFlamegraph(benchEntries(60, 10, 300), Options{})
	b.ReportAllocs()
	for b.Loop() {
		ToSandwich(root)
	}
}
