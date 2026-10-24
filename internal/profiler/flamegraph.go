// Package profiler converts OTel profile data to flame graph format.
package profiler

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/florianl/firepit/internal/store"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

// VirtualFrameType is the frame type of synthetic frames that are derived
// from resource or sample attributes rather than from code locations.
const VirtualFrameType = "virtual"

// unsetAttrValue is used for virtual frames whose attribute is not present
// on a sample or its resource.
const unsetAttrValue = "<unset>"

// Options controls how profiles are converted into a flame graph.
type Options struct {
	// VirtualFrameKeys lists attribute keys that are rendered as virtual
	// frames on top of each stack, outermost first. Sample attributes take
	// precedence over resource attributes with the same key.
	VirtualFrameKeys []string
}

var stackCachePool = sync.Pool{
	New: func() any {
		return make(map[int32][]FrameInfo)
	},
}

type FlameNode struct {
	Name        string                `json:"name"`
	Filename    string                `json:"filename,omitempty"`
	FrameType   string                `json:"frametype,omitempty"`
	Value       int64                 `json:"value"`
	Children    []*FlameNode          `json:"children,omitempty"`
	childrenMap map[string]*FlameNode `json:"-"`
}

type FrameInfo struct {
	Name      string
	Filename  string
	FrameType string
}

type NamedFlamegraph struct {
	Type string     `json:"type"`
	Root *FlameNode `json:"root"`
}

type FunctionStats struct {
	Name  string `json:"name"`
	Self  int64  `json:"self"`
	Total int64  `json:"total"`
}

type SandwichData struct {
	Functions []*FunctionStats `json:"functions"`
	Root      *FlameNode       `json:"root"`
}

type NamedSandwich struct {
	Type string        `json:"type"`
	Data *SandwichData `json:"data"`
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

// attributeTableLookup safely looks up an attribute by index.
// Returns nil if the index is out of bounds or negative.
func attributeTableLookup(dict *profilespb.ProfilesDictionary, idx int32) *profilespb.KeyValueAndUnit {
	if idx < 0 || int(idx) >= len(dict.AttributeTable) {
		return nil
	}
	return dict.AttributeTable[idx]
}

// frameTypeFromLocation extracts the profile.frame.type attribute from a Location.
func frameTypeFromLocation(dict *profilespb.ProfilesDictionary, loc *profilespb.Location) string {
	for _, attrIdx := range loc.AttributeIndices {
		attr := attributeTableLookup(dict, attrIdx)
		if attr == nil || attr.Value == nil {
			continue
		}
		if stringTableLookup(dict, attr.KeyStrindex) == "profile.frame.type" {
			return store.ResolveStringValue(dict.StringTable, attr.Value)
		}
	}
	return ""
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
		if entry.ResourceAttributes == nil {
			continue
		}
		st := entry.Dictionary.GetStringTable()
		for _, attr := range entry.ResourceAttributes {
			if attr.Value == nil {
				continue
			}
			if k, v := store.ResolveKeyValue(st, attr); k == key && v == value {
				filtered = append(filtered, entry)
				break
			}
		}
	}
	return filtered
}

