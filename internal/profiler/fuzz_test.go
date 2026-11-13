package profiler

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// fuzzSeedRequests returns the seed corpus for the fuzz targets.
func fuzzSeedRequests() []*collectorprofiles.ExportProfilesServiceRequest {
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	dict := func() *profilespb.ProfilesDictionary {
		return &profilespb.ProfilesDictionary{
			StringTable: []string{"", "samples", "count", "main", "main.go", "work", "profile.frame.type", "thread.name", "worker", "inlined"},
			FunctionTable: []*profilespb.Function{
				{},
				{NameStrindex: 3, FilenameStrindex: 4},
				{NameStrindex: 5, FilenameStrindex: 4},
				{NameStrindex: 9},
			},
			MappingTable: []*profilespb.Mapping{{}, {MemoryStart: 1, MemoryLimit: 2, FilenameStrindex: 4}},
			LocationTable: []*profilespb.Location{
				{},
				{MappingIndex: 1, Lines: []*profilespb.Line{{FunctionIndex: 1, Line: 10}}, AttributeIndices: []int32{1}},
				{Lines: []*profilespb.Line{{FunctionIndex: 3, Line: 7}, {FunctionIndex: 2, Line: 20}}},
				{Address: 0xdeadbeef},
			},
			LinkTable: []*profilespb.Link{{}, {TraceId: make([]byte, 16), SpanId: make([]byte, 8)}},
			AttributeTable: []*profilespb.KeyValueAndUnit{
				{},
				{KeyStrindex: 6, Value: str("go")},
				{KeyStrindex: 7, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 8}}},
				{KeyStrindex: 7, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
			},
			StackTable: []*profilespb.Stack{
				{},
				{LocationIndices: []int32{2, 1}},
				{LocationIndices: []int32{3, 2, 1}},
			},
		}
	}
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "service.name", Value: str("api")},
		{KeyStrindex: 7, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 8}}},
		{Key: "process.pid", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
	}}
	request := func(profiles ...*profilespb.Profile) *collectorprofiles.ExportProfilesServiceRequest {
		return &collectorprofiles.ExportProfilesServiceRequest{
			ResourceProfiles: []*profilespb.ResourceProfiles{{
				Resource:      resource,
				ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: profiles}},
			}},
			Dictionary: dict(),
		}
	}
	const now = 1_700_000_000_000_000_000

	return []*collectorprofiles.ExportProfilesServiceRequest{
		// Aggregated values
		request(&profilespb.Profile{
			SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
			TimeUnixNano: now,
			Samples: []*profilespb.Sample{
				{StackIndex: 1, Values: []int64{3}, AttributeIndices: []int32{2}},
				{StackIndex: 2, Values: []int64{5}, AttributeIndices: []int32{3}, LinkIndex: 1},
			},
		}),
		// Timestamped samples, and a second profile of another type
		request(
			&profilespb.Profile{
				SampleType:   &profilespb.ValueType{TypeStrindex: 1},
				TimeUnixNano: now,
				Samples: []*profilespb.Sample{
					{StackIndex: 1, TimestampsUnixNano: []uint64{now, now + 15_000_000, now + 2_000_000_000}},
					{StackIndex: 2, Values: []int64{1, 2}, TimestampsUnixNano: []uint64{now + 1, now + 2}},
				},
			},
			&profilespb.Profile{TimeUnixNano: now + 1, Samples: []*profilespb.Sample{{StackIndex: 2, Values: []int64{-4}}}},
		),
		// References that do not resolve
		request(&profilespb.Profile{
			SampleType: &profilespb.ValueType{TypeStrindex: 99, UnitStrindex: -1},
			Samples: []*profilespb.Sample{
				{StackIndex: 99, Values: []int64{1}},
				{StackIndex: -1, Values: []int64{1}, AttributeIndices: []int32{-5, 99}},
				{StackIndex: 0, Values: []int64{0}},
				{StackIndex: 1},
			},
		}),
		// No dictionary and no profiles
		{ResourceProfiles: []*profilespb.ResourceProfiles{{Resource: resource}}},
		{Dictionary: dict()},
		{},
	}
}

