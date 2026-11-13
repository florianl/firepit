// Package profiler converts OTel profile data to flame graph format.
package profiler

import (
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

// maxStackDepth is the maximum number of frames rendered per stack. Deeper
// stacks would nest too deeply for encoding/json.
const maxStackDepth = 2048

// truncatedFrameName is the frame that replaces frames beyond maxStackDepth.
const truncatedFrameName = "[stack truncated]"

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

var stackResolverPool = sync.Pool{
	New: func() any {
		return &stackResolver{}
	},
}

// childrenMapThreshold is the number of children above which a node indexes
// them with a map. Most nodes have only a few children.
const childrenMapThreshold = 8

// frameRange is the range [start, end) of frames in a stackResolver arena.
type frameRange struct {
	start, end int32
	done       bool
}

// stackResolver resolves and caches the stacks of one dictionary. The frame
// arenas are reused across dictionaries.
type stackResolver struct {
	dict *profilespb.ProfilesDictionary
	// Indexed by stack and location index, frames in root-to-leaf order.
	stacks     []frameRange
	stackArena []FrameInfo
	locs       []frameRange
	locArena   []FrameInfo
	// Indexed by attribute index, valid for one set of virtual frame keys.
	virtual []virtualFrame
}

// virtualFrame caches the virtual frame of an attribute.
type virtualFrame struct {
	// Index into the virtual frame keys, or -1 if not a virtual frame.
	key   int
	frame FrameInfo
	done  bool
}

// use switches the resolver to dict, dropping the cache if dict changed.
func (r *stackResolver) use(dict *profilespb.ProfilesDictionary) {
	if r.dict == dict {
		return
	}
	r.release()
	r.dict = dict
	if dict == nil {
		return
	}
	r.stacks = slices.Grow(r.stacks, len(dict.StackTable))[:len(dict.StackTable)]
	r.locs = slices.Grow(r.locs, len(dict.LocationTable))[:len(dict.LocationTable)]
	r.virtual = slices.Grow(r.virtual, len(dict.AttributeTable))[:len(dict.AttributeTable)]
}

// release drops the cache but keeps the allocated buffers.
func (r *stackResolver) release() {
	r.dict = nil
	clear(r.stacks)
	clear(r.locs)
	// Drop references to the dictionary strings.
	clear(r.stackArena)
	clear(r.locArena)
	clear(r.virtual)
	r.virtual = r.virtual[:0]
	r.stacks, r.stackArena = r.stacks[:0], r.stackArena[:0]
	r.locs, r.locArena = r.locs[:0], r.locArena[:0]
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

// FilterByResourceType returns the entries matching the single filter resourceType.
func FilterByResourceType(entries []store.ProfileEntry, resourceType string) []store.ProfileEntry {
	if resourceType == "" {
		return entries
	}
	return FilterByResource(entries, []string{resourceType})
}

// FilterByResource returns the entries whose resource matches all "key:value" filters.
func FilterByResource(entries []store.ProfileEntry, filters []string) []store.ProfileEntry {
	filters = slices.DeleteFunc(slices.Clone(filters), func(f string) bool {
		return !strings.Contains(f, ":")
	})
	if len(filters) == 0 {
		return entries
	}

	var filtered []store.ProfileEntry
	for _, entry := range entries {
		if store.MatchResourceFilters(entry.Dictionary.GetStringTable(), entry.ResourceAttributes, filters) {
			filtered = append(filtered, entry)
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
	return store.ResolveValue(st, v)
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

	resolver := stackResolverPool.Get().(*stackResolver)
	profileCount := 0
	for _, entry := range entries {
		if entry.Profile != nil && entry.Dictionary != nil {
			profileCount++
			resFrames := resourceVirtualFrames(entry, opts.VirtualFrameKeys)
			resolver.use(entry.Dictionary)
			processProfile(root, entry.Profile, entry.Dictionary, resolver, opts.VirtualFrameKeys, resFrames)
		}
	}
	resolver.release()
	stackResolverPool.Put(resolver)

	if profileCount > 0 && root.Value == 0 {
		slog.Warn("Processed profiles but got no data", "profile_count", profileCount)
	}

	return root
}

func processProfile(root *FlameNode, profile *profilespb.Profile, dict *profilespb.ProfilesDictionary, resolver *stackResolver,
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

		stack := resolver.resolveStack(sample.StackIndex)
		if len(stack) == 0 {
			continue
		}

		prefix := resFrames
		if len(virtualKeys) > 0 && len(sample.AttributeIndices) > 0 {
			prefix = resolver.sampleVirtualFrames(prefixBuf, sample, virtualKeys, resFrames)
		}

		insertStack(root, prefix, stack, value)
	}
}

// sampleVirtualFrames fills buf with resFrames and overrides frames whose key
// is set as a sample attribute. It returns resFrames if no key matched.
func (r *stackResolver) sampleVirtualFrames(buf []FrameInfo, sample *profilespb.Sample, keys []string, resFrames []FrameInfo) []FrameInfo {
	matched := false
	for _, attrIdx := range sample.AttributeIndices {
		if attrIdx < 0 || int(attrIdx) >= len(r.virtual) {
			continue
		}
		vf := &r.virtual[attrIdx]
		if !vf.done {
			*vf = virtualFrame{key: -1, done: true}
			if attr := r.dict.AttributeTable[attrIdx]; attr != nil && attr.Value != nil {
				vf.key = slices.Index(keys, stringTableLookup(r.dict, attr.KeyStrindex))
				if vf.key >= 0 {
					vf.frame = FrameInfo{
						Name:      virtualFrameName(keys[vf.key], attrValueString(r.dict.StringTable, attr.Value)),
						FrameType: VirtualFrameType,
					}
				}
			}
		}
		if vf.key < 0 {
			continue
		}
		if !matched {
			copy(buf, resFrames)
			matched = true
		}
		buf[vf.key] = vf.frame
	}
	if !matched {
		return resFrames
	}
	return buf
}

// resolveStack returns the frames of stack idx in root-to-leaf order. The
// result is valid until the next resolveStack or use call.
func (r *stackResolver) resolveStack(idx int32) []FrameInfo {
	if idx < 0 || int(idx) >= len(r.stacks) {
		return nil
	}
	if s := r.stacks[idx]; s.done {
		return r.stackArena[s.start:s.end]
	}

	start := len(r.stackArena)
	if stackEntry := r.dict.StackTable[idx]; stackEntry != nil {
		// location_indices are leaf-first, keep the root side
		locationIndices := stackEntry.LocationIndices
		truncated := len(locationIndices) > maxStackDepth
		if truncated {
			locationIndices = locationIndices[len(locationIndices)-maxStackDepth:]
		}

		for _, locIdx := range slices.Backward(locationIndices) {
			r.stackArena = append(r.stackArena, r.resolveLocation(locIdx)...)
		}

		// Inlined functions can still exceed the limit
		if len(r.stackArena)-start > maxStackDepth {
			r.stackArena = r.stackArena[:start+maxStackDepth]
			truncated = true
		}
		if truncated {
			r.stackArena = append(r.stackArena, FrameInfo{Name: truncatedFrameName})
		}
	}
	end := len(r.stackArena)

	r.stacks[idx] = frameRange{start: int32(start), end: int32(end), done: true}
	return r.stackArena[start:end]
}

// resolveLocation returns the frames of location idx in root-to-leaf order,
// including inlined functions. The result is valid until the next
// resolveLocation or use call.
func (r *stackResolver) resolveLocation(idx int32) []FrameInfo {
	if idx < 0 || int(idx) >= len(r.locs) {
		return nil
	}
	if l := r.locs[idx]; l.done {
		return r.locArena[l.start:l.end]
	}

	dict := r.dict
	start := len(r.locArena)
	if loc := dict.LocationTable[idx]; loc != nil {
		frameType := frameTypeFromLocation(dict, loc)

		if len(loc.Lines) == 0 {
			// Location has no line info; use address as fallback
			r.locArena = append(r.locArena, FrameInfo{Name: "[0x" + strconv.FormatUint(loc.Address, 16) + "]", FrameType: frameType})
		}

		// Lines are leaf-first
		for _, line := range slices.Backward(loc.Lines) {
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
					filename += ":" + strconv.FormatInt(line.Line, 10)
				}
				r.locArena = append(r.locArena, FrameInfo{Name: name, Filename: filename, FrameType: frameType})
			}
		}
	}
	end := len(r.locArena)

	r.locs[idx] = frameRange{start: int32(start), end: int32(end), done: true}
	return r.locArena[start:end]
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

// findChild returns the child of n with the given name, or nil.
func (n *FlameNode) findChild(name string) *FlameNode {
	if n.childrenMap != nil {
		return n.childrenMap[name]
	}
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

// addChild appends child to n, which must not have a child of the same name.
func (n *FlameNode) addChild(child *FlameNode) {
	n.Children = append(n.Children, child)
	if n.childrenMap != nil {
		n.childrenMap[child.Name] = child
		return
	}
	if len(n.Children) > childrenMapThreshold {
		n.childrenMap = make(map[string]*FlameNode, 2*len(n.Children))
		for _, c := range n.Children {
			n.childrenMap[c.Name] = c
		}
	}
}

// insertFrame adds value to the child of current matching frame, creating it
// if needed, and returns that child.
func insertFrame(current *FlameNode, frame FrameInfo, value int64) *FlameNode {
	child := current.findChild(frame.Name)
	if child == nil {
		child = &FlameNode{
			Name:      frame.Name,
			Filename:  frame.Filename,
			FrameType: frame.FrameType,
			Children:  []*FlameNode{},
		}
		current.addChild(child)
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
			if existing := targetRoot.findChild(child.Name); existing != nil {
				// Merge with existing child
				mergeFlameNodes(existing, child)
			} else {
				targetRoot.addChild(copyFlameNode(child))
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
		newNode.addChild(copyFlameNode(child))
	}
	return newNode
}

func mergeFlameNodes(dst, src *FlameNode) {
	dst.Value += src.Value
	for _, srcChild := range src.Children {
		if dstChild := dst.findChild(srcChild.Name); dstChild != nil {
			mergeFlameNodes(dstChild, srcChild)
		} else {
			dst.addChild(copyFlameNode(srcChild))
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