// ParseKeyList splits a comma separated list of attribute keys, trimming
// whitespace and dropping empty and duplicate keys while preserving order.
func ParseKeyList(s string) []string {
	var keys []string
	for k := range strings.SplitSeq(s, ",") {
		k = strings.TrimSpace(k)
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// attrValueString returns a string representation of scalar attribute values.
func attrValueString(st []string, v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_StringValueStrindex:
		return store.ResolveStringValue(st, v)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	}
	return ""
}

// virtualFrameName formats the name of a virtual frame.
func virtualFrameName(key, value string) string {
	if value == "" {
		value = unsetAttrValue
	}
	return key + "=" + value
}

// resourceVirtualFrames returns one virtual frame per key, resolved against
// the resource attributes of entry.
func resourceVirtualFrames(entry store.ProfileEntry, keys []string) []FrameInfo {
	if len(keys) == 0 {
		return nil
	}
	st := entry.Dictionary.GetStringTable()
	frames := make([]FrameInfo, len(keys))
	for i, key := range keys {
		var value string
		for _, attr := range entry.ResourceAttributes {
			if k := store.ResolveKey(st, attr); k == key {
				value = attrValueString(st, attr.GetValue())
				break
			}
		}
		frames[i] = FrameInfo{Name: virtualFrameName(key, value), FrameType: VirtualFrameType}
	}
	return frames
}

func ToFlamegraph(entries []store.ProfileEntry, opts Options) *FlameNode {
	root := &FlameNode{
		Name:        "root",
		Value:       0,
		Children:    []*FlameNode{},
		childrenMap: nil,
	}

	profileCount := 0
	for _, entry := range entries {
		if entry.Profile != nil && entry.Dictionary != nil {
			profileCount++
			resFrames := resourceVirtualFrames(entry, opts.VirtualFrameKeys)
			stackCache := stackCachePool.Get().(map[int32][]FrameInfo)
			processProfile(root, entry.Profile, entry.Dictionary, stackCache, opts.VirtualFrameKeys, resFrames)
			clear(stackCache)
			stackCachePool.Put(stackCache)
		}
	}

	if profileCount > 0 && root.Value == 0 {
		slog.Warn("Processed profiles but got no data", "profile_count", profileCount)
	}

	return root
}

func processProfile(root *FlameNode, profile *profilespb.Profile, dict *profilespb.ProfilesDictionary, stackCache map[int32][]FrameInfo,
	virtualKeys []string, resFrames []FrameInfo,
) {
	if profile == nil || len(profile.Samples) == 0 {
		return
	}

	// prefixBuf is reused across samples, as insertStack does not retain it.
	var prefixBuf []FrameInfo
	if len(virtualKeys) > 0 {
		prefixBuf = make([]FrameInfo, len(virtualKeys))
	}

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

		prefix := resFrames
		if len(virtualKeys) > 0 && len(sample.AttributeIndices) > 0 {
			prefix = sampleVirtualFrames(prefixBuf, sample, dict, virtualKeys, resFrames)
		}

		insertStack(root, prefix, stack, value)
	}
}

// sampleVirtualFrames fills buf with resFrames and overrides frames whose key
// is set as a sample attribute. It returns resFrames if no key matched.
func sampleVirtualFrames(buf []FrameInfo, sample *profilespb.Sample, dict *profilespb.ProfilesDictionary, keys []string, resFrames []FrameInfo) []FrameInfo {
	matched := false
	for _, attrIdx := range sample.AttributeIndices {
		attr := attributeTableLookup(dict, attrIdx)
		if attr == nil || attr.Value == nil {
			continue
		}
		i := slices.Index(keys, stringTableLookup(dict, attr.KeyStrindex))
		if i < 0 {
			continue
		}
		if !matched {
			copy(buf, resFrames)
			matched = true
		}
		buf[i] = FrameInfo{
			Name:      virtualFrameName(keys[i], attrValueString(dict.StringTable, attr.Value)),
			FrameType: VirtualFrameType,
		}
	}
	if !matched {
		return resFrames
	}
	return buf
}

func resolveStack(sample *profilespb.Sample, dict *profilespb.ProfilesDictionary, stackCache map[int32][]FrameInfo) []FrameInfo {
	if dict == nil {
		return nil
	}

	// Check if this stack is already resolved
	if cached, ok := stackCache[sample.StackIndex]; ok {
		return cached
	}

	// Get the stack from the dictionary using the stack_index
	stackEntry := stackTableLookup(dict, sample.StackIndex)
	if stackEntry == nil {
		stackCache[sample.StackIndex] = nil
		return nil
	}

	stack := make([]FrameInfo, 0, len(stackEntry.LocationIndices))
	// Process each location in the stack
	for _, locIdx := range stackEntry.LocationIndices {
		loc := locationTableLookup(dict, locIdx)
		if loc == nil {
			continue
		}

		frameType := frameTypeFromLocation(dict, loc)

		if len(loc.Lines) == 0 {
			// Location has no line info; use address as fallback
			stack = append(stack, FrameInfo{Name: "[0x" + strconv.FormatUint(loc.Address, 16) + "]", FrameType: frameType})
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
				filename := stringTableLookup(dict, fn.FilenameStrindex)
				if line.Line != 0 {
					filename += fmt.Sprintf(":%d", line.Line)
				}
				stack = append(stack, FrameInfo{Name: name, Filename: filename, FrameType: frameType})
			}
		}
	}

	// location_indices are leaf-first; reverse to get root-to-leaf order for the flame graph
	slices.Reverse(stack)

	// Cache the resolved stack
	stackCache[sample.StackIndex] = stack

	return stack
}

