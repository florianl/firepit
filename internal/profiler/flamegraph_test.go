package profiler

import (
	"slices"
	"strconv"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"

	"github.com/florianl/firepit/internal/store"
)

func TestToFlamegraphEmpty(t *testing.T) {
	entries := []store.ProfileEntry{}
	root := ToFlamegraph(entries, Options{})

	if root == nil {
		t.Fatal("Root should not be nil")
	}

	if root.Name != "root" {
		t.Fatalf("Expected root name 'root', got '%s'", root.Name)
	}

	if root.Value != 0 {
		t.Fatalf("Expected root value 0, got %d", root.Value)
	}
}

func TestFilterByResource(t *testing.T) {
	svc := store.ProfileEntry{ResourceAttributes: []*commonpb.KeyValue{{
		Key:   "service.name",
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}},
	}}}
	noAttrs := store.ProfileEntry{}
	entries := []store.ProfileEntry{svc, noAttrs}

	tests := []struct {
		filters []string
		want    int
	}{
		{nil, 2},
		{[]string{"service.name"}, 2},
		{[]string{"service.name:a"}, 1},
		{[]string{"service.name:b"}, 0},
	}
	for _, tt := range tests {
		got := FilterByResource(entries, tt.filters)
		if len(got) != tt.want {
			t.Errorf("FilterByResource(%q) = %d entries, want %d", tt.filters, len(got), tt.want)
			continue
		}
		if tt.want == 1 && got[0].ResourceAttributes == nil {
			t.Errorf("FilterByResource(%q) matched the entry without attributes", tt.filters)
		}
	}
}

// singleFrame returns a profile with one sample on a single frame.
func singleFrame(fn, file, frameType string) store.ProfileEntry {
	dict := &profilespb.ProfilesDictionary{
		StringTable:    []string{"", fn, file, "profile.frame.type"},
		AttributeTable: []*profilespb.KeyValueAndUnit{{}},
		LocationTable:  []*profilespb.Location{{Lines: []*profilespb.Line{{FunctionIndex: 0}}}},
		FunctionTable:  []*profilespb.Function{{NameStrindex: 1, FilenameStrindex: 2}},
		StackTable:     []*profilespb.Stack{{LocationIndices: []int32{0}}},
	}
	if frameType != "" {
		dict.AttributeTable = append(dict.AttributeTable, &profilespb.KeyValueAndUnit{
			KeyStrindex: 3,
			Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: frameType}},
		})
		dict.LocationTable[0].AttributeIndices = []int32{1}
	}
	profile := &profilespb.Profile{
		Samples: []*profilespb.Sample{{StackIndex: 0, Values: []int64{1}}},
	}
	return store.ProfileEntry{Profile: profile, Dictionary: dict}
}

func TestToFlamegraphSingleFrame(t *testing.T) {
	tests := []struct {
		name, file, frameType string
	}{
		{"myFunc", "main.go", ""},
		{"myFunc", "", ""},
		{"myFunc", "", "cpython"},
	}
	for _, tt := range tests {
		root := ToFlamegraph([]store.ProfileEntry{singleFrame(tt.name, tt.file, tt.frameType)}, Options{})
		if len(root.Children) != 1 {
			t.Fatalf("%+v: expected one child, got %d", tt, len(root.Children))
		}
		child := root.Children[0]
		if child.Name != tt.name || child.Filename != tt.file || child.FrameType != tt.frameType {
			t.Errorf("got %q/%q/%q, want %+v", child.Name, child.Filename, child.FrameType, tt)
		}
	}
}

// virtualFrameFixture returns a profile with two samples on the same stack
// "main" -> "work". The second sample carries a thread.name attribute.
func virtualFrameFixture() store.ProfileEntry {
	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"", "main", "work", "thread.name", "worker-1", "process.pid", "service.name"},
		AttributeTable: []*profilespb.KeyValueAndUnit{
			{},
			{
				KeyStrindex: 3, // "thread.name"
				Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 4}},
			},
			{
				KeyStrindex: 5, // "process.pid"
				Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}},
			},
			{
				KeyStrindex: 6, // "service.name"
				Value:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "override"}},
			},
		},
		LocationTable: []*profilespb.Location{
			{Lines: []*profilespb.Line{{FunctionIndex: 1}}},
			{Lines: []*profilespb.Line{{FunctionIndex: 0}}},
		},
		FunctionTable: []*profilespb.Function{
			{NameStrindex: 1},
			{NameStrindex: 2},
		},
		StackTable: []*profilespb.Stack{
			{LocationIndices: []int32{0, 1}}, // leaf first: work, main
		},
	}

	profile := &profilespb.Profile{
		Samples: []*profilespb.Sample{
			{StackIndex: 0, Values: []int64{3}},
			{StackIndex: 0, Values: []int64{5}, AttributeIndices: []int32{1, 2}},
		},
	}

	return store.ProfileEntry{
		Profile:    profile,
		Dictionary: dict,
		ResourceAttributes: []*commonpb.KeyValue{
			{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
			},
		},
	}
}

