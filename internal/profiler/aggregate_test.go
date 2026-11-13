package profiler

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/florianl/firepit/internal/store"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

// fakeSource is a Source whose entries are controlled by the test.
type fakeSource struct {
	mu      sync.Mutex
	entries map[string][]store.ProfileEntry
	lastID  uint64
	changed chan struct{}
	reads   int
}

func newFakeSource() *fakeSource {
	return &fakeSource{entries: make(map[string][]store.ProfileEntry), changed: make(chan struct{})}
}

func (s *fakeSource) ProfileEntries(sampleType string) []store.ProfileEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return slices.Clone(s.entries[sampleType])
}

func (s *fakeSource) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *fakeSource) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *fakeSource) add(sampleType string, entry store.ProfileEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastID++
	entry.ID = s.lastID
	s.entries[sampleType] = append(s.entries[sampleType], entry)
	s.notify()
}

// remove removes the i-th entry of a sample type.
func (s *fakeSource) remove(sampleType string, i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sampleType] = slices.Delete(s.entries[sampleType], i, i+1)
	s.notify()
}

func (s *fakeSource) len(sampleType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries[sampleType])
}

// randomEntry returns a profile of one of three services with overlapping stacks.
func randomEntry(rng *rand.Rand) store.ProfileEntry {
	dict := &profilespb.ProfilesDictionary{
		StringTable:   []string{"", "thread.name", "worker-a", "worker-b"},
		FunctionTable: []*profilespb.Function{{}},
		LocationTable: []*profilespb.Location{{}},
		StackTable:    []*profilespb.Stack{{}},
		AttributeTable: []*profilespb.KeyValueAndUnit{
			{},
			{KeyStrindex: 1, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 2}}},
			{KeyStrindex: 1, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 3}}},
		},
	}
	// Every dictionary lists the functions in its own order.
	for _, f := range rng.Perm(12) {
		dict.StringTable = append(dict.StringTable, fmt.Sprintf("func%d", f))
		dict.FunctionTable = append(dict.FunctionTable, &profilespb.Function{NameStrindex: int32(len(dict.StringTable) - 1)})
		dict.LocationTable = append(dict.LocationTable, &profilespb.Location{
			Lines: []*profilespb.Line{{FunctionIndex: int32(len(dict.FunctionTable) - 1)}},
		})
	}
	for range 6 {
		stack := &profilespb.Stack{}
		for range 1 + rng.IntN(5) {
			stack.LocationIndices = append(stack.LocationIndices, int32(1+rng.IntN(12)))
		}
		dict.StackTable = append(dict.StackTable, stack)
	}

	profile := &profilespb.Profile{}
	for range 1 + rng.IntN(8) {
		sample := &profilespb.Sample{StackIndex: int32(rng.IntN(8))} // may not resolve
		switch rng.IntN(4) {
		case 0:
			sample.TimestampsUnixNano = make([]uint64, 1+rng.IntN(3))
		case 1:
			// Negative values may cancel each other out.
			sample.Values = []int64{int64(rng.IntN(7)) - 3}
		default:
			sample.Values = []int64{int64(1 + rng.IntN(9))}
		}
		if rng.IntN(2) == 0 {
			sample.AttributeIndices = []int32{int32(1 + rng.IntN(2))}
		}
		profile.Samples = append(profile.Samples, sample)
	}

	return store.ProfileEntry{
		Profile:    profile,
		Dictionary: dict,
		ResourceAttributes: []*commonpb.KeyValue{{
			Key:   "service.name",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("svc-%d", rng.IntN(3))}},
		}},
	}
}

// canonical returns a representation of root independent of child order.
func canonical(node *FlameNode) string {
	children := make([]string, len(node.Children))
	for i, child := range node.Children {
		children[i] = canonical(child)
	}
	slices.Sort(children)
	return fmt.Sprintf("%s|%s|%s|%d{%s}", node.Name, node.Filename, node.FrameType, node.Value, strings.Join(children, ","))
}

// Incremental flame graphs must match ToFlamegraph for any arrive and expire sequence.
func TestAggregatorMatchesToFlamegraph(t *testing.T) {
	queries := []struct {
		filters []string
		opts    Options
	}{
		{nil, Options{}},
		{[]string{"service.name:svc-1"}, Options{}},
		{nil, Options{VirtualFrameKeys: []string{"service.name", "thread.name"}}},
		{[]string{"service.name:svc-2"}, Options{VirtualFrameKeys: []string{"thread.name"}}},
		{[]string{"service.name:none"}, Options{}},
		{[]string{"service.name:svc-1", "service.name:svc-2"}, Options{}},
	}

	for seed := range uint64(8) {
		rng := rand.New(rand.NewPCG(seed, 99))
		src := newFakeSource()
		agg := NewAggregator(src)

		for step := range 120 {
			// Randomly add one, expire one or expire all.
			switch n := src.len("cpu"); {
			case n > 0 && rng.IntN(25) == 0:
				for src.len("cpu") > 0 {
					src.remove("cpu", 0)
				}
			case n > 0 && rng.IntN(3) == 0:
				src.remove("cpu", rng.IntN(n))
			default:
				src.add("cpu", randomEntry(rng))
			}
			if rng.IntN(3) == 0 {
				// Several changes between two requests.
				continue
			}

			for _, q := range queries {
				want := ToFlamegraph(FilterByResource(src.ProfileEntries("cpu"), q.filters), q.opts)

				var got string
				agg.WithFlamegraph("cpu", q.filters, q.opts, func(root *FlameNode) { got = canonical(root) })
				if got != canonical(want) {
					t.Fatalf("seed %d, step %d, filters %q, virtual frames %v:\n got %s\nwant %s",
						seed, step, q.filters, q.opts.VirtualFrameKeys, got, canonical(want))
				}

				encoded, err := agg.FlamegraphJSON("cpu", q.filters, q.opts)
				if err != nil {
					t.Fatal(err)
				}
				var decoded FlameNode
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
				if canonical(&decoded) != canonical(want) {
					t.Fatalf("seed %d, step %d: JSON differs from flame graph", seed, step)
				}
			}
		}
		agg.Close()
	}
}

