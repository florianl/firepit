package profiler

import (
	"testing"

	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"

	"github.com/florianl/firepit/internal/store"
)

func TestToHeatMapEmpty(t *testing.T) {
	entries := []store.ProfileEntry{}
	hm := ToHeatMap(entries)

	if hm.NumSeconds != 0 {
		t.Fatalf("Expected empty heatmap, got NumSeconds=%d", hm.NumSeconds)
	}
}

func TestToHeatMapSingleEntryValues(t *testing.T) {
	// Profile with TimeUnixNano=1000000000000 and one sample with Values=[5]
	profile := &profilespb.Profile{
		TimeUnixNano: 1_000_000_000_000,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				Values:     []int64{5},
			},
		},
	}

	entry := store.ProfileEntry{
		Profile:    profile,
		Dictionary: &profilespb.ProfilesDictionary{},
	}

	hm := ToHeatMap([]store.ProfileEntry{entry})

	if hm.NumSeconds != 1 {
		t.Fatalf("Expected NumSeconds=1, got %d", hm.NumSeconds)
	}

	if hm.MaxValue != 5 {
		t.Fatalf("Expected MaxValue=5, got %d", hm.MaxValue)
	}

	if len(hm.Cells) != 100 {
		t.Fatalf("Expected 100 cells, got %d", len(hm.Cells))
	}

	// The sample should fall into the first bucket (xi=0, yi=0 since TimeUnixNano % 1e9 = 0)
	if hm.Cells[0] != 5 {
		t.Fatalf("Expected cells[0]=5, got %d", hm.Cells[0])
	}
}

func TestToHeatMapWithTimestamps(t *testing.T) {
	// Profile with timestamps spanning two seconds
	baseTime := uint64(1_000_000_000_000)
	profile := &profilespb.Profile{
		TimeUnixNano: baseTime,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				TimestampsUnixNano: []uint64{
					baseTime + 500_000_000,   // 0.5 seconds in
					baseTime + 1_500_000_000, // 1.5 seconds in
				},
			},
		},
	}

	entry := store.ProfileEntry{
		Profile:    profile,
		Dictionary: &profilespb.ProfilesDictionary{},
	}

	hm := ToHeatMap([]store.ProfileEntry{entry})

	if hm.NumSeconds != 2 {
		t.Fatalf("Expected NumSeconds=2, got %d", hm.NumSeconds)
	}

	if hm.MaxValue != 1 {
		t.Fatalf("Expected MaxValue=1, got %d", hm.MaxValue)
	}

	// First timestamp: xi=0 (first second), yi=50 (500ms / 10ms per bucket)
	xi0Yi50 := 0*numSubBuckets + 50
	// Second timestamp: xi=1 (second second), yi=50 (500ms / 10ms per bucket)
	xi1Yi50 := 1*numSubBuckets + 50

	if hm.Cells[xi0Yi50] != 1 {
		t.Fatalf("Expected cells[0*100+50]=1, got %d", hm.Cells[xi0Yi50])
	}

	if hm.Cells[xi1Yi50] != 1 {
		t.Fatalf("Expected cells[1*100+50]=1, got %d", hm.Cells[xi1Yi50])
	}
}

func TestToHeatMapMaxValue(t *testing.T) {
	baseTime := uint64(1_000_000_000_000)

	// First entry: 10 samples
	profile1 := &profilespb.Profile{
		TimeUnixNano: baseTime,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				Values:     []int64{10},
			},
		},
	}

	// Second entry: 25 samples
	profile2 := &profilespb.Profile{
		TimeUnixNano: baseTime + 100_000_000,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				Values:     []int64{25},
			},
		},
	}

	entries := []store.ProfileEntry{
		{Profile: profile1, Dictionary: &profilespb.ProfilesDictionary{}},
		{Profile: profile2, Dictionary: &profilespb.ProfilesDictionary{}},
	}

	hm := ToHeatMap(entries)

	if hm.MaxValue != 25 {
		t.Fatalf("Expected MaxValue=25, got %d", hm.MaxValue)
	}
}

func TestToHeatMapZeroTimeUnixNano(t *testing.T) {
	// Profile with zero TimeUnixNano and no timestamps - should be skipped
	profile := &profilespb.Profile{
		TimeUnixNano: 0,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				Values:     []int64{5},
			},
		},
	}

	entry := store.ProfileEntry{
		Profile:    profile,
		Dictionary: &profilespb.ProfilesDictionary{},
	}

	hm := ToHeatMap([]store.ProfileEntry{entry})

	if hm.NumSeconds != 0 {
		t.Fatalf("Expected empty heatmap for zero TimeUnixNano, got NumSeconds=%d", hm.NumSeconds)
	}
}

func TestToHeatMapMixedScenarios(t *testing.T) {
	baseTime := uint64(2_000_000_000_000)

	// Profile with TimeUnixNano + Values
	profile1 := &profilespb.Profile{
		TimeUnixNano: baseTime,
		Samples: []*profilespb.Sample{
			{
				StackIndex: 0,
				Values:     []int64{5},
			},
		},
	}

	// Profile with timestamps
	profile2 := &profilespb.Profile{
		TimeUnixNano: baseTime + 1_000_000_000,
		Samples: []*profilespb.Sample{
			{
				StackIndex:         0,
				TimestampsUnixNano: []uint64{baseTime + 1_500_000_000},
			},
		},
	}

	entries := []store.ProfileEntry{
		{Profile: profile1, Dictionary: &profilespb.ProfilesDictionary{}},
		{Profile: profile2, Dictionary: &profilespb.ProfilesDictionary{}},
	}

	hm := ToHeatMap(entries)

	if hm.NumSeconds != 2 {
		t.Fatalf("Expected NumSeconds=2, got %d", hm.NumSeconds)
	}

	if hm.MaxValue != 5 {
		t.Fatalf("Expected MaxValue=5, got %d", hm.MaxValue)
	}

	// First second, bucket 0: value 5
	if hm.Cells[0] != 5 {
		t.Fatalf("Expected cells[0]=5, got %d", hm.Cells[0])
	}

	// Second second, bucket 50: value 1
	xi1Yi50 := 1*numSubBuckets + 50
	if hm.Cells[xi1Yi50] != 1 {
		t.Fatalf("Expected cells[1*100+50]=1, got %d", hm.Cells[xi1Yi50])
	}
}
