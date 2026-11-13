package profiler

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/florianl/firepit/internal/store"
)

const (
	// maxViews limits the number of flame graphs an Aggregator maintains.
	maxViews = 64
	// viewIdleTimeout is how long an unused flame graph is maintained.
	viewIdleTimeout = 2 * time.Minute
)

// Source provides the profiles to aggregate. It is implemented by *store.Store.
type Source interface {
	// ProfileEntries returns the profiles of a sample type sorted by ID.
	ProfileEntries(sampleType string) []store.ProfileEntry
	// Changed returns a channel that is closed on the next change.
	Changed() <-chan struct{}
}

// Aggregator maintains flame graphs incrementally as profiles arrive and expire.
// Updates run in the background, so neither requests nor ingestion wait for them.
type Aggregator struct {
	src         Source
	idleTimeout time.Duration
	now         func() time.Time

	mu    sync.Mutex
	views map[string]*view

	done     chan struct{}
	stopOnce sync.Once
}

// view is a flame graph that is kept in sync with the source.
type view struct {
	sampleType string
	filters    []string // normalized resource filters
	opts       Options

	mu   sync.Mutex
	root *FlameNode
	// included holds the entries aggregated into root, sorted by ID.
	included []store.ProfileEntry
	// lastID is the highest entry ID seen, whether it matched the filters or not.
	lastID uint64
	// encoded caches the JSON of root, nil if stale.
	encoded []byte

	// lastUsed is guarded by Aggregator.mu.
	lastUsed time.Time
}

// NewAggregator returns an Aggregator for src. Call Close to release it.
func NewAggregator(src Source) *Aggregator {
	a := &Aggregator{
		src:         src,
		idleTimeout: viewIdleTimeout,
		now:         time.Now,
		views:       make(map[string]*view),
		done:        make(chan struct{}),
	}
	// Subscribe before returning to not miss changes.
	go a.run(src.Changed())
	return a
}

// Close stops maintaining flame graphs.
func (a *Aggregator) Close() {
	a.stopOnce.Do(func() { close(a.done) })
}

// run updates the flame graphs on each change and drops idle ones.
func (a *Aggregator) run(changed <-chan struct{}) {
	ticker := time.NewTicker(a.idleTimeout / 4)
	defer ticker.Stop()

	for {
		select {
		case <-changed:
			// Subscribe before reading to not miss concurrent changes.
			changed = a.src.Changed()
			a.syncAll()
		case <-ticker.C:
			a.dropIdle()
		case <-a.done:
			return
		}
	}
}

func (a *Aggregator) syncAll() {
	a.mu.Lock()
	views := make([]*view, 0, len(a.views))
	for _, v := range a.views {
		views = append(views, v)
	}
	a.mu.Unlock()

	for _, v := range views {
		v.mu.Lock()
		v.sync(a.src)
		v.mu.Unlock()
	}
}

// dropIdle drops flame graphs not used within the idle timeout.
func (a *Aggregator) dropIdle() {
	a.mu.Lock()
	defer a.mu.Unlock()

	deadline := a.now().Add(-a.idleTimeout)
	for key, v := range a.views {
		if v.lastUsed.Before(deadline) {
			delete(a.views, key)
		}
	}
}

// view returns the flame graph for the given parameters, creating it if needed.
func (a *Aggregator) view(sampleType string, filters []string, opts Options) *view {
	filters = normalizeFilters(filters)
	key := viewKey(sampleType, filters, opts)

	a.mu.Lock()
	defer a.mu.Unlock()

	v, ok := a.views[key]
	if !ok {
		if len(a.views) >= maxViews {
			a.dropLeastRecentlyUsed()
		}
		v = &view{
			sampleType: sampleType,
			filters:    filters,
			opts:       Options{VirtualFrameKeys: append([]string(nil), opts.VirtualFrameKeys...)},
			root:       newFlameRoot(),
		}
		a.views[key] = v
	}
	v.lastUsed = a.now()
	return v
}

// viewKey returns the key of a view. filters must be normalized.
func viewKey(sampleType string, filters []string, opts Options) string {
	return sampleType + "\x01" + strings.Join(filters, "\x00") + "\x01" +
		strings.Join(opts.VirtualFrameKeys, "\x00")
}

// normalizeFilters drops invalid filters and sorts and deduplicates the rest.
func normalizeFilters(filters []string) []string {
	filters = slices.DeleteFunc(slices.Clone(filters), func(f string) bool {
		return !strings.Contains(f, ":")
	})
	slices.Sort(filters)
	return slices.Compact(filters)
}

// dropLeastRecentlyUsed drops the least recently used view. a.mu must be held.
func (a *Aggregator) dropLeastRecentlyUsed() {
	var oldestKey string
	var oldest *view
	for key, v := range a.views {
		if oldest == nil || v.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest = key, v
		}
	}
	delete(a.views, oldestKey)
}

// WithFlamegraph calls fn with the flame graph of sampleType matching filters.
// It equals ToFlamegraph up to the order of children. root is only valid
// during the call and must not be modified.
func (a *Aggregator) WithFlamegraph(sampleType string, filters []string, opts Options, fn func(root *FlameNode)) {
	v := a.view(sampleType, filters, opts)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sync(a.src)
	fn(v.root)
}

// FlamegraphJSON returns the cached JSON of the flame graph. The returned
// bytes must not be modified.
func (a *Aggregator) FlamegraphJSON(sampleType string, filters []string, opts Options) (json.RawMessage, error) {
	v := a.view(sampleType, filters, opts)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sync(a.src)
	if v.encoded == nil {
		encoded, err := json.Marshal(v.root)
		if err != nil {
			return nil, err
		}
		v.encoded = encoded
	}
	return v.encoded, nil
}

func newFlameRoot() *FlameNode {
	return &FlameNode{Name: "root", Children: []*FlameNode{}}
}

// sync removes the samples of entries that are gone and adds new ones.
// v.mu must be held.
func (v *view) sync(src Source) {
	current := src.ProfileEntries(v.sampleType)
	resolver := stackResolverPool.Get().(*stackResolver)
	defer func() {
		resolver.release()
		stackResolverPool.Put(resolver)
	}()

	// Both are sorted by ID. Missing entries expired or were evicted.
	kept := v.included[:0]
	removed := false
	i := 0
	for _, entry := range v.included {
		for i < len(current) && current[i].ID < entry.ID {
			i++
		}
		if i < len(current) && current[i].ID == entry.ID {
			kept = append(kept, entry)
			continue
		}
		applyEntry(v.root, entry, v.opts, resolver, -1)
		removed = true
	}
	// Release the removed profiles.
	clear(v.included[len(kept):])
	v.included = kept

	if removed {
		if len(v.included) == 0 {
			// Cheaper than pruning the whole tree.
			v.root = newFlameRoot()
		} else {
			pruneFlameNode(v.root)
		}
	}

	added := false
	for _, entry := range current {
		if entry.ID <= v.lastID {
			continue
		}
		v.lastID = entry.ID
		if !store.MatchResourceFilters(entry.Dictionary.GetStringTable(), entry.ResourceAttributes, v.filters) {
			continue
		}
		applyEntry(v.root, entry, v.opts, resolver, 1)
		v.included = append(v.included, entry)
		added = true
	}

	if removed || added {
		v.encoded = nil
	}
}