func TestAggregatorKeepsEncodingUntilChange(t *testing.T) {
	src := newFakeSource()
	agg := NewAggregator(src)
	defer agg.Close()
	rng := rand.New(rand.NewPCG(1, 2))
	src.add("cpu", randomEntry(rng))

	first, err := agg.FlamegraphJSON("cpu", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := agg.FlamegraphJSON("cpu", nil, Options{})
	if &first[0] != &second[0] {
		t.Error("unchanged flame graph was encoded again")
	}

	entry := randomEntry(rng)
	entry.Profile.Samples = append(entry.Profile.Samples, &profilespb.Sample{StackIndex: 1, Values: []int64{1000}})
	src.add("cpu", entry)
	third, _ := agg.FlamegraphJSON("cpu", nil, Options{})
	if string(third) == string(first) {
		t.Error("encoding did not change with the flame graph")
	}

	// Other sample types are independent and empty.
	other, _ := agg.FlamegraphJSON("alloc", nil, Options{})
	if got, want := string(other), `{"name":"root","value":0}`; got != want {
		t.Errorf("flame graph of unknown sample type = %s, want %s", got, want)
	}
}

// Flame graphs update on source changes and release removed profiles.
func TestAggregatorUpdatesInBackground(t *testing.T) {
	src := newFakeSource()
	agg := NewAggregator(src)
	defer agg.Close()
	rng := rand.New(rand.NewPCG(3, 4))

	src.add("cpu", randomEntry(rng))
	agg.WithFlamegraph("cpu", nil, Options{}, func(*FlameNode) {})
	v := agg.view("cpu", nil, Options{})

	state := func() (lastID uint64, included int) {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.lastID, len(v.included)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("flame graph was not updated in the background: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	src.add("cpu", randomEntry(rng))
	src.add("cpu", randomEntry(rng))
	waitFor("added profiles", func() bool { id, n := state(); return id == 3 && n == 3 })

	src.remove("cpu", 0)
	waitFor("removed profile", func() bool { _, n := state(); return n == 2 })
}

func TestAggregatorDropsUnusedFlamegraphs(t *testing.T) {
	src := newFakeSource()
	agg := NewAggregator(src)
	defer agg.Close()

	now := time.Now()
	agg.mu.Lock()
	agg.now = func() time.Time { return now }
	agg.mu.Unlock()
	count := func() int {
		agg.mu.Lock()
		defer agg.mu.Unlock()
		return len(agg.views)
	}

	// Beyond maxViews the least recently used is dropped.
	for i := range maxViews + 10 {
		now = now.Add(time.Second)
		agg.WithFlamegraph("cpu", []string{fmt.Sprintf("service.name:%d", i)}, Options{}, func(*FlameNode) {})
	}
	if got := count(); got != maxViews {
		t.Fatalf("%d flame graphs, want %d", got, maxViews)
	}
	agg.mu.Lock()
	_, oldest := agg.views[viewKey("cpu", []string{"service.name:0"}, Options{})]
	_, newest := agg.views[viewKey("cpu", []string{fmt.Sprintf("service.name:%d", maxViews+9)}, Options{})]
	agg.mu.Unlock()
	if oldest || !newest {
		t.Errorf("oldest flame graph kept: %v, newest kept: %v", oldest, newest)
	}

	// Idle flame graphs are dropped.
	now = now.Add(viewIdleTimeout - 5*time.Second)
	agg.WithFlamegraph("cpu", nil, Options{}, func(*FlameNode) {})
	now = now.Add(10 * time.Second)
	agg.dropIdle()
	if got := count(); got != 1 {
		t.Errorf("%d flame graphs after the idle timeout, want 1", got)
	}
}

// Virtual frame keys are part of the identity of a flame graph.
func TestAggregatorSeparatesOptions(t *testing.T) {
	src := newFakeSource()
	agg := NewAggregator(src)
	defer agg.Close()
	src.add("cpu", randomEntry(rand.New(rand.NewPCG(5, 6))))

	plain, _ := agg.FlamegraphJSON("cpu", nil, Options{})
	keys := []string{"service.name"}
	grouped, _ := agg.FlamegraphJSON("cpu", nil, Options{VirtualFrameKeys: keys})
	if string(plain) == string(grouped) || !strings.Contains(string(grouped), "service.name=svc-") {
		t.Errorf("virtual frames not applied: %s", grouped)
	}
	// The caller may reuse its slice of keys.
	keys[0] = "thread.name"
	again, _ := agg.FlamegraphJSON("cpu", nil, Options{VirtualFrameKeys: []string{"service.name"}})
	if string(again) != string(grouped) {
		t.Error("flame graph changed with the slice of keys of an earlier request")
	}
}
