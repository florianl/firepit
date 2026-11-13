// Package store provides a thread-safe rolling buffer for OTel profiles grouped by sample type with automatic TTL-based cleanup.
package store

import (
	"cmp"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unique"
	"unsafe"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

type ProfileEntry struct {
	CreatedAt          time.Time
	Profile            *profilespb.Profile
	Dictionary         *profilespb.ProfilesDictionary
	ResourceAttributes []*commonpb.KeyValue
	// SampleAttributeKeys holds the sorted keys of attributes referenced by
	// the samples of Profile.
	SampleAttributeKeys []string
	Size                int64
	// interned keeps the strings of Dictionary interned while the entry is stored.
	interned []unique.Handle[string]
}

// internStrings replaces the strings of table with interned copies and returns
// their handles. Dictionaries of different requests largely repeat the same
// strings, so this keeps a single copy of each.
func internStrings(table []string) []unique.Handle[string] {
	handles := make([]unique.Handle[string], len(table))
	for i, s := range table {
		handles[i] = unique.Make(s)
		table[i] = handles[i].Value()
	}
	return handles
}

// stringTableLookup safely looks up a string in the string table by index.
// Returns empty string if the index is out of bounds or negative.
func stringTableLookup(st []string, idx int32) string {
	// Validate bounds: index must be non-negative and within string table range
	if idx >= 0 && idx < int32(len(st)) {
		return st[idx]
	}
	return ""
}

// ResolveKeyValue returns the key and string value of kv. References into the
// string table (key_strindex, string_value_strindex) take precedence over the
// inline key and string_value.
func ResolveKeyValue(st []string, kv *commonpb.KeyValue) (key, value string) {
	return ResolveKey(st, kv), ResolveStringValue(st, kv.GetValue())
}

// ResolveKey returns the key of kv. A reference into the string table
// (key_strindex) takes precedence over the inline key.
func ResolveKey(st []string, kv *commonpb.KeyValue) string {
	if idx := kv.GetKeyStrindex(); idx != 0 {
		return stringTableLookup(st, idx)
	}
	return kv.GetKey()
}

// ResolveStringValue returns the string value of v, resolving
// string_value_strindex against the string table if set.
func ResolveStringValue(st []string, v *commonpb.AnyValue) string {
	if ref, ok := v.GetValue().(*commonpb.AnyValue_StringValueStrindex); ok {
		return stringTableLookup(st, ref.StringValueStrindex)
	}
	return v.GetStringValue()
}

// ResolveValue returns the scalar value v as string, or "" if v is not a scalar.
func ResolveValue(st []string, v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_StringValueStrindex:
		return ResolveStringValue(st, v)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	}
	return ""
}

// ResourceAttribute is a scalar resource attribute usable as filter.
type ResourceAttribute struct {
	Key   string
	Value string
}

// String returns the filter form "key:value".
func (a ResourceAttribute) String() string {
	return a.Key + ":" + a.Value
}

func compareResourceAttributes(a, b ResourceAttribute) int {
	return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Value, b.Value))
}

// collectResourceAttributes adds the scalar attributes in attrs to seen.
func collectResourceAttributes(seen map[ResourceAttribute]bool, st []string, attrs []*commonpb.KeyValue) {
	for _, attr := range attrs {
		if attr.GetValue() == nil {
			continue
		}
		if key, value := ResolveKey(st, attr), ResolveValue(st, attr.Value); key != "" && value != "" {
			seen[ResourceAttribute{Key: key, Value: value}] = true
		}
	}
}

