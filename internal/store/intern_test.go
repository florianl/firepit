package store

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"testing"
	"time"
	"unsafe"

	collectorprofiles "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/protobuf/proto"
)

// internTestRequest returns an encoded export request with numFunctions
// functions in its dictionary.
func internTestRequest(tb testing.TB, numFunctions int) []byte {
	tb.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	dict := &profilespb.ProfilesDictionary{
		StringTable:   []string{"", "samples", "count"},
		FunctionTable: []*profilespb.Function{{}},
		LocationTable: []*profilespb.Location{{}},
		StackTable:    []*profilespb.Stack{{}},
	}
	for f := range numFunctions {
		dict.StringTable = append(dict.StringTable,
			fmt.Sprintf("github.com/example/project/internal/pkg%d.(*Type%d).Method%d", f%40, f%97, f),
			fmt.Sprintf("/home/build/src/github.com/example/project/internal/pkg%d/file%d.go", f%40, f))
		n := int32(len(dict.StringTable))
		dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{NameStrindex: n - 2, FilenameStrindex: n - 1})
		dict.LocationTable = append(dict.LocationTable, &profilespb.Location{
			Address: uint64(0x400000 + 16*f),
			Lines:   []*profilespb.Line{{FunctionIndex: int32(f + 1), Line: int64(1 + f%500)}},
		})
	}
	for range numFunctions / 2 {
		stack := &profilespb.Stack{}
		for range 10 + rng.IntN(40) {
			stack.LocationIndices = append(stack.LocationIndices, int32(1+rng.IntN(numFunctions)))
		}
		dict.StackTable = append(dict.StackTable, stack)
	}
	profile := &profilespb.Profile{
		SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
		TimeUnixNano: uint64(time.Now().UnixNano()),
	}
	for range 300 {
		profile.Samples = append(profile.Samples, &profilespb.Sample{
			StackIndex: int32(1 + rng.IntN(numFunctions/2)),
			Values:     []int64{1},
		})
	}
	wire, err := proto.Marshal(&collectorprofiles.ExportProfilesServiceRequest{
		ResourceProfiles: []*profilespb.ResourceProfiles{{
			ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{profile}}},
		}},
		Dictionary: dict,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return wire
}

// addDecoded decodes wire, like a receiver does, and adds it to s.
func addDecoded(tb testing.TB, s *Store, wire []byte) {
	tb.Helper()
	req := &collectorprofiles.ExportProfilesServiceRequest{}
	if err := proto.Unmarshal(wire, req); err != nil {
		tb.Fatal(err)
	}
	s.Add(req.ResourceProfiles, req.Dictionary)
}

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestDictionariesShareStrings(t *testing.T) {
	s := New(time.Hour, time.Hour, 0)
	defer s.Close()

	wire := internTestRequest(t, 50)
	addDecoded(t, s, wire)
	addDecoded(t, s, wire)

	entries := s.ProfileEntries("samples(count)")
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	first, second := entries[0].Dictionary.StringTable, entries[1].Dictionary.StringTable
	if len(first) != len(second) || len(first) < 100 {
		t.Fatalf("string tables have %d and %d strings", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("string %d differs: %q, %q", i, first[i], second[i])
		}
		if len(first[i]) > 0 && unsafe.StringData(first[i]) != unsafe.StringData(second[i]) {
			t.Fatalf("string %d (%q) is stored twice", i, first[i])
		}
	}
}

// TestInternedStringsMemory checks that repeated strings are stored once and
// released on eviction.
func TestInternedStringsMemory(t *testing.T) {
	const requests = 40
	wire := internTestRequest(t, 2000)

	s := New(time.Hour, time.Hour, 0)
	defer s.Close()

	empty := liveHeap()
	addDecoded(t, s, wire)
	one := liveHeap() - empty
	for range requests - 1 {
		addDecoded(t, s, wire)
	}
	all := liveHeap() - empty

	perRequest := float64(all-one) / (requests - 1)
	t.Logf("first request: %d bytes, each further request: %.0f bytes (%.0f%%)", one, perRequest, 100*perRequest/float64(one))
	if perRequest > 0.75*float64(one) {
		t.Errorf("a request repeating known strings needs %.0f bytes, the first one %d; want at most 75%%", perRequest, one)
	}

	// Evicting all profiles must release the interned strings.
	s.mu.Lock()
	clear(s.entries)
	s.totalBytes = 0
	s.mu.Unlock()
	if left := int64(liveHeap()) - int64(empty); left > int64(one)/4 {
		t.Errorf("%d bytes still in use after evicting all profiles", left)
	}
	runtime.KeepAlive(s)
}

// BenchmarkAddDecoded measures decoding and storing an export request.
func BenchmarkAddDecoded(b *testing.B) {
	wire := internTestRequest(b, 2000)
	s := New(time.Hour, time.Hour, 0)
	defer s.Close()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if i%64 == 0 {
			s.mu.Lock()
			clear(s.entries)
			s.totalBytes = 0
			s.mu.Unlock()
		}
		addDecoded(b, s, wire)
	}
}
