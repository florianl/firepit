// Package store provides a thread-safe rolling buffer for OTel profiles grouped by sample type with automatic TTL-based cleanup.
package store

import (
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/protobuf/proto"
)

type ProfileEntry struct {
	CreatedAt  time.Time
	Profile    *profilespb.Profile
	Dictionary *profilespb.ProfilesDictionary
	Attributes []*commonpb.KeyValue
	Size       int64
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

type Store struct {
	mu              sync.RWMutex
	entries         map[string][]ProfileEntry // sample type → profile entries
	resourceTypes   []string
	maxAge          time.Duration
	cleanupInterval time.Duration
	maxMemory       int64 // 0 = unlimited
	totalBytes      int64
	done            chan struct{}
}

// New creates a new Store with the specified max age, cleanup interval, and max memory.
// maxMemory of 0 means unlimited.
func New(maxAge, cleanupInterval time.Duration, maxMemory int64) *Store {
	s := &Store{
		entries:         make(map[string][]ProfileEntry),
		maxAge:          maxAge,
		cleanupInterval: cleanupInterval,
		maxMemory:       maxMemory,
		done:            make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// Add stores profiles with their associated dictionary. dictionary must not be nil;
// if nil, the profiles are discarded and not stored.
func (s *Store) Add(resourceProfiles []*profilespb.ResourceProfiles, dictionary *profilespb.ProfilesDictionary) {
	if dictionary == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st := dictionary.StringTable
	seenResourceTypes := make(map[string]bool)

	for _, rp := range resourceProfiles {
		if rp == nil {
			continue
		}

		// Extract resource attributes
		var attributes []*commonpb.KeyValue
		if rp.Resource != nil {
			attributes = rp.Resource.Attributes

			// Cache resource attributes for the filtering
			for _, attr := range attributes {
				if attr.Value != nil {
					if strVal := attr.Value.GetStringValue(); strVal != "" {
						seenResourceTypes[attr.Key+":"+strVal] = true
					}
				}
			}
		}

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

				entry := ProfileEntry{
					CreatedAt:  time.Unix(0, int64(profile.TimeUnixNano)),
					Profile:    profile,
					Dictionary: dictionary,
					Attributes: attributes,
					Size:       int64(proto.Size(profile)),
				}
				s.entries[key] = append(s.entries[key], entry)
				s.totalBytes += entry.Size
			}
		}
	}

	resAttrs := slices.Sorted(maps.Keys(seenResourceTypes))
	s.resourceTypes = append(s.resourceTypes, resAttrs...)
	slices.Sort(s.resourceTypes)
	s.resourceTypes = slices.Compact(s.resourceTypes)
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

func (s *Store) ResourceTypes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.resourceTypes
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
	now := time.Now()
	for typeStr, entries := range s.entries {
		var kept []ProfileEntry
		for _, entry := range entries {
			if now.Sub(entry.CreatedAt) < s.maxAge {
				kept = append(kept, entry)
			} else {
				s.totalBytes -= entry.Size
			}
		}
		if len(kept) > 0 {
			s.entries[typeStr] = kept
		} else {
			delete(s.entries, typeStr)
		}
	}

	if s.maxMemory > 0 && s.totalBytes > s.maxMemory {
		s.evictOldest()
	}
}

func (s *Store) evictOldest() {
	type indexed struct {
		typeStr string
		entry   ProfileEntry
	}
	var all []indexed
	for typeStr, entries := range s.entries {
		for _, e := range entries {
			all = append(all, indexed{typeStr, e})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].entry.CreatedAt.Before(all[j].entry.CreatedAt)
	})

	dropped := 0
	for _, item := range all {
		if s.totalBytes <= s.maxMemory {
			break
		}
		entries := s.entries[item.typeStr]
		for i, e := range entries {
			if e.CreatedAt.Equal(item.entry.CreatedAt) && e.Size == item.entry.Size {
				s.entries[item.typeStr] = append(entries[:i], entries[i+1:]...)
				break
			}
		}
		s.totalBytes -= item.entry.Size
		dropped++
	}

	for typeStr, entries := range s.entries {
		if len(entries) == 0 {
			delete(s.entries, typeStr)
		}
	}

	if dropped > 0 {
		slog.Warn("Evicted profiles due to memory limit", "dropped", dropped, "max_memory", s.maxMemory, "current_bytes", s.totalBytes)
	}
}

func (s *Store) Close() {
	close(s.done)
}
