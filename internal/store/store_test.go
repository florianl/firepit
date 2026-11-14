package store

import (
	"slices"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// strAttr returns a string attribute.
func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

// addProfile adds a profile of sample type "<sampleType>(count)" taken at ts
// with the resource attributes attrs to s and returns it.
func addProfile(s *Store, sampleType string, ts time.Time, attrs ...*commonpb.KeyValue) *profilespb.Profile {
	profile := &profilespb.Profile{
		SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
		TimeUnixNano: uint64(ts.UnixNano()),
	}
	rp := &profilespb.ResourceProfiles{
		ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{profile}}},
	}
	if len(attrs) > 0 {
		rp.Resource = &resourcepb.Resource{Attributes: attrs}
	}
	s.Add([]*profilespb.ResourceProfiles{rp}, &profilespb.ProfilesDictionary{StringTable: []string{"", sampleType, "count"}})
	return profile
}

func TestAddAndRetrieve(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	profile := addProfile(st, "cpu", time.Now(), strAttr("service.name", "test-service"))
	addProfile(st, "alloc", time.Now())

	if got, want := st.SampleTypes(), []string{"alloc(count)", "cpu(count)"}; !slices.Equal(got, want) {
		t.Fatalf("SampleTypes() = %v, want %v", got, want)
	}

	entries := st.ProfileEntries("cpu(count)")
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}

	if entries[0].Profile != profile {
		t.Fatal("Retrieved profile does not match")
	}

	if got := Lookup(entries[0].Dictionary.GetStringTable(), 1); got != "cpu" {
		t.Fatalf("Retrieved dictionary does not match, string 1 is %q", got)
	}

	if len(entries[0].ResourceAttributes) != 1 {
		t.Fatal("Expected 1 attribute")
	}

	if entries[0].ResourceAttributes[0].Key != "service.name" {
		t.Fatal("Attribute key mismatch")
	}

	if attrs := st.ProfileEntries("alloc(count)")[0].ResourceAttributes; attrs != nil {
		t.Fatalf("Expected nil attributes, got %v", attrs)
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

	if count, _, _, ok := st.Stats(); ok || count != 0 {
		t.Fatalf("Stats() of an empty store = %d, %v, want 0, false", count, ok)
	}

	now := time.Now().Truncate(time.Second)
	addProfile(st, "cpu", now)
	addProfile(st, "alloc", now.Add(-time.Second))

	count, minTime, maxTime, ok := st.Stats()
	if !ok || count != 2 || !minTime.Equal(now.Add(-time.Second)) || !maxTime.Equal(now) {
		t.Fatalf("Stats() = %d, %v, %v, %v, want 2, %v, %v, true", count, minTime, maxTime, ok, now.Add(-time.Second), now)
	}
}

func TestMemoryLimitDisabled(t *testing.T) {
	st := New(5*time.Minute, 10*time.Second, 0)
	defer st.Close()

	for range 10 {
		addProfile(st, "cpu", time.Now())
	}
	st.cleanup()
	if n := len(st.ProfileEntries("cpu(count)")); n != 10 {
		t.Fatalf("%d entries after cleanup with limit disabled, want 10", n)
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

	addProfile(s, "cpu", time.Now())
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
	addProfile(s, "cpu", time.Now().Add(-time.Hour))
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

	addProfile(s, "cpu", time.Now(), strAttr("m", "m"))
	addProfile(s, "cpu", time.Now(), strAttr("z", "z"))
	types, keys, attrs := s.ResourceTypes(), s.AttributeKeys(), s.ResourceAttributes()
	wantTypes, wantKeys, wantAttrs := slices.Clone(types), slices.Clone(keys), slices.Clone(attrs)

	// "a" sorts before the existing values.
	addProfile(s, "cpu", time.Now(), strAttr("a", "a"))
	if !slices.Equal(types, wantTypes) || !slices.Equal(keys, wantKeys) || !slices.Equal(attrs, wantAttrs) {
		t.Errorf("returned slices were modified: %v, %v, %v; want %v, %v, %v", types, keys, attrs, wantTypes, wantKeys, wantAttrs)
	}
	if got, want := s.ResourceTypes(), []string{"a:a", "m:m", "z:z"}; !slices.Equal(got, want) {
		t.Errorf("ResourceTypes() = %v, want %v", got, want)
	}
	if got, want := s.AttributeKeys(), []string{"a", "m", "z"}; !slices.Equal(got, want) {
		t.Errorf("AttributeKeys() = %v, want %v", got, want)
	}
	if got, want := s.ResourceAttributes(), []ResourceAttribute{{"a", "a"}, {"m", "m"}, {"z", "z"}}; !slices.Equal(got, want) {
		t.Errorf("ResourceAttributes() = %v, want %v", got, want)
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

func filterTestAttributes() ([]string, []*commonpb.KeyValue) {
	st := []string{"", "host.name", "node-1"}
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	return st, []*commonpb.KeyValue{
		{Key: "service.name", Value: str("api")},
		{Key: "process.pid", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
		{Key: "debug", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
		{Key: "load", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}}},
		{Key: "k8s:label", Value: str("a:b")},
		{KeyStrindex: 1, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 2}}},
		{Key: "tags", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{str("x")}}}}},
		{Key: "empty"},
	}
}

