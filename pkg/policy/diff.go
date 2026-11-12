package policy

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Path is a field path. List elements are segments of the form "[name=x]" when
// the element has a unique name, or "[*]" when it cannot be keyed.
type Path []string

func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 && !strings.HasPrefix(s, "[") {
			b.WriteByte('.')
		}
		b.WriteString(s)
	}
	return b.String()
}

type Change struct {
	Path Path
	Old  any
	New  any
}

// Diff returns the leaf-level changes between two objects after dropping
// server-managed fields. A nil old object yields the full content of new.
func Diff(old, new map[string]any) []Change {
	var out []Change
	diffValue(nil, normalize(old), normalize(new), &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path.String() < out[j].Path.String() })
	return out
}

func normalize(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		switch k {
		case "status", "apiVersion", "kind":
		case "metadata":
			if md := normalizeMetadata(v); len(md) > 0 {
				out[k] = md
			}
		default:
			out[k] = v
		}
	}
	return out
}

func normalizeMetadata(v any) map[string]any {
	md, _ := v.(map[string]any)
	out := map[string]any{}
	if labels, ok := md["labels"].(map[string]any); ok && len(labels) > 0 {
		out["labels"] = labels
	}
	if ann, ok := md["annotations"].(map[string]any); ok {
		kept := map[string]any{}
		for k, v := range ann {
			if !strings.HasPrefix(k, AnnotationPrefix) {
				kept[k] = v
			}
		}
		if len(kept) > 0 {
			out["annotations"] = kept
		}
	}
	return out
}

func diffValue(path Path, a, b any, out *[]Change) {
	if am, ok := a.(map[string]any); ok {
		if bm, ok := b.(map[string]any); ok {
			diffMap(path, am, bm, out)
			return
		}
	}
	if al, ok := a.([]any); ok {
		if bl, ok := b.([]any); ok {
			if diffNamedList(path, al, bl, out) {
				return
			}
		}
	}
	if a == nil && b == nil {
		return
	}
	if am, ok := a.(map[string]any); ok && b == nil {
		diffMap(path, am, nil, out)
		return
	}
	if bm, ok := b.(map[string]any); ok && a == nil {
		diffMap(path, nil, bm, out)
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, Change{Path: clonePath(path), Old: a, New: b})
	}
}

func diffMap(path Path, a, b map[string]any, out *[]Change) {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	for k := range keys {
		diffValue(append(path, k), a[k], b[k], out)
	}
}

func diffNamedList(path Path, a, b []any, out *[]Change) bool {
	am, ok := indexByName(a)
	if !ok {
		return false
	}
	bm, ok := indexByName(b)
	if !ok {
		return false
	}
	names := map[string]struct{}{}
	for k := range am {
		names[k] = struct{}{}
	}
	for k := range bm {
		names[k] = struct{}{}
	}
	for n := range names {
		seg := fmt.Sprintf("[name=%s]", n)
		ea, inA := am[n]
		eb, inB := bm[n]
		switch {
		case inA && inB:
			diffValue(append(path, seg), ea, eb, out)
		default:
			*out = append(*out, Change{Path: clonePath(append(path, seg)), Old: nilIfMissing(ea, inA), New: nilIfMissing(eb, inB)})
		}
	}
	return true
}

func indexByName(items []any) (map[string]any, bool) {
	m := make(map[string]any, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := obj["name"].(string)
		if !ok {
			return nil, false
		}
		if _, dup := m[name]; dup {
			return nil, false
		}
		m[name] = obj
	}
	return m, true
}

func nilIfMissing(v any, present bool) any {
	if !present {
		return nil
	}
	return v
}

func clonePath(p Path) Path { return append(Path(nil), p...) }
