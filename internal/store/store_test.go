package store

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestNewStore(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	if st == nil {
		t.Fatal("Store should not be nil")
	}
}

func TestAddAndRetrieve(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{
					Key: "service.name",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
					},
				},
			},
		},
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	types := st.SampleTypes()
	if len(types) != 1 || types[0] != "main(bytes)" {
		t.Fatalf("Expected sample type 'main(bytes)', got %v", types)
	}

	entries := st.ProfileEntries("main(bytes)")
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}

	if entries[0].Profile != profile {
		t.Fatal("Retrieved profile does not match")
	}

	if entries[0].Dictionary != dict {
		t.Fatal("Retrieved dictionary does not match")
	}

	if len(entries[0].Attributes) != 1 {
		t.Fatal("Expected 1 attribute")
	}

	if entries[0].Attributes[0].Key != "service.name" {
		t.Fatal("Attribute key mismatch")
	}
}

func TestResourceTypes(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{
					Key: "service.name",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "service-a"},
					},
				},
				{
					Key: "version",
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "v1.0"},
					},
				},
			},
		},
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	types := st.ResourceTypes()
	if len(types) != 2 {
		t.Fatalf("Expected 2 resource types, got %d", len(types))
	}

	// Check that both attributes are returned
	found := make(map[string]bool)
	for _, t := range types {
		found[t] = true
	}

	if !found["service.name:service-a"] {
		t.Fatal("Expected 'service.name:service-a' in resource types")
	}

	if !found["version:v1.0"] {
		t.Fatal("Expected 'version:v1.0' in resource types")
	}
}

func TestStats(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	count, minTime, maxTime, ok := st.Stats()
	if !ok {
		t.Fatal("Stats should return ok=true when entries exist")
	}

	if count != 1 {
		t.Fatalf("Expected count=1, got %d", count)
	}

	if minTime.After(maxTime) {
		t.Fatal("minTime should not be after maxTime")
	}
}

func TestEmptyStats(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	count, _, _, ok := st.Stats()
	if ok {
		t.Fatal("Stats should return ok=false when no entries exist")
	}

	if count != 0 {
		t.Fatalf("Expected count=0, got %d", count)
	}
}

func TestCloseStore(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	// Should not panic when closing
	st.Close()
}

func TestAddMultipleProfiles(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "cpu", "memory"},
	}

	profile1 := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	profile2 := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 3,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile1, profile2},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	types := st.SampleTypes()
	if len(types) != 2 {
		t.Fatalf("Expected 2 sample types, got %d", len(types))
	}
}

func TestNilResourceAttributes(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		Resource: nil,
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	entries := st.ProfileEntries("main(bytes)")
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}

	if entries[0].Attributes != nil {
		t.Fatal("Expected nil attributes")
	}
}

func TestMemoryLimitEviction(t *testing.T) {
	maxMemory := int64(1)
	st := New(5*time.Minute, 10*time.Second, maxMemory)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile1 := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	profile2 := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp1 := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile1},
			},
		},
	}

	rp2 := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile2},
			},
		},
	}

	st.Add([]*profilespb.ResourceProfiles{rp1}, dict)
	st.Add([]*profilespb.ResourceProfiles{rp2}, dict)

	entries := st.ProfileEntries("main(bytes)")
	if len(entries) != 2 {
		t.Fatalf("Expected 2 entries before cleanup, got %d", len(entries))
	}

	st.cleanup()

	entries = st.ProfileEntries("main(bytes)")
	if len(entries) >= 2 {
		t.Fatalf("Expected fewer than 2 entries after memory eviction, got %d", len(entries))
	}
}

func TestMemoryLimitDisabled(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"root", "main", "bytes"},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{
			TypeStrindex: 1,
			UnitStrindex: 2,
		},
		Samples:      []*profilespb.Sample{},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{
			{
				Profiles: []*profilespb.Profile{profile},
			},
		},
	}

	for i := 0; i < 10; i++ {
		st.Add([]*profilespb.ResourceProfiles{rp}, dict)
	}

	entries := st.ProfileEntries("main(bytes)")
	if len(entries) != 10 {
		t.Fatalf("Expected 10 entries with memory limit disabled, got %d", len(entries))
	}

	st.cleanup()

	entries = st.ProfileEntries("main(bytes)")
	if len(entries) != 10 {
		t.Fatalf("Expected 10 entries after cleanup with limit disabled, got %d", len(entries))
	}
}