// exerciseRequest stores req and computes all web UI views from it.
func exerciseRequest(t *testing.T, req *collectorprofiles.ExportProfilesServiceRequest, virtualFrames, filter, function string) {
	t.Helper()

	st := store.New(time.Hour, time.Hour, 0)
	defer st.Close()
	st.Add(req.ResourceProfiles, req.Dictionary)

	st.Stats()
	st.AttributeKeys()
	filters := append(st.ResourceTypes(), filter, "")
	opts := Options{VirtualFrameKeys: ParseKeyList(virtualFrames)}

	encode := func(v any) {
		if err := json.NewEncoder(io.Discard).Encode(v); err != nil {
			t.Fatalf("result cannot be encoded: %v", err)
		}
	}

	for _, sampleType := range st.SampleTypes() {
		all := st.ProfileEntries(sampleType)
		for _, f := range filters {
			entries := FilterByResourceType(all, f)

			root := ToFlamegraph(entries, opts)
			encode(NamedFlamegraph{Type: sampleType, Root: root})

			checkFlameNode(t, root)

			sandwich := ToSandwich(root)
			encode(NamedSandwich{Type: sampleType, Data: sandwich})
			for _, fn := range sandwich.Functions {
				encode(ExtractSandwichGraphs(root, fn.Name))
			}
			encode(ExtractSandwichGraphs(root, function))

			encode(NamedFlamescope{Type: sampleType, Data: ToHeatMap(entries)})
		}
	}
}

// checkFlameNode verifies that sibling names are unique.
func checkFlameNode(t *testing.T, node *FlameNode) {
	t.Helper()
	seen := make(map[string]bool, len(node.Children))
	for _, child := range node.Children {
		if seen[child.Name] {
			t.Fatalf("node %q has two children named %q", node.Name, child.Name)
		}
		seen[child.Name] = true
		checkFlameNode(t, child)
	}
}

// FuzzProtobufRequest fuzzes protobuf encoded export requests.
func FuzzProtobufRequest(f *testing.F) {
	for _, req := range fuzzSeedRequests() {
		wire, err := proto.Marshal(req)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(wire, "thread.name,service.name", "service.name:api", "main")
	}

	f.Fuzz(func(t *testing.T, wire []byte, virtualFrames, filter, function string) {
		req := &collectorprofiles.ExportProfilesServiceRequest{}
		if err := proto.Unmarshal(wire, req); err != nil {
			return
		}
		exerciseRequest(t, req, virtualFrames, filter, function)
	})
}

// FuzzJSONRequest fuzzes JSON encoded export requests.
func FuzzJSONRequest(f *testing.F) {
	for _, req := range fuzzSeedRequests() {
		body, err := protojson.Marshal(req)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body, "thread.name,service.name", "service.name:api", "main")
	}

	f.Fuzz(func(t *testing.T, body []byte, virtualFrames, filter, function string) {
		req := &collectorprofiles.ExportProfilesServiceRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			return
		}
		exerciseRequest(t, req, virtualFrames, filter, function)
	})
}

// flameDepth returns the number of nodes on the longest path below node.
func flameDepth(node *FlameNode) int {
	depth := 0
	for _, child := range node.Children {
		depth = max(depth, 1+flameDepth(child))
	}
	return depth
}