func TestMatchResourceFilters(t *testing.T) {
	st, attrs := filterTestAttributes()

	tests := []struct {
		filters []string
		want    bool
	}{
		{nil, true},
		{[]string{"service.name:api"}, true},
		{[]string{"service.name:db"}, false},
		{[]string{"service.name:ap"}, false},
		{[]string{"service.nam:api"}, false},
		// Non-string scalars
		{[]string{"process.pid:42"}, true},
		{[]string{"process.pid:43"}, false},
		{[]string{"debug:true"}, true},
		{[]string{"load:0.5"}, true},
		// Colons in key and value
		{[]string{"k8s:label:a:b"}, true},
		{[]string{"k8s:label:a"}, false},
		// Dictionary references
		{[]string{"host.name:node-1"}, true},
		// All filters must match
		{[]string{"service.name:api", "process.pid:42", "k8s:label:a:b"}, true},
		{[]string{"service.name:api", "process.pid:43"}, false},
		{[]string{"service.name:db", "process.pid:42"}, false},
		// Non-scalar values
		{[]string{"tags:x"}, false},
		{[]string{"empty:"}, false},
		// Filters without a colon are ignored
		{[]string{"service.name"}, true},
		{[]string{"", "service.name:api"}, true},
	}
	for _, tt := range tests {
		if got := MatchResourceFilters(st, attrs, tt.filters); got != tt.want {
			t.Errorf("MatchResourceFilters(%q) = %v, want %v", tt.filters, got, tt.want)
		}
	}
}

func TestResourceAttributes(t *testing.T) {
	s := New(time.Minute, time.Hour, 0)
	defer s.Close()

	st, attrs := filterTestAttributes()
	s.Add([]*profilespb.ResourceProfiles{{
		Resource: &resourcepb.Resource{Attributes: attrs},
		ScopeProfiles: []*profilespb.ScopeProfiles{{
			Profiles: []*profilespb.Profile{{TimeUnixNano: uint64(time.Now().UnixNano())}},
		}},
	}}, &profilespb.ProfilesDictionary{StringTable: st})

	want := []ResourceAttribute{
		{"debug", "true"},
		{"host.name", "node-1"},
		{"k8s:label", "a:b"},
		{"load", "0.5"},
		{"process.pid", "42"},
		{"service.name", "api"},
	}
	if got := s.ResourceAttributes(); !slices.Equal(got, want) {
		t.Errorf("ResourceAttributes() = %v, want %v", got, want)
	}

	// Every offered attribute matches its own resource.
	for _, filter := range s.ResourceTypes() {
		if !MatchResourceFilters(st, attrs, []string{filter}) {
			t.Errorf("resource type %q does not match the resource it stems from", filter)
		}
	}
}

func TestMetrics(t *testing.T) {
	s := New(time.Minute, time.Hour, 0)
	defer s.Close()

	now := time.Now()
	addProfile(s, "cpu", now)
	addProfile(s, "cpu", now.Add(-time.Second))
	addProfile(s, "alloc", now.Add(-2*time.Second))
	addProfile(s, "alloc", now.Add(-time.Hour)) // expired
	s.Add([]*profilespb.ResourceProfiles{{}}, nil)

	m := s.Metrics()
	if m.Profiles != 4 || m.SampleTypes != 2 || m.AddedProfiles != 4 || m.DroppedRequests != 1 || m.Bytes <= 0 {
		t.Errorf("after adding: %+v", m)
	}

	s.cleanup()
	m = s.Metrics()
	if m.Profiles != 3 || m.ExpiredProfiles != 1 || m.EvictedProfiles != 0 {
		t.Errorf("after expiry: %+v", m)
	}

	// Leave room for one profile only.
	s.mu.Lock()
	s.maxMemory = s.totalBytes / 3
	s.mu.Unlock()
	s.cleanup()
	m = s.Metrics()
	if m.Profiles != 1 || m.EvictedProfiles != 2 || m.ExpiredProfiles != 1 || m.AddedProfiles != 4 {
		t.Errorf("after eviction: %+v", m)
	}
	if m.MaxBytes == 0 || m.Bytes > m.MaxBytes {
		t.Errorf("after eviction: %d bytes with a limit of %d", m.Bytes, m.MaxBytes)
	}
}