// childNames returns the names of the children of n.
func childNames(n *FlameNode) []string {
	names := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		names = append(names, c.Name)
	}
	return names
}

func TestToFlamegraphVirtualFramesResource(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{virtualFrameFixture()},
		Options{VirtualFrameKeys: []string{"service.name"}})

	if got, want := childNames(root), []string{"service.name=checkout"}; !slices.Equal(got, want) {
		t.Fatalf("root children = %v, want %v", got, want)
	}
	vf := root.Children[0]
	if vf.FrameType != VirtualFrameType {
		t.Fatalf("expected frame type %q, got %q", VirtualFrameType, vf.FrameType)
	}
	if vf.Value != 8 {
		t.Fatalf("expected virtual frame value 8, got %d", vf.Value)
	}
	if got, want := childNames(vf), []string{"main"}; !slices.Equal(got, want) {
		t.Fatalf("virtual frame children = %v, want %v", got, want)
	}
}

func TestToFlamegraphVirtualFramesSample(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{virtualFrameFixture()},
		Options{VirtualFrameKeys: []string{"service.name", "thread.name", "process.pid"}})

	if root.Value != 8 {
		t.Fatalf("expected root value 8, got %d", root.Value)
	}
	svc := root.Children[0]
	if got, want := childNames(svc), []string{"thread.name=<unset>", "thread.name=worker-1"}; !slices.Equal(got, want) {
		t.Fatalf("service children = %v, want %v", got, want)
	}

	unset := svc.Children[0]
	if unset.Value != 3 {
		t.Fatalf("expected unset thread value 3, got %d", unset.Value)
	}
	if got, want := childNames(unset), []string{"process.pid=<unset>"}; !slices.Equal(got, want) {
		t.Fatalf("unset thread children = %v, want %v", got, want)
	}

	worker := svc.Children[1]
	if worker.Value != 5 {
		t.Fatalf("expected worker thread value 5, got %d", worker.Value)
	}
	if got, want := childNames(worker), []string{"process.pid=42"}; !slices.Equal(got, want) {
		t.Fatalf("worker thread children = %v, want %v", got, want)
	}
	if got, want := childNames(worker.Children[0]), []string{"main"}; !slices.Equal(got, want) {
		t.Fatalf("pid children = %v, want %v", got, want)
	}
}

func TestToFlamegraphVirtualFramesSampleOverridesResource(t *testing.T) {
	entry := virtualFrameFixture()
	entry.Profile.Samples[1].AttributeIndices = append(entry.Profile.Samples[1].AttributeIndices, 3)

	root := ToFlamegraph([]store.ProfileEntry{entry}, Options{VirtualFrameKeys: []string{"service.name"}})

	if got, want := childNames(root), []string{"service.name=checkout", "service.name=override"}; !slices.Equal(got, want) {
		t.Fatalf("root children = %v, want %v", got, want)
	}
	if root.Children[0].Value != 3 || root.Children[1].Value != 5 {
		t.Fatalf("unexpected values %d/%d", root.Children[0].Value, root.Children[1].Value)
	}
}

func TestToFlamegraphNoVirtualFrames(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{virtualFrameFixture()}, Options{})

	if got, want := childNames(root), []string{"main"}; !slices.Equal(got, want) {
		t.Fatalf("root children = %v, want %v", got, want)
	}
}

func TestToSandwichSkipsVirtualFrames(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{virtualFrameFixture()},
		Options{VirtualFrameKeys: []string{"service.name", "thread.name"}})
	data := ToSandwich(root)

	var names []string
	for _, fn := range data.Functions {
		names = append(names, fn.Name)
	}
	slices.Sort(names)
	if want := []string{"main", "work"}; !slices.Equal(names, want) {
		t.Fatalf("sandwich functions = %v, want %v", names, want)
	}
	for _, fn := range data.Functions {
		if fn.Total != 8 {
			t.Fatalf("expected total 8 for %s, got %d", fn.Name, fn.Total)
		}
	}
}

