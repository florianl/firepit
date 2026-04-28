package profiler

import (
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

	// First pass: find min/max TimeUnixNano
	var minT, maxT uint64
	hasValidTime := false

	for _, entry := range entries {
		if entry.Profile == nil {
			continue
		}

		// Check Profile.TimeUnixNano
		if entry.Profile.TimeUnixNano > 0 {
			if !hasValidTime {
				minT = entry.Profile.TimeUnixNano
				maxT = entry.Profile.TimeUnixNano
				hasValidTime = true
			} else {
				if entry.Profile.TimeUnixNano < minT {
					minT = entry.Profile.TimeUnixNano
				}
				if entry.Profile.TimeUnixNano > maxT {
					maxT = entry.Profile.TimeUnixNano
				}
			}
		}

		// Check Sample.TimestampsUnixNano
		for _, sample := range entry.Profile.Samples {
			if sample == nil {
				continue
			}
			for _, ts := range sample.TimestampsUnixNano {
				if !hasValidTime {
					minT = ts
					maxT = ts
					hasValidTime = true
				} else {
					if ts < minT {
						minT = ts
					}
					if ts > maxT {
						maxT = ts
					}
				}
			}
		}
	}

	if !hasValidTime {
		return HeatMapData{}
	}

	// Calculate number of seconds
	numSeconds := int((maxT-minT)/1_000_000_000) + 1
	if numSeconds > 3600 {
		numSeconds = 3600
	}

	// Allocate cells array
	cells := make([]int64, numSeconds*numSubBuckets)

	// Second pass: fill cells
	for _, entry := range entries {
		if entry.Profile == nil {
			continue
		}

		for _, sample := range entry.Profile.Samples {
			if sample == nil {
				continue
			}

			// Scenario A: has TimestampsUnixNano
			if len(sample.TimestampsUnixNano) > 0 {
				for _, ts := range sample.TimestampsUnixNano {
					xIdx := int((ts - minT) / 1_000_000_000)
					yIdx := int((ts % 1_000_000_000) / subBucketNanos)

					// Clamp to valid range
					if xIdx < 0 {
						xIdx = 0
					}
					if xIdx >= numSeconds {
						xIdx = numSeconds - 1
					}
					if yIdx < 0 {
						yIdx = 0
					}
					if yIdx >= numSubBuckets {
						yIdx = numSubBuckets - 1
					}

					cells[xIdx*numSubBuckets+yIdx]++
				}
			} else if len(sample.Values) > 0 && sample.Values[0] > 0 {
				// Scenario B: only Values, use Profile.TimeUnixNano
				if entry.Profile.TimeUnixNano > 0 {
					ts := entry.Profile.TimeUnixNano
					xIdx := int((ts - minT) / 1_000_000_000)
					yIdx := int((ts % 1_000_000_000) / subBucketNanos)

					// Clamp to valid range
					if xIdx < 0 {
						xIdx = 0
					}
					if xIdx >= numSeconds {
						xIdx = numSeconds - 1
					}
					if yIdx < 0 {
						yIdx = 0
					}
					if yIdx >= numSubBuckets {
						yIdx = numSubBuckets - 1
					}

					cells[xIdx*numSubBuckets+yIdx] += sample.Values[0]
				}
			}
		}
	}

	// Find max value
	var maxValue int64
	for _, v := range cells {
		if v > maxValue {
			maxValue = v
		}
	}

	return HeatMapData{
		MinTimeSec:    int64(minT / 1_000_000_000),
		NumSeconds:    numSeconds,
		NumSubBuckets: numSubBuckets,
		SubBucketMs:   10,
		Cells:         cells,
		MaxValue:      maxValue,
	}
}
