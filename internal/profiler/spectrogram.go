package profiler

import (
	"cmp"
	"slices"

	"github.com/florianl/firepit/internal/store"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

// maxSpectrogramBuckets caps the number of time buckets. Longer ranges get wider buckets.
const maxSpectrogramBuckets = 600

const nanosPerSecond = uint64(1_000_000_000)

// SpectrogramRow is the sample count per time bucket of one attribute value.
type SpectrogramRow struct {
	Value  string  `json:"value"`
	Total  int64   `json:"total"`
	Counts []int64 `json:"counts"`
}

// SpectrogramCategory groups the rows of one attribute key.
type SpectrogramCategory struct {
	Key  string            `json:"key"`
	Rows []*SpectrogramRow `json:"rows"`
}

// SpectrogramData is the sample count over time per filterable attribute value.
type SpectrogramData struct {
	MinTimeSec int64                  `json:"minTimeSec"`
	BucketSec  int                    `json:"bucketSec"`
	NumBuckets int                    `json:"numBuckets"`
	Categories []*SpectrogramCategory `json:"categories"`
	MaxValue   int64                  `json:"maxValue"`
}

type NamedSpectrogram struct {
	Type string          `json:"type"`
	Data SpectrogramData `json:"data"`
}

// forEachSampleTime calls fn for each sample in profile. Samples without timestamps use the profile time.
func forEachSampleTime(profile *profilespb.Profile, fn func(ts uint64, n int64)) {
	for _, sample := range profile.GetSamples() {
		if sample == nil {
			continue
		}
		if len(sample.TimestampsUnixNano) > 0 {
			for _, ts := range sample.TimestampsUnixNano {
				fn(ts, 1)
			}
		} else if len(sample.Values) > 0 && sample.Values[0] > 0 && profile.TimeUnixNano > 0 {
			fn(profile.TimeUnixNano, sample.Values[0])
		}
	}
}

// ToSpectrogram counts the samples of entries over time per filterable attribute value.
func ToSpectrogram(entries []store.ProfileEntry) SpectrogramData {
	empty := SpectrogramData{Categories: []*SpectrogramCategory{}}

	var minT, maxT uint64
	hasValidTime := false
	for _, entry := range entries {
		forEachSampleTime(entry.Profile, func(ts uint64, _ int64) {
			if !hasValidTime {
				minT, maxT = ts, ts
				hasValidTime = true
				return
			}
			minT = min(minT, ts)
			maxT = max(maxT, ts)
		})
	}
	if !hasValidTime {
		return empty
	}

	minSec := minT / nanosPerSecond
	spanSec := maxT/nanosPerSecond - minSec + 1
	bucketSec := (spanSec + maxSpectrogramBuckets - 1) / maxSpectrogramBuckets
	numBuckets := int((spanSec + bucketSec - 1) / bucketSec)

	type rowKey struct{ key, value string }
	rows := make(map[rowKey]*SpectrogramRow)
	var entryRows []*SpectrogramRow
	for _, entry := range entries {
		entryRows = entryRows[:0]
		st := entry.Dictionary.GetStringTable()
		for _, attr := range entry.ResourceAttributes {
			if attr.GetValue() == nil {
				continue
			}
			key, value := store.ResolveKey(st, attr), store.ResolveValue(st, attr.GetValue())
			if key == "" || value == "" {
				continue
			}
			rk := rowKey{key, value}
			row, ok := rows[rk]
			if !ok {
				row = &SpectrogramRow{Value: value, Counts: make([]int64, numBuckets)}
				rows[rk] = row
			}
			if !slices.Contains(entryRows, row) {
				entryRows = append(entryRows, row)
			}
		}
		if len(entryRows) == 0 {
			continue
		}

		forEachSampleTime(entry.Profile, func(ts uint64, n int64) {
			idx := int((ts/nanosPerSecond - minSec) / bucketSec)
			for _, row := range entryRows {
				row.Counts[idx] += n
				row.Total += n
			}
		})
	}

	categories := make(map[string]*SpectrogramCategory)
	var maxValue int64
	for rk, row := range rows {
		cat, ok := categories[rk.key]
		if !ok {
			cat = &SpectrogramCategory{Key: rk.key}
			categories[rk.key] = cat
		}
		cat.Rows = append(cat.Rows, row)
		maxValue = max(maxValue, slices.Max(row.Counts))
	}

	result := make([]*SpectrogramCategory, 0, len(categories))
	for _, cat := range categories {
		slices.SortFunc(cat.Rows, func(a, b *SpectrogramRow) int {
			return cmp.Compare(a.Value, b.Value)
		})
		result = append(result, cat)
	}
	slices.SortFunc(result, func(a, b *SpectrogramCategory) int {
		return cmp.Compare(a.Key, b.Key)
	})

	return SpectrogramData{
		MinTimeSec: int64(minSec),
		BucketSec:  int(bucketSec),
		NumBuckets: numBuckets,
		Categories: result,
		MaxValue:   maxValue,
	}
}
