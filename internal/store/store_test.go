package store

import (
	"slices"
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

	if len(entries[0].ResourceAttributes) != 1 {
		t.Fatal("Expected 1 attribute")
	}

	if entries[0].ResourceAttributes[0].Key != "service.name" {
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

func TestResourceTypesDictionaryReferences(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"", "samples", "count", "process.executable.name", "dockerd", "process.executable.path"},
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
					KeyStrindex: 3,
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 4},
					},
				},
				{
					// key_strindex takes precedence over key
					Key:         "ignored",
					KeyStrindex: 5,
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "/usr/bin/dockerd"},
					},
				},
				{
					KeyStrindex: 0,
					Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 4},
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

	want := []string{"process.executable.name:dockerd", "process.executable.path:/usr/bin/dockerd"}
	if got := st.ResourceTypes(); !slices.Equal(got, want) {
		t.Fatalf("Expected %v, got %v", want, got)
	}

	// cleanup recomputes the resource types from the stored entries
	st.cleanup()
	if got := st.ResourceTypes(); !slices.Equal(got, want) {
		t.Fatalf("Expected %v after cleanup, got %v", want, got)
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

	if entries[0].ResourceAttributes != nil {
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

	for range 10 {
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

func TestAttributeKeys(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"", "cpu", "thread.name", "worker"},
		AttributeTable: []*profilespb.KeyValueAndUnit{
			{},
			{
				KeyStrindex: 2,
				Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "worker"}},
			},
		},
	}

	profile := &profilespb.Profile{
		SampleType: &profilespb.ValueType{TypeStrindex: 1},
		Samples: []*profilespb.Sample{
			{AttributeIndices: []int32{1}},
			{AttributeIndices: []int32{1, 99}}, // out of range index is ignored
		},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}

	rp := &profilespb.ResourceProfiles{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}},
				},
				{
					KeyStrindex: 2, // non-string values still contribute their key
					Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}},
				},
			},
		},
		ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{profile}}},
	}

	st.Add([]*profilespb.ResourceProfiles{rp}, dict)

	want := []string{"service.name", "thread.name"}
	if got := st.AttributeKeys(); !slices.Equal(got, want) {
		t.Fatalf("AttributeKeys() = %v, want %v", got, want)
	}

	// Keys must survive a cleanup while the entries are retained.
	st.cleanup()
	if got := st.AttributeKeys(); !slices.Equal(got, want) {
		t.Fatalf("AttributeKeys() after cleanup = %v, want %v", got, want)
	}
}

func TestGeneration(t *testing.T) {
	s := New(time.Minute, time.Hour, 0)
	defer s.Close()

	gen := s.Generation()

	// Nothing is stored without a dictionary or without profiles.
	s.Add([]*profilespb.ResourceProfiles{{}}, nil)
	s.Add(nil, &profilespb.ProfilesDictionary{StringTable: []string{""}})
	if got := s.Generation(); got != gen {
		t.Fatalf("generation changed from %d to %d without storing a profile", gen, got)
	}

	add := func(ts time.Time) {
		s.Add([]*profilespb.ResourceProfiles{{
			ScopeProfiles: []*profilespb.ScopeProfiles{{
				Profiles: []*profilespb.Profile{{TimeUnixNano: uint64(ts.UnixNano())}},
			}},
		}}, &profilespb.ProfilesDictionary{StringTable: []string{""}})
	}

	add(time.Now())
	afterAdd := s.Generation()
	if afterAdd == gen {
		t.Fatal("generation did not change after adding a profile")
	}

	// A cleanup that removes nothing keeps the generation.
	s.cleanup()
	if got := s.Generation(); got != afterAdd {
		t.Fatalf("generation changed from %d to %d by a cleanup removing nothing", afterAdd, got)
	}

	// An expired profile is removed by the cleanup.
	add(time.Now().Add(-time.Hour))
	afterExpired := s.Generation()
	s.cleanup()
	if got := s.Generation(); got == afterExpired {
		t.Fatal("generation did not change after removing an expired profile")
	}
}

// Readers use the returned slices without the lock, so Add must not modify them.
func TestAddDoesNotModifyReturnedSlices(t *testing.T) {
	s := New(time.Minute, time.Hour, 0)
	defer s.Close()

	add := func(service string) {
		s.Add([]*profilespb.ResourceProfiles{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: service, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}},
			}},
			ScopeProfiles: []*profilespb.ScopeProfiles{{
				Profiles: []*profilespb.Profile{{TimeUnixNano: uint64(time.Now().UnixNano())}},
			}},
		}}, &profilespb.ProfilesDictionary{StringTable: []string{""}})
	}

	add("m")
	add("z")
	types, keys := s.ResourceTypes(), s.AttributeKeys()
	wantTypes, wantKeys := slices.Clone(types), slices.Clone(keys)

	// "a" sorts before the existing values.
	add("a")
	if !slices.Equal(types, wantTypes) || !slices.Equal(keys, wantKeys) {
		t.Errorf("returned slices were modified: %v, %v; want %v, %v", types, keys, wantTypes, wantKeys)
	}
	if got, want := s.ResourceTypes(), []string{"a:a", "m:m", "z:z"}; !slices.Equal(got, want) {
		t.Errorf("ResourceTypes() = %v, want %v", got, want)
	}
	if got, want := s.AttributeKeys(), []string{"a", "m", "z"}; !slices.Equal(got, want) {
		t.Errorf("AttributeKeys() = %v, want %v", got, want)
	}
}

// Resources without profiles are not offered as filter.
func TestAddResourceWithoutProfiles(t *testing.T) {
	s := New(time.Minute, time.Hour, 0)
	defer s.Close()

	s.Add([]*profilespb.ResourceProfiles{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "idle"}}},
		}},
	}}, &profilespb.ProfilesDictionary{StringTable: []string{""}})

	if got := s.ResourceTypes(); len(got) != 0 {
		t.Errorf("ResourceTypes() = %v, want none", got)
	}
}
