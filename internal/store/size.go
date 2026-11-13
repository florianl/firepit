package store

import (
	"unsafe"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Heap usage estimates of decoded messages. The wire size reported by
// proto.Size is several times smaller.

// sizeClasses are the sizes the Go allocator rounds small allocations up to.
var sizeClasses = [...]uintptr{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256,
	288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280,
	1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456, 4096, 4864, 5120, 5504, 6144,
	6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384, 18432,
	19072, 20480, 21760, 24576, 27264, 28672, 32768,
}

const pageSize = 8192

// allocSize returns the memory used by an allocation of n bytes.
func allocSize(n uintptr) int64 {
	if n == 0 {
		return 0
	}
	if n > sizeClasses[len(sizeClasses)-1] {
		return int64((n + pageSize - 1) / pageSize * pageSize)
	}
	lo, hi := 0, len(sizeClasses)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if sizeClasses[mid] < n {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return int64(sizeClasses[lo])
}

// sliceSize returns the memory used by the backing array of s.
func sliceSize[T any](s []T) int64 {
	var zero T
	return allocSize(uintptr(cap(s)) * unsafe.Sizeof(zero))
}

// stringSize returns the memory used by the bytes of s.
func stringSize(s string) int64 {
	return allocSize(uintptr(len(s)))
}

// stringsSize returns the memory used by s and its strings.
func stringsSize(s []string) int64 {
	n := sliceSize(s)
	for _, str := range s {
		n += stringSize(str)
	}
	return n
}

// structSize returns the memory used by the allocation of a T.
func structSize[T any]() int64 {
	var zero T
	return allocSize(unsafe.Sizeof(zero))
}

// profileSize returns the heap usage of p.
func profileSize(p *profilespb.Profile) int64 {
	if p == nil {
		return 0
	}
	n := structSize[profilespb.Profile]()
	if p.SampleType != nil {
		n += structSize[profilespb.ValueType]()
	}
	if p.PeriodType != nil {
		n += structSize[profilespb.ValueType]()
	}
	n += sliceSize(p.ProfileId) + stringSize(p.OriginalPayloadFormat) +
		sliceSize(p.OriginalPayload) + sliceSize(p.AttributeIndices)

	n += sliceSize(p.Samples)
	sampleStruct := structSize[profilespb.Sample]()
	for _, s := range p.Samples {
		if s == nil {
			continue
		}
		n += sampleStruct + sliceSize(s.AttributeIndices) + sliceSize(s.Values) + sliceSize(s.TimestampsUnixNano)
	}
	return n
}

// dictionarySize returns the heap usage of d.
func dictionarySize(d *profilespb.ProfilesDictionary) int64 {
	if d == nil {
		return 0
	}
	n := structSize[profilespb.ProfilesDictionary]()

	n += sliceSize(d.MappingTable)
	for _, m := range d.MappingTable {
		if m != nil {
			n += structSize[profilespb.Mapping]() + sliceSize(m.AttributeIndices)
		}
	}

	n += sliceSize(d.LocationTable)
	locationStruct, lineStruct := structSize[profilespb.Location](), structSize[profilespb.Line]()
	for _, l := range d.LocationTable {
		if l == nil {
			continue
		}
		n += locationStruct + sliceSize(l.AttributeIndices) + sliceSize(l.Lines)
		for _, line := range l.Lines {
			if line != nil {
				n += lineStruct
			}
		}
	}

	n += sliceSize(d.FunctionTable)
	functionStruct := structSize[profilespb.Function]()
	for _, f := range d.FunctionTable {
		if f != nil {
			n += functionStruct
		}
	}

	n += sliceSize(d.LinkTable)
	for _, l := range d.LinkTable {
		if l != nil {
			n += structSize[profilespb.Link]() + sliceSize(l.TraceId) + sliceSize(l.SpanId)
		}
	}

	n += stringsSize(d.StringTable)

	n += sliceSize(d.AttributeTable)
	for _, a := range d.AttributeTable {
		if a != nil {
			n += structSize[profilespb.KeyValueAndUnit]() + anyValueSize(a.Value)
		}
	}

	n += sliceSize(d.StackTable)
	stackStruct := structSize[profilespb.Stack]()
	for _, s := range d.StackTable {
		if s != nil {
			n += stackStruct + sliceSize(s.LocationIndices)
		}
	}
	return n
}

// resourceSize returns the heap usage of r.
func resourceSize(r *resourcepb.Resource) int64 {
	if r == nil {
		return 0
	}
	n := structSize[resourcepb.Resource]() + keyValuesSize(r.Attributes)
	n += sliceSize(r.EntityRefs)
	for _, e := range r.EntityRefs {
		if e != nil {
			n += structSize[commonpb.EntityRef]() + stringSize(e.SchemaUrl) + stringSize(e.Type) +
				stringsSize(e.IdKeys) + stringsSize(e.DescriptionKeys)
		}
	}
	return n
}

// keyValuesSize returns the heap usage of kvs.
func keyValuesSize(kvs []*commonpb.KeyValue) int64 {
	n := sliceSize(kvs)
	for _, kv := range kvs {
		if kv != nil {
			n += structSize[commonpb.KeyValue]() + stringSize(kv.Key) + anyValueSize(kv.Value)
		}
	}
	return n
}

// anyValueSize returns the heap usage of v.
func anyValueSize(v *commonpb.AnyValue) int64 {
	if v == nil {
		return 0
	}
	n := structSize[commonpb.AnyValue]()
	// Oneof values are boxed in a wrapper struct.
	switch val := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		n += structSize[commonpb.AnyValue_StringValue]() + stringSize(val.StringValue)
	case *commonpb.AnyValue_BoolValue:
		n += structSize[commonpb.AnyValue_BoolValue]()
	case *commonpb.AnyValue_IntValue:
		n += structSize[commonpb.AnyValue_IntValue]()
	case *commonpb.AnyValue_DoubleValue:
		n += structSize[commonpb.AnyValue_DoubleValue]()
	case *commonpb.AnyValue_StringValueStrindex:
		n += structSize[commonpb.AnyValue_StringValueStrindex]()
	case *commonpb.AnyValue_BytesValue:
		n += structSize[commonpb.AnyValue_BytesValue]() + sliceSize(val.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		n += structSize[commonpb.AnyValue_ArrayValue]()
		if val.ArrayValue != nil {
			n += structSize[commonpb.ArrayValue]() + sliceSize(val.ArrayValue.Values)
			for _, elem := range val.ArrayValue.Values {
				n += anyValueSize(elem)
			}
		}
	case *commonpb.AnyValue_KvlistValue:
		n += structSize[commonpb.AnyValue_KvlistValue]()
		if val.KvlistValue != nil {
			n += structSize[commonpb.KeyValueList]() + keyValuesSize(val.KvlistValue.Values)
		}
	}
	return n
}