// TestDeepStackIsTruncated verifies stacks are cut at maxStackDepth.
func TestDeepStackIsTruncated(t *testing.T) {
	const depth = 3 * maxStackDepth

	tests := map[string]*profilespb.ProfilesDictionary{
		"many locations": {
			StringTable:   []string{"", "leaf", "middle", "root"},
			FunctionTable: []*profilespb.Function{{}, {NameStrindex: 1}, {NameStrindex: 2}, {NameStrindex: 3}},
			LocationTable: []*profilespb.Location{
				{},
				{Lines: []*profilespb.Line{{FunctionIndex: 1}}},
				{Lines: []*profilespb.Line{{FunctionIndex: 2}}},
				{Lines: []*profilespb.Line{{FunctionIndex: 3}}},
			},
			StackTable: []*profilespb.Stack{{}, {LocationIndices: func() []int32 {
				// leaf-first: leaf, middle..., root
				indices := make([]int32, depth)
				for i := range indices {
					indices[i] = 2
				}
				indices[0], indices[depth-1] = 1, 3
				return indices
			}()}},
		},
		"many inlined functions": {
			StringTable:   []string{"", "leaf", "middle", "root"},
			FunctionTable: []*profilespb.Function{{}, {NameStrindex: 1}, {NameStrindex: 2}, {NameStrindex: 3}},
			LocationTable: []*profilespb.Location{{}, {Lines: func() []*profilespb.Line {
				lines := make([]*profilespb.Line, depth)
				for i := range lines {
					lines[i] = &profilespb.Line{FunctionIndex: 2}
				}
				lines[0], lines[depth-1] = &profilespb.Line{FunctionIndex: 1}, &profilespb.Line{FunctionIndex: 3}
				return lines
			}()}},
			StackTable: []*profilespb.Stack{{}, {LocationIndices: []int32{1}}},
		},
	}

	for name, dict := range tests {
		t.Run(name, func(t *testing.T) {
			entries := []store.ProfileEntry{{
				Dictionary: dict,
				Profile:    &profilespb.Profile{Samples: []*profilespb.Sample{{StackIndex: 1, Values: []int64{7}}}},
			}}
			root := ToFlamegraph(entries, Options{VirtualFrameKeys: []string{"service.name"}})

			// virtual frame + frames + truncation marker
			if got, want := flameDepth(root), maxStackDepth+2; got != want {
				t.Errorf("depth = %d, want %d", got, want)
			}

			// Cut at the leaf side, marker carries the sample value
			node := root.Children[0].Children[0]
			if node.Name != "root" {
				t.Errorf("first frame = %q, want root", node.Name)
			}
			for len(node.Children) > 0 {
				node = node.Children[0]
			}
			if node.Name != truncatedFrameName || node.Value != 7 {
				t.Errorf("last frame = %q with value %d, want %q with value 7", node.Name, node.Value, truncatedFrameName)
			}

			for view, v := range map[string]any{
				"flame graph": root,
				"sandwich":    ToSandwich(root),
				"callers":     ExtractSandwichGraphs(root, truncatedFrameName),
				"callees":     ExtractSandwichGraphs(root, "root"),
			} {
				if err := json.NewEncoder(io.Discard).Encode(v); err != nil {
					t.Errorf("%s cannot be encoded: %v", view, err)
				}
			}
		})
	}
}

// TestStackAtDepthLimitIsNotTruncated verifies stacks at the limit are kept whole.
func TestStackAtDepthLimitIsNotTruncated(t *testing.T) {
	indices := make([]int32, maxStackDepth)
	for i := range indices {
		indices[i] = 1
	}
	entries := []store.ProfileEntry{{
		Dictionary: &profilespb.ProfilesDictionary{
			StringTable:   []string{"", "f"},
			FunctionTable: []*profilespb.Function{{}, {NameStrindex: 1}},
			LocationTable: []*profilespb.Location{{}, {Lines: []*profilespb.Line{{FunctionIndex: 1}}}},
			StackTable:    []*profilespb.Stack{{}, {LocationIndices: indices}},
		},
		Profile: &profilespb.Profile{Samples: []*profilespb.Sample{{StackIndex: 1, Values: []int64{1}}}},
	}}
	root := ToFlamegraph(entries, Options{})
	if got := flameDepth(root); got != maxStackDepth {
		t.Errorf("depth = %d, want %d", got, maxStackDepth)
	}
	if stats := ToSandwich(root).Functions; len(stats) != 1 || stats[0].Name != "f" {
		t.Errorf("functions = %v, want only f", stats)
	}
}