// MatchResourceFilters reports whether attrs match all "key:value" filters.
// Filters without a colon are ignored.
func MatchResourceFilters(st []string, attrs []*commonpb.KeyValue, filters []string) bool {
	for _, filter := range filters {
		if !strings.Contains(filter, ":") {
			continue
		}
		matched := false
		for _, attr := range attrs {
			if attr.GetValue() == nil {
				continue
			}
			key := ResolveKey(st, attr)
			// Keys and values may contain colons, so match by prefix
			// instead of splitting the filter.
			if rest, ok := strings.CutPrefix(filter, key); !ok || !strings.HasPrefix(rest, ":") {
				continue
			}
			if value := ResolveValue(st, attr.Value); value != "" && filter[len(key)+1:] == value {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// collectAttributeKeys adds the keys of all resource attributes in attrs to seen.
func collectAttributeKeys(seen map[string]bool, st []string, attrs []*commonpb.KeyValue) {
	for _, attr := range attrs {
		if key := ResolveKey(st, attr); key != "" {
			seen[key] = true
		}
	}
}

// sampleAttributeKeys returns the sorted keys of all attributes referenced by
// the samples of profile.
func sampleAttributeKeys(profile *profilespb.Profile, dict *profilespb.ProfilesDictionary) []string {
	seenIdx := make(map[int32]bool)
	seen := make(map[string]bool)
	for _, sample := range profile.Samples {
		for _, idx := range sample.GetAttributeIndices() {
			if seenIdx[idx] || idx < 0 || int(idx) >= len(dict.AttributeTable) {
				continue
			}
			seenIdx[idx] = true
			if key := stringTableLookup(dict.StringTable, dict.AttributeTable[idx].GetKeyStrindex()); key != "" {
				seen[key] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

type Store struct {
	mu              sync.RWMutex
	entries         map[string][]ProfileEntry // sample type → profile entries
	resourceAttrs   []ResourceAttribute
	attributeKeys   []string
	maxAge          time.Duration
	cleanupInterval time.Duration
	maxMemory       int64 // 0 = unlimited
	totalBytes      int64
	generation      atomic.Uint64
	changed         chan struct{} // closed and replaced on every change
	done            chan struct{}

	// Counters for Metrics, guarded by mu except droppedRequests.
	addedProfiles   uint64
	expiredProfiles uint64
	evictedProfiles uint64
	droppedRequests atomic.Uint64
}

// Metrics is a snapshot of the store state and counters.
type Metrics struct {
	Profiles        int
	SampleTypes     int
	Bytes           int64
	MaxBytes        int64 // 0 if unlimited
	AddedProfiles   uint64
	ExpiredProfiles uint64 // removed after the retention time
	EvictedProfiles uint64 // removed to meet the memory limit
	DroppedRequests uint64 // requests without dictionary
}

// Metrics returns the current metrics of the store.
func (s *Store) Metrics() Metrics {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m := Metrics{
		SampleTypes:     len(s.entries),
		Bytes:           s.totalBytes,
		MaxBytes:        s.maxMemory,
		AddedProfiles:   s.addedProfiles,
		ExpiredProfiles: s.expiredProfiles,
		EvictedProfiles: s.evictedProfiles,
		DroppedRequests: s.droppedRequests.Load(),
	}
	for _, entries := range s.entries {
		m.Profiles += len(entries)
	}
	return m
}

// New creates a new Store with the specified max age, cleanup interval, and max memory.
// maxMemory of 0 means unlimited.
func New(maxAge, cleanupInterval time.Duration, maxMemory int64) *Store {
	s := &Store{
		entries:         make(map[string][]ProfileEntry),
		maxAge:          maxAge,
		cleanupInterval: cleanupInterval,
		maxMemory:       maxMemory,
		changed:         make(chan struct{}),
		done:            make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// Add stores profiles with their associated dictionary. dictionary must not be nil;
// if nil, the profiles are discarded and not stored. The store takes ownership
// of the profiles and the dictionary, and interns the dictionary strings.
func (s *Store) Add(resourceProfiles []*profilespb.ResourceProfiles, dictionary *profilespb.ProfilesDictionary) {
	if dictionary == nil {
		s.droppedRequests.Add(1)
		return
	}

	interned := internStrings(dictionary.StringTable)

	// Prepare entries without holding the lock.
	type newEntry struct {
		key   string
		entry ProfileEntry
	}
	var newEntries []newEntry

	st := dictionary.StringTable
	seenResourceAttrs := make(map[ResourceAttribute]bool)
	seenAttributeKeys := make(map[string]bool)

	// Shared data is accounted in equal shares to the profiles using it.
	numProfiles := 0
	for _, rp := range resourceProfiles {
		numProfiles += countProfiles(rp)
	}
	var dictShare int64
	if numProfiles > 0 {
		dictShare = dictionarySize(dictionary) / int64(numProfiles)
	}

	for _, rp := range resourceProfiles {
		if rp == nil {
			continue
		}

		var resAttrs []*commonpb.KeyValue
		var resShare int64
		if n := countProfiles(rp); n > 0 {
			resShare = resourceSize(rp.Resource) / int64(n)
		}
		if rp.Resource != nil {
			resAttrs = rp.Resource.Attributes
		}
		stored := false

		for _, sp := range rp.ScopeProfiles {
			if sp == nil {
				continue
			}
			for _, profile := range sp.Profiles {
				if profile == nil {
					continue
				}

				// Resolve sample type and unit
				typeStr := "unknown"
				unitStr := ""
				if profile.SampleType != nil {
					typeStr = stringTableLookup(st, profile.SampleType.TypeStrindex)
					if typeStr == "" {
						typeStr = "unknown"
					}
					unitStr = stringTableLookup(st, profile.SampleType.UnitStrindex)
				}

				// Format key as "type(unit)"
				key := typeStr
				if unitStr != "" {
					key = typeStr + "(" + unitStr + ")"
				}

				sampleKeys := sampleAttributeKeys(profile, dictionary)
				for _, k := range sampleKeys {
					seenAttributeKeys[k] = true
				}

				entry := ProfileEntry{
					CreatedAt:           time.Unix(0, int64(profile.TimeUnixNano)),
					Profile:             profile,
					Dictionary:          dictionary,
					ResourceAttributes:  resAttrs,
					SampleAttributeKeys: sampleKeys,
					interned:            interned,
				}
				entry.Size = int64(unsafe.Sizeof(entry)) + stringsSize(sampleKeys) +
					profileSize(profile) + dictShare + resShare
				newEntries = append(newEntries, newEntry{key, entry})
				stored = true
			}
		}

		if stored {
			// Cache resource attributes for the filtering
			collectResourceAttributes(seenResourceAttrs, st, resAttrs)
			collectAttributeKeys(seenAttributeKeys, st, resAttrs)
		}
	}

	if len(newEntries) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range newEntries {
		s.entries[e.key] = append(s.entries[e.key], e.entry)
		s.totalBytes += e.entry.Size
		s.addedProfiles++
	}
	s.resourceAttrs = mergeSorted(s.resourceAttrs, seenResourceAttrs, compareResourceAttributes)
	s.attributeKeys = mergeSorted(s.attributeKeys, seenAttributeKeys, strings.Compare)
	s.generation.Add(1)
	s.notifyChanged()
}

// Generation returns a counter that changes whenever profiles are added or removed.
func (s *Store) Generation() uint64 {
	return s.generation.Load()
}

// Changed returns a channel that is closed on the next change of the stored
// profiles. Get it before reading the store to not miss a change.
func (s *Store) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

// notifyChanged wakes up waiters on Changed. s.mu must be held.
func (s *Store) notifyChanged() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// mergeSorted returns the sorted union of sorted and seen without modifying sorted.
func mergeSorted[T comparable](sorted []T, seen map[T]bool, compare func(a, b T) int) []T {
	var missing []T
	for k := range seen {
		if _, found := slices.BinarySearchFunc(sorted, k, compare); !found {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return sorted
	}
	merged := make([]T, 0, len(sorted)+len(missing))
	merged = append(append(merged, sorted...), missing...)
	slices.SortFunc(merged, compare)
	return merged
}

// countProfiles returns the number of non-nil profiles in rp.
func countProfiles(rp *profilespb.ResourceProfiles) int {
	n := 0
	for _, sp := range rp.GetScopeProfiles() {
		for _, profile := range sp.GetProfiles() {
			if profile != nil {
				n++
			}
		}
	}
	return n
}

func (s *Store) SampleTypes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return slices.Sorted(maps.Keys(s.entries))
}

func (s *Store) ProfileEntries(sampleType string) []ProfileEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := s.entries[sampleType]
	result := make([]ProfileEntry, len(entries))
	copy(result, entries)
	return result
}

// ResourceTypes returns the resource attributes in filter form "key:value".
func (s *Store) ResourceTypes() []string {
	attrs := s.ResourceAttributes()
	if len(attrs) == 0 {
		return nil
	}
	types := make([]string, len(attrs))
	for i, attr := range attrs {
		types[i] = attr.String()
	}
	// Distinct attributes may collide, e.g. a:b:c
	slices.Sort(types)
	return slices.Compact(types)
}

// ResourceAttributes returns the sorted resource attributes usable as filter.
// The returned slice must not be modified.
func (s *Store) ResourceAttributes() []ResourceAttribute {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.resourceAttrs
}

// AttributeKeys returns the sorted keys of all resource and sample attributes
// of the stored profiles.
func (s *Store) AttributeKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.attributeKeys
}

func (s *Store) Stats() (count int, minTime, maxTime time.Time, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, entries := range s.entries {
		count += len(entries)
		for _, e := range entries {
			if !ok || e.CreatedAt.Before(minTime) {
				minTime = e.CreatedAt
			}
			if !ok || e.CreatedAt.After(maxTime) {
				maxTime = e.CreatedAt
			}
			ok = true
		}
	}
	return count, minTime, maxTime, ok
}

func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanup()
		case <-s.done:
			return
		}
	}
}

func (s *Store) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	if s.maxAge > 0 {
		now := time.Now()
		for typeStr, entries := range s.entries {
			// Readers get copies, so filter in place.
			kept := entries[:0]
			for _, entry := range entries {
				if now.Sub(entry.CreatedAt) < s.maxAge {
					kept = append(kept, entry)
				} else {
					s.totalBytes -= entry.Size
					s.expiredProfiles++
				}
			}
			if len(kept) == len(entries) {
				continue
			}
			removed += len(entries) - len(kept)
			// Release removed profiles.
			clear(entries[len(kept):])
			if len(kept) > 0 {
				s.entries[typeStr] = kept
			} else {
				delete(s.entries, typeStr)
			}
		}
	}

	if s.maxMemory > 0 && s.totalBytes > s.maxMemory {
		removed += s.evictOldest()
	}

	if removed == 0 {
		// Add keeps the filter attributes up to date.
		return
	}
	s.generation.Add(1)
	s.notifyChanged()

	// Update ResourceAttributes for filtering
	seenResourceAttrs := make(map[ResourceAttribute]bool)
	seenAttributeKeys := make(map[string]bool)
	for st := range s.entries {
		for _, entry := range s.entries[st] {
			strTable := entry.Dictionary.GetStringTable()
			collectResourceAttributes(seenResourceAttrs, strTable, entry.ResourceAttributes)
			collectAttributeKeys(seenAttributeKeys, strTable, entry.ResourceAttributes)
			for _, k := range entry.SampleAttributeKeys {
				seenAttributeKeys[k] = true
			}
		}
	}
	s.resourceAttrs = slices.SortedFunc(maps.Keys(seenResourceAttrs), compareResourceAttributes)
	s.attributeKeys = slices.Sorted(maps.Keys(seenAttributeKeys))
}

// evictOldest removes the oldest profiles to meet the memory limit and returns their count.
func (s *Store) evictOldest() int {
	type ref struct {
		typeStr   string
		idx       int
		createdAt time.Time
		size      int64
	}
	var all []ref
	for typeStr, entries := range s.entries {
		for i, e := range entries {
			all = append(all, ref{typeStr, i, e.CreatedAt, e.Size})
		}
	}
	slices.SortFunc(all, func(a, b ref) int {
		return a.createdAt.Compare(b.createdAt)
	})

	// Mark first, then compact each sample type in one pass.
	evict := make(map[string][]bool)
	dropped := 0
	for _, item := range all {
		if s.totalBytes <= s.maxMemory {
			break
		}
		marks := evict[item.typeStr]
		if marks == nil {
			marks = make([]bool, len(s.entries[item.typeStr]))
			evict[item.typeStr] = marks
		}
		marks[item.idx] = true
		s.totalBytes -= item.size
		dropped++
	}
	s.evictedProfiles += uint64(dropped)

	for typeStr, marks := range evict {
		entries := s.entries[typeStr]
		kept := entries[:0]
		for i, e := range entries {
			if !marks[i] {
				kept = append(kept, e)
			}
		}
		clear(entries[len(kept):])
		if len(kept) > 0 {
			s.entries[typeStr] = kept
		} else {
			delete(s.entries, typeStr)
		}
	}

	if dropped > 0 {
		slog.Warn("Evicted profiles due to memory limit", "dropped", dropped, "max_memory", s.maxMemory, "current_bytes", s.totalBytes)
	}
	return dropped
}

func (s *Store) Close() {
	close(s.done)
}