// insertStack adds value along the path of prefix followed by stack.
func insertStack(root *FlameNode, prefix, stack []FrameInfo, value int64) {
	root.Value += value
	current := root
	for _, frame := range prefix {
		current = insertFrame(current, frame, value)
	}
	for _, frame := range stack {
		current = insertFrame(current, frame, value)
	}
}

// insertFrame adds value to the child of current matching frame, creating it
// if needed, and returns that child.
func insertFrame(current *FlameNode, frame FrameInfo, value int64) *FlameNode {
	var child *FlameNode
	var exists bool
	if current.childrenMap != nil {
		child, exists = current.childrenMap[frame.Name]
	}
	if !exists {
		child = &FlameNode{
			Name:        frame.Name,
			Filename:    frame.Filename,
			FrameType:   frame.FrameType,
			Value:       0,
			Children:    []*FlameNode{},
			childrenMap: nil,
		}
		current.Children = append(current.Children, child)

		if current.childrenMap == nil {
			current.childrenMap = make(map[string]*FlameNode)
		}
		current.childrenMap[frame.Name] = child
	}

	child.Value += value
	return child
}

func ToSandwich(root *FlameNode) *SandwichData {
	stats := make(map[string]*FunctionStats, len(root.Children))
	collectFunctionStats(root, stats)

	functions := make([]*FunctionStats, 0, len(stats))
	for _, stat := range stats {
		functions = append(functions, stat)
	}

	slices.SortFunc(functions, func(a, b *FunctionStats) int {
		if a.Total != b.Total {
			return int(b.Total - a.Total)
		}
		return strings.Compare(a.Name, b.Name)
	})

	return &SandwichData{
		Functions: functions,
		Root:      root,
	}
}

// SandwichGraphs holds caller and callee flamegraphs for a target function
type SandwichGraphs struct {
	Callers *FlameNode `json:"callers"`
	Callees *FlameNode `json:"callees"`
}

// ExtractSandwichGraphs builds separate caller and callee flamegraphs for a target function
func ExtractSandwichGraphs(root *FlameNode, targetName string) *SandwichGraphs {
	if root == nil || targetName == "" {
		return &SandwichGraphs{}
	}

	// Build caller flamegraph with target as root, showing who calls it
	callersGraph := buildCallerGraph(root, targetName)

	// Build callee flamegraph with target as root, showing what it calls
	calleeGraph := buildCalleeGraph(root, targetName)

	return &SandwichGraphs{
		Callers: callersGraph,
		Callees: calleeGraph,
	}
}

func buildCallerGraph(originalRoot *FlameNode, targetName string) *FlameNode {
	// Build an inverted tree for icicle visualization: the target at the root
	// with its callers as descendants. Every occurrence of the target is merged,
	// each caller accumulating the value of the target it leads to.
	newRoot := &FlameNode{Name: targetName, Children: []*FlameNode{}, childrenMap: nil}
	found := false

	var walk func(node *FlameNode, callers []*FlameNode)
	walk = func(node *FlameNode, callers []*FlameNode) {
		if node.Name == targetName {
			if !found {
				newRoot.Filename = node.Filename
				newRoot.FrameType = node.FrameType
				found = true
			}
			newRoot.Value += node.Value

			// Add callers in reverse order, starting with the immediate caller
			current := newRoot
			for i := len(callers) - 1; i >= 0; i-- {
				c := callers[i]
				current = insertFrame(current, FrameInfo{Name: c.Name, Filename: c.Filename, FrameType: c.FrameType}, node.Value)
			}
			return
		}

		callers = append(callers, node)
		for _, child := range node.Children {
			walk(child, callers)
		}
	}

	// Skip the artificial root node
	for _, child := range originalRoot.Children {
		walk(child, nil)
	}

	return newRoot
}

