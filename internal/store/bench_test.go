package store

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// testDictionary returns a dictionary of numFunctions functions and numStacks
// random stacks. Its attribute table holds thread.name=worker at index 1.
func testDictionary(rng *rand.Rand, numFunctions, numStacks int) *profilespb.ProfilesDictionary {
	dict := &profilespb.ProfilesDictionary{
		StringTable:    []string{"", "samples", "count", "thread.name", "worker"},
		FunctionTable:  []*profilespb.Function{{}},
		LocationTable:  []*profilespb.Location{{}},
		StackTable:     []*profilespb.Stack{{}},
		AttributeTable: []*profilespb.KeyValueAndUnit{{}, {KeyStrindex: 3, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 4}}}},
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
	for range numStacks {
		stack := &profilespb.Stack{}
		for range 10 + rng.IntN(40) {
			stack.LocationIndices = append(stack.LocationIndices, int32(1+rng.IntN(numFunctions)))
		}
		dict.StackTable = append(dict.StackTable, stack)
	}
	return dict
}

// testProfile returns a samples(count) profile taken at ts with 300 samples
// of the stacks of a dictionary from testDictionary.
func testProfile(rng *rand.Rand, numStacks int, ts time.Time) *profilespb.Profile {
	profile := &profilespb.Profile{
		SampleType:   &profilespb.ValueType{TypeStrindex: 1, UnitStrindex: 2},
		TimeUnixNano: uint64(ts.UnixNano()),
	}
	for range 300 {
		profile.Samples = append(profile.Samples, &profilespb.Sample{
			StackIndex:       int32(1 + rng.IntN(numStacks)),
			AttributeIndices: []int32{1},
			Values:           []int64{1},
		})
	}
	return profile
}

// benchRequest returns a request whose profiles of different services share one dictionary.
func benchRequest(profiles int, ts time.Time) ([]*profilespb.ResourceProfiles, *profilespb.ProfilesDictionary) {
	const numStacks = 1500
	rng := rand.New(rand.NewPCG(1, 2))
	dict := testDictionary(rng, 2000, numStacks)

	var rps []*profilespb.ResourceProfiles
	for p := range profiles {
		rps = append(rps, &profilespb.ResourceProfiles{
			Resource:      &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", fmt.Sprintf("svc-%d", p))}},
			ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: []*profilespb.Profile{testProfile(rng, numStacks, ts)}}},
		})
	}
	return rps, dict
}

// BenchmarkAdd measures storing an export request with 20 profiles.
func BenchmarkAdd(b *testing.B) {
	rps, dict := benchRequest(20, time.Now())
	s := New(time.Hour, time.Hour, 0)
	defer s.Close()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if i%256 == 0 {
			// Bound the store size.
			s.mu.Lock()
			clear(s.entries)
			s.totalBytes = 0
			s.mu.Unlock()
		}
		s.Add(rps, dict)
	}
}

// BenchmarkReadDuringAdd measures reader latency during concurrent Add calls.
func BenchmarkReadDuringAdd(b *testing.B) {
	rps, dict := benchRequest(20, time.Now())
	s := New(time.Hour, time.Hour, 0)
	defer s.Close()

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			if i%256 == 0 {
				s.mu.Lock()
				clear(s.entries)
				s.totalBytes = 0
				s.mu.Unlock()
			}
			s.Add(rps, dict)
		}
	}()

	for b.Loop() {
		s.SampleTypes()
	}
	close(done)
	<-stopped
}

// BenchmarkCleanupNoop measures a cleanup that has nothing to remove.
func BenchmarkCleanupNoop(b *testing.B) {
	rps, dict := benchRequest(20, time.Now())
	s := New(time.Hour, time.Hour, 0)
	defer s.Close()
	for range 100 {
		s.Add(rps, dict)
	}

	b.ReportAllocs()
	for b.Loop() {
		s.cleanup()
	}
}

// BenchmarkEvictOldest measures evicting half of 4000 profiles.
func BenchmarkEvictOldest(b *testing.B) {
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(logger)

	const numEntries = 4000
	now := time.Now()
	rng := rand.New(rand.NewPCG(1, 2))
	entries := make([]ProfileEntry, numEntries)
	for i := range entries {
		entries[i] = ProfileEntry{
			CreatedAt: now.Add(-time.Duration(rng.IntN(1_000_000)) * time.Microsecond),
			Size:      1024,
		}
	}

	s := New(time.Hour, time.Hour, numEntries*1024/2)
	defer s.Close()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		s.mu.Lock()
		clear(s.entries)
		for i, e := range entries {
			key := fmt.Sprintf("type-%d", i%4)
			s.entries[key] = append(s.entries[key], e)
		}
		s.totalBytes = numEntries * 1024
		s.mu.Unlock()
		b.StartTimer()

		s.cleanup()
	}
}
