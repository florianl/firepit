// Package profiler converts OTel profile data to flame graph format.
package profiler

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/florianl/firepit/internal/store"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

type FlameNode struct {
	Name        string                `json:"name"`
	Value       int64                 `json:"value"`
	Children    []*FlameNode          `json:"children,omitempty"`
	childrenMap map[string]*FlameNode `json:"-`
}

type NamedFlamegraph struct {
	Type string     `json:"type"`
	Root *FlameNode `json:"root"`
}

// stackTableLookup safely looks up a stack entry by index.
// Returns nil if the index is out of bounds or negative.
func stackTableLookup(dict *profilespb.ProfilesDictionary, idx int32) *profilespb.Stack {
	if idx < 0 || int(idx) >= len(dict.StackTable) {
		return nil
	}
	return dict.StackTable[idx]
}

// locationTableLookup safely looks up a location by index.
// Returns nil if the index is out of bounds or negative.
func locationTableLookup(dict *profilespb.ProfilesDictionary, idx int32) *profilespb.Location {
	if idx < 0 || int(idx) >= len(dict.LocationTable) {
		return nil
	}
	return dict.LocationTable[idx]
}

// functionTableLookup safely looks up a function by index.
// Returns nil if the index is out of bounds or negative.
func functionTableLookup(dict *profilespb.ProfilesDictionary, idx int32) *profilespb.Function {
	if idx < 0 || int(idx) >= len(dict.FunctionTable) {
		return nil
	}
	return dict.FunctionTable[idx]
}

// stringTableLookup safely looks up a string by index.
// Returns empty string if the index is out of bounds or negative.
func stringTableLookup(dict *profilespb.ProfilesDictionary, idx int32) string {
	if idx < 0 || int(idx) >= len(dict.StringTable) {
		return ""
	}
	return dict.StringTable[idx]
}

func FilterByResourceType(entries []store.ProfileEntry, resourceType string) []store.ProfileEntry {
	if resourceType == "" {
		return entries
	}

	parts := strings.SplitN(resourceType, ":", 2)
	if len(parts) != 2 {
		return entries
	}
	key, value := parts[0], parts[1]

	var filtered []store.ProfileEntry
	for _, entry := range entries {
		if entry.Attributes == nil {
			continue
		}
		for _, attr := range entry.Attributes {
			if attr.Key == key && attr.Value != nil {
				if strVal := attr.Value.GetStringValue(); strVal == value {
					filtered = append(filtered, entry)
					break
				}
			}
		}
	}
	return filtered
}

func ToFlamegraph(entries []store.ProfileEntry) *FlameNode {
	root := &FlameNode{
		Name:        "root",
		Value:       0,
		Children:    []*FlameNode{},
		childrenMap: make(map[string]*FlameNode),
	}

	// Cache resolved stacks to avoid re-resolving identical stacks
	stackCache := make(map[int32][]string)

	profileCount := 0
	for _, entry := range entries {
		if entry.Profile != nil && entry.Dictionary != nil {
			profileCount++
			processProfile(root, entry.Profile, entry.Dictionary, stackCache)
		}
	}

	if profileCount > 0 && root.Value == 0 {
		slog.Warn("Processed profiles but got no data", "profile_count", profileCount)
	}

	return root
}

func processProfile(root *FlameNode, profile *profilespb.Profile, dict *profilespb.ProfilesDictionary, stackCache map[int32][]string) {
	if profile == nil || len(profile.Samples) == 0 {
		return
	}

	processedSamples := 0

	for _, sample := range profile.Samples {
		var value int64

		// Scenario 1: Timestamped samples without aggregated values
		// Each timestamp represents one sample occurrence
		if len(sample.Values) == 0 && len(sample.TimestampsUnixNano) > 0 {
			value = int64(len(sample.TimestampsUnixNano))
		} else if len(sample.Values) > 0 {
			// Scenario 2 & 3: Aggregated values (with or without timestamps)
			value = sample.Values[0]
			if value == 0 {
				continue
			}
		} else {
			// No value and no timestamps - skip
			continue
		}

		stack := resolveStack(sample, dict, stackCache)
		if len(stack) == 0 {
			continue
		}

		insertStack(root, stack, value)
		processedSamples++
	}
}

func resolveStack(sample *profilespb.Sample, dict *profilespb.ProfilesDictionary, stackCache map[int32][]string) []string {
	var stack []string

	if dict == nil {
		return stack
	}

	// Check if this stack is already resolved
	if cached, ok := stackCache[sample.StackIndex]; ok {
		return cached
	}

	// Get the stack from the dictionary using the stack_index
	stackEntry := stackTableLookup(dict, sample.StackIndex)
	if stackEntry == nil {
		stackCache[sample.StackIndex] = stack
		return stack
	}

	// Process each location in the stack
	for _, locIdx := range stackEntry.LocationIndices {
		loc := locationTableLookup(dict, locIdx)
		if loc == nil {
			continue
		}

		if len(loc.Lines) == 0 {
			// Location has no line info; use address as fallback
			stack = append(stack, "[0x"+strconv.FormatUint(loc.Address, 16)+"]")
			continue
		}

		// Get the function names from the location's lines
		for _, line := range loc.Lines {
			if line == nil {
				continue
			}

			fn := functionTableLookup(dict, line.FunctionIndex)
			if fn == nil {
				continue
			}

			name := stringTableLookup(dict, fn.NameStrindex)
			if name != "" {
				stack = append(stack, name)
			}
		}
	}

	// location_indices are leaf-first; reverse to get root-to-leaf order for the flame graph
	slices.Reverse(stack)

	// Cache the resolved stack
	stackCache[sample.StackIndex] = stack

	return stack
}

func insertStack(root *FlameNode, stack []string, value int64) {
	current := root
	root.Value += value

	for _, name := range stack {
		child, exists := current.childrenMap[name]
		if !exists {
			child = &FlameNode{
				Name:        name,
				Value:       0,
				Children:    []*FlameNode{},
				childrenMap: make(map[string]*FlameNode),
			}
			current.Children = append(current.Children, child)
			current.childrenMap[name] = child
		}

		child.Value += value
		current = child
	}
}
