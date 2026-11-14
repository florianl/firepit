package profiler

import (
	"slices"

	"github.com/florianl/firepit/internal/store"
)

const (
	numSubBuckets  = 100
	subBucketNanos = uint64(10_000_000) // 10 ms
)

type HeatMapData struct {
	MinTimeSec    int64   `json:"minTimeSec"`
	NumSeconds    int     `json:"numSeconds"`
	NumSubBuckets int     `json:"numSubBuckets"`
	SubBucketMs   int     `json:"subBucketMs"`
	Cells         []int64 `json:"cells"`
	MaxValue      int64   `json:"maxValue"`
}

type NamedFlamescope struct {
	Type string      `json:"type"`
	Data HeatMapData `json:"data"`
}

func ToHeatMap(entries []store.ProfileEntry) HeatMapData {
	if len(entries) == 0 {
		return HeatMapData{}
	}

	var minT, maxT uint64
	hasValidTime := false
	extend := func(ts uint64) {
		if !hasValidTime {
			minT, maxT, hasValidTime = ts, ts, true
			return
		}
		minT = min(ts, minT)
		maxT = max(ts, maxT)
	}

	for _, entry := range entries {
		if entry.Profile == nil {
			continue
		}
		if t := entry.Profile.TimeUnixNano; t != 0 {
			extend(t)
		}
		for _, sample := range entry.Profile.Samples {
			for _, ts := range sample.GetTimestampsUnixNano() {
				extend(ts)
			}
		}
	}

	if !hasValidTime {
		return HeatMapData{}
	}

	numSeconds := min(int((maxT-minT)/1_000_000_000)+1, 3600)
	cells := make([]int64, numSeconds*numSubBuckets)
	cell := func(ts uint64) *int64 {
		// Timestamps beyond the capped range go into the last column.
		x := min(int((ts-minT)/1_000_000_000), numSeconds-1)
		y := int((ts % 1_000_000_000) / subBucketNanos)
		return &cells[x*numSubBuckets+y]
	}

	for _, entry := range entries {
		if entry.Profile == nil {
			continue
		}
		for _, sample := range entry.Profile.Samples {
			if sample == nil {
				continue
			}
			if len(sample.TimestampsUnixNano) > 0 {
				for _, ts := range sample.TimestampsUnixNano {
					*cell(ts)++
				}
			} else if len(sample.Values) > 0 && sample.Values[0] > 0 && entry.Profile.TimeUnixNano > 0 {
				*cell(entry.Profile.TimeUnixNano) += sample.Values[0]
			}
		}
	}

	return HeatMapData{
		MinTimeSec:    int64(minT / 1_000_000_000),
		NumSeconds:    numSeconds,
		NumSubBuckets: numSubBuckets,
		SubBucketMs:   10,
		Cells:         cells,
		MaxValue:      slices.Max(cells),
	}
}