func buildCalleeGraph(originalRoot *FlameNode, targetName string) *FlameNode {
	// Create root as the target function
	root := &FlameNode{
		Name:        targetName,
		Children:    []*FlameNode{},
		childrenMap: nil,
	}

	// Find all callees (full subtrees) of the target
	extractCalleeSubtrees(originalRoot, targetName, root)

	return root
}

func extractCalleeSubtrees(node *FlameNode, targetName string, targetRoot *FlameNode) {
	if node.Name == targetName {
		// Found target - accumulate its value and copy all its children (full subtrees) to targetRoot
		if targetRoot.Value == 0 {
			targetRoot.Filename = node.Filename
			targetRoot.FrameType = node.FrameType
		}
		targetRoot.Value += node.Value
		for _, child := range node.Children {
			copiedChild := copyFlameNode(child)
			var existing *FlameNode
			var exists bool
			if targetRoot.childrenMap != nil {
				existing, exists = targetRoot.childrenMap[copiedChild.Name]
			}
			if exists {
				// Merge with existing child
				mergeFlameNodes(existing, copiedChild)
			} else {
				targetRoot.Children = append(targetRoot.Children, copiedChild)
				if targetRoot.childrenMap == nil {
					targetRoot.childrenMap = make(map[string]*FlameNode)
				}
				targetRoot.childrenMap[copiedChild.Name] = copiedChild
			}
		}
		return
	}

	// Continue searching in children
	for _, child := range node.Children {
		extractCalleeSubtrees(child, targetName, targetRoot)
	}
}

func copyFlameNode(node *FlameNode) *FlameNode {
	if node == nil {
		return nil
	}
	newNode := &FlameNode{
		Name:        node.Name,
		Value:       node.Value,
		Filename:    node.Filename,
		FrameType:   node.FrameType,
		Children:    make([]*FlameNode, 0, len(node.Children)),
		childrenMap: nil,
	}
	for _, child := range node.Children {
		copiedChild := copyFlameNode(child)
		newNode.Children = append(newNode.Children, copiedChild)
		if newNode.childrenMap == nil {
			newNode.childrenMap = make(map[string]*FlameNode)
		}
		newNode.childrenMap[copiedChild.Name] = copiedChild
	}
	return newNode
}

func mergeFlameNodes(dst, src *FlameNode) {
	dst.Value += src.Value
	for _, srcChild := range src.Children {
		var dstChild *FlameNode
		var exists bool
		if dst.childrenMap != nil {
			dstChild, exists = dst.childrenMap[srcChild.Name]
		}
		if exists {
			mergeFlameNodes(dstChild, srcChild)
		} else {
			copiedChild := copyFlameNode(srcChild)
			dst.Children = append(dst.Children, copiedChild)
			if dst.childrenMap == nil {
				dst.childrenMap = make(map[string]*FlameNode)
			}
			dst.childrenMap[copiedChild.Name] = copiedChild
		}
	}
}

func collectFunctionStats(node *FlameNode, stats map[string]*FunctionStats) {
	// The synthetic root and virtual frames are not functions.
	if node.Name == "root" || node.FrameType == VirtualFrameType {
		for _, child := range node.Children {
			collectFunctionStats(child, stats)
		}
		return
	}

	if _, exists := stats[node.Name]; !exists {
		stats[node.Name] = &FunctionStats{
			Name:  node.Name,
			Self:  0,
			Total: 0,
		}
	}

	stat := stats[node.Name]
	stat.Total += node.Value
	stat.Self += calculateSelfValue(node)

	for _, child := range node.Children {
		collectFunctionStats(child, stats)
	}
}

func calculateSelfValue(node *FlameNode) int64 {
	if len(node.Children) == 0 {
		return node.Value
	}

	childrenSum := int64(0)
	for _, child := range node.Children {
		childrenSum += child.Value
	}

	self := node.Value - childrenSum
	if self < 0 {
		return 0
	}
	return self
}
