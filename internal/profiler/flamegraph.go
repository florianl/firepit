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
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
)

var stackCachePool = sync.Pool{
	New: func() any {
		return make(map[int32][]FrameInfo)
	},
}

type FlameNode struct {
	Name        string                `json:"name"`
	Filename    string                `json:"filename,omitempty"`
	FrameType   string                `json:"l,omitempty"`
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

type SandwitchData struct {
	Functions []*FunctionStats `json:"functions"`
	Root      *FlameNode       `json:"root"`
}

type NamedSandwitch struct {
	Type string        `json:"type"`
	Data *SandwitchData `json:"data"`
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
			return attr.Value.GetStringValue()
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

	stackCache := stackCachePool.Get().(map[int32][]FrameInfo)
	defer func() {
		clear(stackCache)
		stackCachePool.Put(stackCache)
	}()

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

func processProfile(root *FlameNode, profile *profilespb.Profile, dict *profilespb.ProfilesDictionary, stackCache map[int32][]FrameInfo) {
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

func resolveStack(sample *profilespb.Sample, dict *profilespb.ProfilesDictionary, stackCache map[int32][]FrameInfo) []FrameInfo {
	var stack []FrameInfo

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

func insertStack(root *FlameNode, stack []FrameInfo, value int64) {
	current := root
	root.Value += value

	for _, frame := range stack {
		child, exists := current.childrenMap[frame.Name]
		if !exists {
			child = &FlameNode{
				Name:        frame.Name,
				Filename:    frame.Filename,
				FrameType:   frame.FrameType,
				Value:       0,
				Children:    []*FlameNode{},
				childrenMap: make(map[string]*FlameNode),
			}
			current.Children = append(current.Children, child)
			current.childrenMap[frame.Name] = child
		}

		child.Value += value
		current = child
	}
}

func ToSandwitch(root *FlameNode) *SandwitchData {
	stats := make(map[string]*FunctionStats)
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

	return &SandwitchData{
		Functions: functions,
		Root:      root,
	}
}

// SandwitchGraphs holds caller and callee flamegraphs for a target function
type SandwitchGraphs struct {
	Callers *FlameNode `json:"callers"`
	Callees *FlameNode `json:"callees"`
}

// ExtractSandwitchForFunction builds caller and callee flamegraphs for a target function
func ExtractSandwitchForFunction(root *FlameNode, targetName string) *FlameNode {
	// For backward compatibility, return the combined view
	// (deprecated - use ExtractSandwitchGraphs instead)
	sg := ExtractSandwitchGraphs(root, targetName)
	if sg.Callers != nil {
		return sg.Callers
	}
	return root
}

// ExtractSandwitchGraphs builds separate caller and callee flamegraphs for a target function
func ExtractSandwitchGraphs(root *FlameNode, targetName string) *SandwitchGraphs {
	if root == nil || targetName == "" {
		return &SandwitchGraphs{}
	}

	// Find all paths through the target function and build both caller and callee maps
	var callersMap = make(map[string]int64)
	var targetValue int64
	var calleesMap = make(map[string]int64)

	extractSandwitchPaths(root, targetName, []string{}, &callersMap, &targetValue, &calleesMap)

	if targetValue == 0 {
		return &SandwitchGraphs{}
	}

	// Build caller flamegraph with target as root, showing who calls it
	callersGraph := buildCallerGraph(root, targetName, targetValue)

	// Build callee flamegraph with target as root, showing what it calls
	calleeGraph := buildCalleeGraph(root, targetName, targetValue)

	return &SandwitchGraphs{
		Callers: callersGraph,
		Callees: calleeGraph,
	}
}

func buildCallerGraph(originalRoot *FlameNode, targetName string, targetValue int64) *FlameNode {
	// Find the path to target and build an inverted tree for icicle visualization
	var pathToTarget []*FlameNode
	findPathToTarget(originalRoot, targetName, []*FlameNode{}, &pathToTarget)

	if len(pathToTarget) == 0 {
		return &FlameNode{Name: targetName, Value: targetValue, Children: []*FlameNode{}, childrenMap: make(map[string]*FlameNode)}
	}

	// Build inverted tree: target at root (top, widest) with callers as descendants
	// pathToTarget[0] is root (artificial), pathToTarget[len-1] is targetFunc
	// Result: targetFunc (root) -> funcB -> funcA -> ... (excluding artificial root)
	newRoot := &FlameNode{
		Name:        targetName,
		Value:       targetValue,
		Children:    []*FlameNode{},
		childrenMap: make(map[string]*FlameNode),
	}

	// Add callers in reverse order (from immediate caller up, but excluding the artificial root node)
	currentNode := newRoot
	for i := len(pathToTarget) - 2; i > 0; i-- {
		node := pathToTarget[i]
		newNode := &FlameNode{
			Name:        node.Name,
			Value:       node.Value,
			Filename:    node.Filename,
			FrameType:   node.FrameType,
			Children:    []*FlameNode{},
			childrenMap: make(map[string]*FlameNode),
		}
		currentNode.Children = append(currentNode.Children, newNode)
		currentNode.childrenMap[newNode.Name] = newNode
		currentNode = newNode
	}

	return newRoot
}

func findPathToTarget(node *FlameNode, targetName string, currentPath []*FlameNode, resultPath *[]*FlameNode) bool {
	currentPath = append(currentPath, node)

	if node.Name == targetName {
		*resultPath = make([]*FlameNode, len(currentPath))
		copy(*resultPath, currentPath)
		return true
	}

	for _, child := range node.Children {
		if findPathToTarget(child, targetName, currentPath, resultPath) {
			return true
		}
	}

	return false
}

func buildCalleeGraph(originalRoot *FlameNode, targetName string, targetValue int64) *FlameNode {
	// Create root as the target function
	root := &FlameNode{
		Name:        targetName,
		Value:       targetValue,
		Children:    []*FlameNode{},
		childrenMap: make(map[string]*FlameNode),
	}

	// Find all callees (full subtrees) of the target
	extractCalleeSubtrees(originalRoot, targetName, root)

	return root
}


func extractCalleeSubtrees(node *FlameNode, targetName string, targetRoot *FlameNode) {
	if node.Name == targetName {
		// Found target - copy all its children (full subtrees) to targetRoot
		for _, child := range node.Children {
			copiedChild := copyFlameNode(child)
			if existing, exists := targetRoot.childrenMap[copiedChild.Name]; exists {
				// Merge with existing child
				mergeFlameNodes(existing, copiedChild)
			} else {
				targetRoot.Children = append(targetRoot.Children, copiedChild)
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
		childrenMap: make(map[string]*FlameNode),
	}
	for _, child := range node.Children {
		copiedChild := copyFlameNode(child)
		newNode.Children = append(newNode.Children, copiedChild)
		newNode.childrenMap[copiedChild.Name] = copiedChild
	}
	return newNode
}

func mergeFlameNodes(dst, src *FlameNode) {
	dst.Value += src.Value
	for _, srcChild := range src.Children {
		if dstChild, exists := dst.childrenMap[srcChild.Name]; exists {
			mergeFlameNodes(dstChild, srcChild)
		} else {
			copiedChild := copyFlameNode(srcChild)
			dst.Children = append(dst.Children, copiedChild)
			dst.childrenMap[copiedChild.Name] = copiedChild
		}
	}
}

func extractSandwitchPaths(node *FlameNode, targetName string, ancestors []string, callers *map[string]int64, targetValue *int64, callees *map[string]int64) {
	if node.Name == targetName {
		// Found target - record ancestors as callers and self value
		*targetValue += node.Value

		for _, ancestor := range ancestors {
			if ancestor != "root" {
				(*callers)[ancestor] += node.Value
			}
		}

		// Process children as callees
		for _, child := range node.Children {
			aggregateCallees(child, callees, node.Value)
		}
		return
	}

	// Continue searching in children
	newAncestors := append(ancestors, node.Name)
	for _, child := range node.Children {
		extractSandwitchPaths(child, targetName, newAncestors, callers, targetValue, callees)
	}
}

func aggregateCallees(node *FlameNode, callees *map[string]int64, parentValue int64) {
	if node.Name != "root" {
		proportion := float64(node.Value) / float64(parentValue)
		(*callees)[node.Name] += int64(float64(node.Value) * proportion)
	}

	for _, child := range node.Children {
		aggregateCallees(child, callees, node.Value)
	}
}

func collectFunctionStats(node *FlameNode, stats map[string]*FunctionStats) {
	if node.Name == "root" {
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
