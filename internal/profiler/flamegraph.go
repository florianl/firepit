// Package profiler converts OTel profile data to flame graph format.
package profiler

import (
	"cmp"
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
	// samples counts the samples through the node. Unlike Value it tells
	// when the node can be pruned, as sample values may be negative.
	samples int64
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

// FilterByResource returns the entries whose resource matches all "key:value" filters.
func FilterByResource(entries []store.ProfileEntry, filters []string) []store.ProfileEntry {
	filters = normalizeFilters(filters)
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
	root := newFlameRoot()

	resolver := stackResolverPool.Get().(*stackResolver)
	profileCount := 0
	for _, entry := range entries {
		if entry.Profile != nil && entry.Dictionary != nil {
			profileCount++
			applyEntry(root, entry, opts, resolver, 1)
		}
	}
	resolver.release()
	stackResolverPool.Put(resolver)

	if profileCount > 0 && root.Value == 0 {
		slog.Warn("Processed profiles but got no data", "profile_count", profileCount)
	}

	return root
}

// applyEntry adds (sign 1) or removes (sign -1) the samples of entry.
// Removal leaves empty nodes behind, see pruneFlameNode.
func applyEntry(root *FlameNode, entry store.ProfileEntry, opts Options, resolver *stackResolver, sign int64) {
	if entry.Profile == nil || entry.Dictionary == nil {
		return
	}
	resFrames := resourceVirtualFrames(entry, opts.VirtualFrameKeys)
	resolver.use(entry.Dictionary)
	processProfile(root, entry.Profile, resolver, opts.VirtualFrameKeys, resFrames, sign)
}

// pruneFlameNode removes the descendants of node without samples.
func pruneFlameNode(node *FlameNode) {
	kept := node.Children[:0]
	for _, child := range node.Children {
		if child.samples == 0 {
			delete(node.childrenMap, child.Name)
			continue
		}
		pruneFlameNode(child)
		kept = append(kept, child)
	}
	clear(node.Children[len(kept):])
	node.Children = kept
}

func processProfile(root *FlameNode, profile *profilespb.Profile, resolver *stackResolver,
	virtualKeys []string, resFrames []FrameInfo, sign int64,
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
		// Without values, each timestamp is one occurrence.
		value := int64(len(sample.TimestampsUnixNano))
		if len(sample.Values) > 0 {
			value = sample.Values[0]
		}
		if value == 0 {
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

		insertStack(root, prefix, stack, sign*value, sign)
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

// insertStack adds value and samples along the path of prefix and stack.
func insertStack(root *FlameNode, prefix, stack []FrameInfo, value, samples int64) {
	root.Value += value
	root.samples += samples
	current := root
	for _, frame := range prefix {
		current = insertFrame(current, frame, value, samples)
	}
	for _, frame := range stack {
		current = insertFrame(current, frame, value, samples)
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

// insertFrame adds value and samples to the child for frame, creating it if needed.
func insertFrame(current *FlameNode, frame FrameInfo, value, samples int64) *FlameNode {
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
	child.samples += samples
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
		return cmp.Or(cmp.Compare(b.Total, a.Total), strings.Compare(a.Name, b.Name))
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

// buildCallerGraph returns an inverted tree with the merged occurrences of
// targetName at the root and their callers as descendants, or nil if not found.
func buildCallerGraph(originalRoot *FlameNode, targetName string) *FlameNode {
	var newRoot *FlameNode

	var walk func(node *FlameNode, callers []*FlameNode)
	walk = func(node *FlameNode, callers []*FlameNode) {
		if node.Name == targetName {
			if newRoot == nil {
				newRoot = newTargetNode(node)
			}
			newRoot.Value += node.Value

			// Starting with the immediate caller
			current := newRoot
			for _, c := range slices.Backward(callers) {
				current = insertFrame(current, FrameInfo{Name: c.Name, Filename: c.Filename, FrameType: c.FrameType}, node.Value, 1)
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

// buildCalleeGraph returns the merged subtrees of all occurrences of
// targetName, or nil if not found.
func buildCalleeGraph(originalRoot *FlameNode, targetName string) *FlameNode {
	var newRoot *FlameNode

	var walk func(node *FlameNode)
	walk = func(node *FlameNode) {
		if node.Name == targetName {
			if newRoot == nil {
				newRoot = newTargetNode(node)
			}
			mergeFlameNodes(newRoot, node)
			return
		}
		for _, child := range node.Children {
			walk(child)
		}
	}

	walk(originalRoot)
	return newRoot
}

// newTargetNode returns an empty node with the frame of node.
func newTargetNode(node *FlameNode) *FlameNode {
	return &FlameNode{Name: node.Name, Filename: node.Filename, FrameType: node.FrameType, Children: []*FlameNode{}}
}

func copyFlameNode(node *FlameNode) *FlameNode {
	if node == nil {
		return nil
	}
	newNode := &FlameNode{
		Name:      node.Name,
		Value:     node.Value,
		Filename:  node.Filename,
		FrameType: node.FrameType,
		Children:  make([]*FlameNode, 0, len(node.Children)),
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
		stats[node.Name] = &FunctionStats{Name: node.Name}
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
