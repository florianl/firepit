package profiler

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"

	"github.com/florianl/firepit/internal/store"
)

func TestToFlamegraphEmpty(t *testing.T) {
	entries := []store.ProfileEntry{}
	root := ToFlamegraph(entries)

	if root == nil {
		t.Fatal("Root should not be nil")
	}

	if root.Name != "root" {
		t.Fatalf("Expected root name 'root', got '%s'", root.Name)
	}

	if root.Value != 0 {
		t.Fatalf("Expected root value 0, got %d", root.Value)
	}
}

func TestToFlamegraphWithProfile(t *testing.T) {
	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "func1", "func2"},
		LocationTable: []*profilespb.Location{
			{
				Address: 0x1000,
				Lines: []*profilespb.Line{
					{FunctionIndex: 1},
				},
			},
			{
				Address: 0x2000,
				Lines: []*profilespb.Line{
					{FunctionIndex: 2},
				},
			},
		},
		FunctionTable: []*profilespb.Function{
			{NameStrindex: 0},
			{NameStrindex: 1},
			{NameStrindex: 2},
		},
		StackTable: []*profilespb.Stack{
			{
				LocationIndices: []int32{0},
			},
			{
				LocationIndices: []int32{1, 0},
			},
		},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 0,
			UnitStrindex: 0,
		},
		Samples: []*profilespb.Sample{
			{
				StackIndex: 1,
				Values:     []int64{100},
			},
		},
	}

	entry := store.ProfileEntry{
		Profile:    profile,
		Dictionary: dict,
	}

	root := ToFlamegraph([]store.ProfileEntry{entry})

	if root.Value != 100 {
		t.Fatalf("Expected root value 100, got %d", root.Value)
	}

	if len(root.Children) == 0 {
		t.Fatal("Expected children in root")
	}
}

func TestFilterByResourceTypeEmpty(t *testing.T) {
	entries := []store.ProfileEntry{}
	filtered := FilterByResourceType(entries, "")

	if len(filtered) != 0 {
		t.Fatalf("Expected empty result, got %d entries", len(filtered))
	}
}

func TestFilterByResourceTypeNoFilter(t *testing.T) {
	entry := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
				},
			},
		},
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry}, "")
	if len(filtered) != 1 {
		t.Fatalf("Expected 1 entry with empty filter, got %d", len(filtered))
	}
}

func TestFilterByResourceTypeMatch(t *testing.T) {
	entry := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
				},
			},
		},
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry}, "service.name:test-service")
	if len(filtered) != 1 {
		t.Fatalf("Expected 1 matching entry, got %d", len(filtered))
	}
}

func TestFilterByResourceTypeNoMatch(t *testing.T) {
	entry := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
				},
			},
		},
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry}, "service.name:other-service")
	if len(filtered) != 0 {
		t.Fatalf("Expected 0 matching entries, got %d", len(filtered))
	}
}

func TestFilterByResourceTypeMultiple(t *testing.T) {
	entry1 := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "service-a"},
				},
			},
		},
	}

	entry2 := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "service-b"},
				},
			},
		},
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry1, entry2}, "service.name:service-a")
	if len(filtered) != 1 {
		t.Fatalf("Expected 1 matching entry, got %d", len(filtered))
	}

	if filtered[0].Attributes[0].Value.GetStringValue() != "service-a" {
		t.Fatal("Wrong entry returned")
	}
}

func TestFilterByResourceTypeNilAttributes(t *testing.T) {
	entry := store.ProfileEntry{
		Attributes: nil,
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry}, "service.name:test")
	if len(filtered) != 0 {
		t.Fatalf("Expected 0 entries for nil attributes, got %d", len(filtered))
	}
}

func TestNamedFlamegraph(t *testing.T) {
	ng := NamedFlamegraph{
		Type: "cpu",
		Root: &FlameNode{
			Name:  "root",
			Value: 1000,
		},
	}

	if ng.Type != "cpu" {
		t.Fatalf("Expected type 'cpu', got '%s'", ng.Type)
	}

	if ng.Root.Value != 1000 {
		t.Fatalf("Expected root value 1000, got %d", ng.Root.Value)
	}
}

func TestFilterByResourceTypeInvalidFormat(t *testing.T) {
	entry := store.ProfileEntry{
		Attributes: []*commonpb.KeyValue{
			{
				Key: "service.name",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
				},
			},
		},
	}

	filtered := FilterByResourceType([]store.ProfileEntry{entry}, "invalid-format")
	// Invalid format returns all entries (no filtering applied)
	if len(filtered) != 1 {
		t.Fatalf("Expected 1 entry for invalid format (graceful degradation), got %d", len(filtered))
	}
}
