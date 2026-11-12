package policy

import (
	"encoding/json"
	"strings"
)

type managedEntry struct {
	Manager     string
	Subresource string
	Paths       []Path
}

// managedFields extracts metadata.managedFields from a decoded object and
// flattens each fieldsV1 tree into owned leaf paths.
func managedFields(obj map[string]any) []managedEntry {
	md, _ := obj["metadata"].(map[string]any)
	raw, _ := md["managedFields"].([]any)
	var out []managedEntry
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		e := managedEntry{}
		e.Manager, _ = m["manager"].(string)
		e.Subresource, _ = m["subresource"].(string)
		if tree, ok := m["fieldsV1"].(map[string]any); ok {
			flattenFields(nil, tree, &e.Paths)
		}
		out = append(out, e)
	}
	return out
}

func flattenFields(prefix Path, tree map[string]any, out *[]Path) {
	for k, v := range tree {
		if k == "." {
			continue
		}
		seg, ok := fieldSegment(k)
		if !ok {
			continue
		}
		p := append(clonePath(prefix), seg)
		child, _ := v.(map[string]any)
		if hasRealChildren(child) {
			flattenFields(p, child, out)
		} else {
			*out = append(*out, p)
		}
	}
}

func hasRealChildren(m map[string]any) bool {
	for k := range m {
		if k != "." {
			return true
		}
	}
	return false
}

func fieldSegment(k string) (string, bool) {
	switch {
	case strings.HasPrefix(k, "f:"):
		return k[2:], true
	case strings.HasPrefix(k, "k:"):
		var key map[string]any
		if err := json.Unmarshal([]byte(k[2:]), &key); err == nil {
			if name, ok := key["name"].(string); ok {
				return "[name=" + name + "]", true
			}
		}
		return "[*]", true
	case strings.HasPrefix(k, "v:"), strings.HasPrefix(k, "i:"):
		return "[*]", true
	}
	return "", false
}

// overlaps reports whether one path is a prefix of the other, treating "[*]"
// as matching any list element.
func overlaps(a, b Path) bool {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if !segmentMatch(a[i], b[i]) {
			return false
		}
	}
	return true
}

func segmentMatch(a, b string) bool {
	if a == b {
		return true
	}
	isList := func(s string) bool { return strings.HasPrefix(s, "[") }
	return (a == "[*]" && isList(b)) || (b == "[*]" && isList(a))
}

func (m ManagerMatch) matches(e managedEntry) bool {
	if m.Subresource != "" && m.Subresource != e.Subresource {
		return false
	}
	if prefix, ok := strings.CutSuffix(m.Manager, "*"); ok {
		return strings.HasPrefix(e.Manager, prefix)
	}
	return m.Manager == e.Manager
}

type ownershipConflict struct {
	Path        Path
	Manager     string
	Subresource string
}

func ownershipConflicts(protected []ManagerMatch, old map[string]any, changes []Change) []ownershipConflict {
	var out []ownershipConflict
	entries := managedFields(old)
	for _, c := range changes {
		if found, ok := firstOwner(protected, entries, c.Path); ok {
			out = append(out, found)
		}
	}
	return out
}

func firstOwner(protected []ManagerMatch, entries []managedEntry, p Path) (ownershipConflict, bool) {
	for _, e := range entries {
		if !isProtected(protected, e) {
			continue
		}
		for _, owned := range e.Paths {
			if overlaps(owned, p) {
				return ownershipConflict{Path: p, Manager: e.Manager, Subresource: e.Subresource}, true
			}
		}
	}
	return ownershipConflict{}, false
}

func isProtected(protected []ManagerMatch, e managedEntry) bool {
	for _, m := range protected {
		if m.matches(e) {
			return true
		}
	}
	return false
}
