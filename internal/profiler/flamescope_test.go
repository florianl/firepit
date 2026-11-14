package profiler

import (
	"testing"

	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"

	"github.com/florianl/firepit/internal/store"
)

// heatMapEntry returns an entry for a profile at time t with the given samples.
func heatMapEntry(t uint64, samples ...*profilespb.Sample) store.ProfileEntry {
	return store.ProfileEntry{
		Profile:    &profilespb.Profile{TimeUnixNano: t, Samples: samples},
		Dictionary: &profilespb.ProfilesDictionary{},
	}
}

func TestToHeatMap(t *testing.T) {
	const base = uint64(1_000_000_000_000)

	tests := []struct {
		name       string
		entries    []store.ProfileEntry
		numSeconds int
		maxValue   int64
		// cells maps cell index (second*numSubBuckets + bucket) to its value
		cells map[int]int64
	}{
		{
			name: "empty",
		},
		{
			name:       "single entry values",
			entries:    []store.ProfileEntry{heatMapEntry(base, &profilespb.Sample{Values: []int64{5}})},
			numSeconds: 1,
			maxValue:   5,
			cells:      map[int]int64{0: 5},
		},
		{
			name: "timestamps",
			entries: []store.ProfileEntry{heatMapEntry(base, &profilespb.Sample{
				TimestampsUnixNano: []uint64{base + 500_000_000, base + 1_500_000_000},
			})},
			numSeconds: 2,
			maxValue:   1,
			cells:      map[int]int64{50: 1, numSubBuckets + 50: 1},
		},
		{
			name: "max value",
			entries: []store.ProfileEntry{
				heatMapEntry(base, &profilespb.Sample{Values: []int64{10}}),
				heatMapEntry(base+100_000_000, &profilespb.Sample{Values: []int64{25}}),
			},
			numSeconds: 1,
			maxValue:   25,
			cells:      map[int]int64{0: 10, 10: 25},
		},
		{
			name:    "zero profile time is skipped",
			entries: []store.ProfileEntry{heatMapEntry(0, &profilespb.Sample{Values: []int64{5}})},
		},
		{
			name: "values and timestamps",
			entries: []store.ProfileEntry{
				heatMapEntry(base, &profilespb.Sample{Values: []int64{5}}),
				heatMapEntry(base+1_000_000_000, &profilespb.Sample{
					TimestampsUnixNano: []uint64{base + 1_500_000_000},
				}),
			},
			numSeconds: 2,
			maxValue:   5,
			cells:      map[int]int64{0: 5, numSubBuckets + 50: 1},
		},
		{
			name: "timestamps of a profile without time",
			entries: []store.ProfileEntry{
				heatMapEntry(base, &profilespb.Sample{Values: []int64{5}}),
				heatMapEntry(0, &profilespb.Sample{TimestampsUnixNano: []uint64{base - 1_500_000_000}}),
			},
			numSeconds: 2,
			maxValue:   5,
			cells:      map[int]int64{50: 1, numSubBuckets: 5},
		},
	}
	for _, tt := range tests {
		hm := ToHeatMap(tt.entries)
		if hm.NumSeconds != tt.numSeconds || hm.MaxValue != tt.maxValue {
			t.Errorf("%s: NumSeconds=%d MaxValue=%d, want %d %d",
				tt.name, hm.NumSeconds, hm.MaxValue, tt.numSeconds, tt.maxValue)
			continue
		}
		if len(hm.Cells) != tt.numSeconds*numSubBuckets {
			t.Errorf("%s: %d cells, want %d", tt.name, len(hm.Cells), tt.numSeconds*numSubBuckets)
			continue
		}
		for i, c := range hm.Cells {
			if c != tt.cells[i] {
				t.Errorf("%s: cells[%d]=%d, want %d", tt.name, i, c, tt.cells[i])
			}
		}
	}
}
