package profiler

import (
	"slices"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"

	"github.com/florianl/firepit/internal/store"
)

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func findSpectrogramRow(t *testing.T, d SpectrogramData, key, value string) *SpectrogramRow {
	t.Helper()
	for _, cat := range d.Categories {
		if cat.Key != key {
			continue
		}
		for _, row := range cat.Rows {
			if row.Value == value {
				return row
			}
		}
	}
	t.Fatalf("row %s:%s not found", key, value)
	return nil
}

func TestToSpectrogramEmpty(t *testing.T) {
	d := ToSpectrogram(nil)
	if d.NumBuckets != 0 || d.Categories == nil || len(d.Categories) != 0 {
		t.Fatalf("Expected empty spectrogram, got %+v", d)
	}
}

func TestToSpectrogram(t *testing.T) {
	base := uint64(1_000_000_000_000)
	dict := &profilespb.ProfilesDictionary{}

	entries := []store.ProfileEntry{
		{
			// Each timestamp is one sample.
			Profile: &profilespb.Profile{
				TimeUnixNano: base,
				Samples: []*profilespb.Sample{
					{TimestampsUnixNano: []uint64{base + 100, base + 200, base + 2_500_000_000}},
				},
			},
			Dictionary: dict,
			ResourceAttributes: []*commonpb.KeyValue{
				strAttr("service.name", "a"),
				strAttr("host.name", "h1"),
			},
		},
		{
			// Samples without timestamps count at the profile time.
			Profile: &profilespb.Profile{
				TimeUnixNano: base + 1_000_000_000,
				Samples: []*profilespb.Sample{
					{Values: []int64{5}},
					{Values: []int64{0}},
				},
			},
			Dictionary: dict,
			ResourceAttributes: []*commonpb.KeyValue{
				strAttr("service.name", "b"),
				strAttr("host.name", "h1"),
				// Scalar values are filterable.
				{Key: "process.pid", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}},
				// Non-scalar values are not.
				{Key: "tags", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}}},
			},
		},
	}

	d := ToSpectrogram(entries)

	if d.MinTimeSec != 1000 || d.BucketSec != 1 || d.NumBuckets != 3 {
		t.Fatalf("Unexpected time range: %+v", d)
	}
	if d.MaxValue != 5 {
		t.Fatalf("Expected MaxValue=5, got %d", d.MaxValue)
	}

	var keys []string
	for _, cat := range d.Categories {
		keys = append(keys, cat.Key)
	}
	if !slices.Equal(keys, []string{"host.name", "process.pid", "service.name"}) {
		t.Fatalf("Unexpected categories: %v", keys)
	}

	for _, tc := range []struct {
		key, value string
		counts     []int64
		total      int64
	}{
		{"service.name", "a", []int64{2, 0, 1}, 3},
		{"service.name", "b", []int64{0, 5, 0}, 5},
		{"host.name", "h1", []int64{2, 5, 1}, 8},
		{"process.pid", "1", []int64{0, 5, 0}, 5},
	} {
		row := findSpectrogramRow(t, d, tc.key, tc.value)
		if !slices.Equal(row.Counts, tc.counts) || row.Total != tc.total {
			t.Errorf("%s:%s: got counts=%v total=%d, want counts=%v total=%d",
				tc.key, tc.value, row.Counts, row.Total, tc.counts, tc.total)
		}
	}
}

func TestToSpectrogramWidensBuckets(t *testing.T) {
	base := uint64(1_000_000_000_000)
	entries := []store.ProfileEntry{{
		Profile: &profilespb.Profile{
			TimeUnixNano: base,
			Samples: []*profilespb.Sample{
				// 1201s need 3s buckets.
				{TimestampsUnixNano: []uint64{base, base + 1200*nanosPerSecond}},
			},
		},
		Dictionary:         &profilespb.ProfilesDictionary{},
		ResourceAttributes: []*commonpb.KeyValue{strAttr("service.name", "a")},
	}}

	d := ToSpectrogram(entries)

	if d.BucketSec != 3 || d.NumBuckets != 401 {
		t.Fatalf("Expected 401 buckets of 3s, got %d buckets of %ds", d.NumBuckets, d.BucketSec)
	}
	row := findSpectrogramRow(t, d, "service.name", "a")
	if row.Counts[0] != 1 || row.Counts[400] != 1 || row.Total != 2 {
		t.Fatalf("Unexpected counts: first=%d last=%d total=%d", row.Counts[0], row.Counts[400], row.Total)
	}
}