func TestParseKeyList(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{" , ,", nil},
		{"service.name", []string{"service.name"}},
		{" service.name , thread.name,service.name ", []string{"service.name", "thread.name"}},
	}
	for _, tt := range tests {
		if got := ParseKeyList(tt.input); !slices.Equal(got, tt.want) {
			t.Errorf("ParseKeyList(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestExtractSandwichGraphsMergesVirtualFrameGroups(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{virtualFrameFixture()},
		Options{VirtualFrameKeys: []string{"thread.name"}})
	graphs := ExtractSandwichGraphs(root, "work")

	if graphs.Callees.Value != 8 {
		t.Fatalf("expected callee root value 8, got %d", graphs.Callees.Value)
	}

	callers := graphs.Callers
	if callers.Value != 8 {
		t.Fatalf("expected caller root value 8, got %d", callers.Value)
	}
	if got, want := childNames(callers), []string{"main"}; !slices.Equal(got, want) {
		t.Fatalf("caller root children = %v, want %v", got, want)
	}
	main := callers.Children[0]
	if main.Value != 8 {
		t.Fatalf("expected main value 8, got %d", main.Value)
	}
	if got, want := childNames(main), []string{"thread.name=<unset>", "thread.name=worker-1"}; !slices.Equal(got, want) {
		t.Fatalf("main children = %v, want %v", got, want)
	}
	if main.Children[0].Value != 3 || main.Children[1].Value != 5 {
		t.Fatalf("unexpected thread values %d/%d", main.Children[0].Value, main.Children[1].Value)
	}
}

// resolverFixture returns a profile with one sample per value of the stack
// main -> outer -> inlined, where inlined is inlined into outer.
func resolverFixture(prefix string, values ...int64) store.ProfileEntry {
	dict := &profilespb.ProfilesDictionary{
		StringTable: []string{"", prefix + "main", prefix + "outer", prefix + "inlined"},
		FunctionTable: []*profilespb.Function{
			{},
			{NameStrindex: 1},
			{NameStrindex: 2},
			{NameStrindex: 3},
		},
		LocationTable: []*profilespb.Location{
			{},
			{Lines: []*profilespb.Line{{FunctionIndex: 1}}},
			// Lines are leaf-first
			{Lines: []*profilespb.Line{{FunctionIndex: 3}, {FunctionIndex: 2}}},
		},
		StackTable: []*profilespb.Stack{
			{},
			{LocationIndices: []int32{2, 1}},
		},
	}
	profile := &profilespb.Profile{}
	for _, v := range values {
		profile.Samples = append(profile.Samples, &profilespb.Sample{StackIndex: 1, Values: []int64{v}})
	}
	return store.ProfileEntry{Profile: profile, Dictionary: dict}
}

func flamePath(n *FlameNode) []string {
	var path []string
	for len(n.Children) == 1 {
		n = n.Children[0]
		path = append(path, n.Name)
	}
	return path
}

func TestToFlamegraphInlinedFramesOrder(t *testing.T) {
	root := ToFlamegraph([]store.ProfileEntry{resolverFixture("", 1, 2)}, Options{})
	if got, want := flamePath(root), []string{"main", "outer", "inlined"}; !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
	if root.Value != 3 {
		t.Errorf("root value = %d, want 3", root.Value)
	}
}

// Stacks cached for one dictionary must not leak into another.
func TestToFlamegraphDictionaryChange(t *testing.T) {
	a := resolverFixture("a.", 1)
	a2 := store.ProfileEntry{Profile: a.Profile, Dictionary: a.Dictionary}
	b := resolverFixture("b.", 5)
	// Sample stack index out of range
	c := resolverFixture("c.", 7)
	c.Dictionary.StackTable = c.Dictionary.StackTable[:1]

	root := ToFlamegraph([]store.ProfileEntry{a, a2, b, c, a}, Options{})
	if root.Value != 8 {
		t.Errorf("root value = %d, want 8", root.Value)
	}
	got := map[string]int64{}
	for _, child := range root.Children {
		got[child.Name] = child.Value
	}
	if len(got) != 2 || got["a.main"] != 3 || got["b.main"] != 5 {
		t.Errorf("children = %v, want a.main=3 b.main=5", got)
	}
}

func TestFlameNodeManyChildren(t *testing.T) {
	root := &FlameNode{Name: "root"}
	for round := range 2 {
		for i := range 4 * childrenMapThreshold {
			insertFrame(root, FrameInfo{Name: "f" + strconv.Itoa(i)}, 1, 1)
		}
		if len(root.Children) != 4*childrenMapThreshold {
			t.Fatalf("round %d: %d children, want %d", round, len(root.Children), 4*childrenMapThreshold)
		}
	}
	for _, child := range root.Children {
		if child.Value != 2 {
			t.Errorf("%s value = %d, want 2", child.Name, child.Value)
		}
	}
}
