// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package merge

import (
	"fmt"
	"strconv"
	"strings"
)

// A Path addresses one location in a decoded JSON document.
//
// The syntax is deliberately tiny: `$` for the root, `.name` to descend into an
// object, `[0]` to descend into a fixed array position. There are no wildcards,
// no filters, no expressions and no functions — a rule has to stay a data
// structure, and every one of those features is a step towards it becoming a
// language.
type Path []step

type step struct {
	name  string
	index int
	array bool
}

func (s step) String() string {
	if s.array {
		return "[" + strconv.Itoa(s.index) + "]"
	}
	return "." + s.name
}

func (p Path) String() string {
	var b strings.Builder
	b.WriteByte('$')
	for _, s := range p {
		b.WriteString(s.String())
	}
	return b.String()
}

// IsRoot reports whether the path addresses the whole document.
func (p Path) IsRoot() bool { return len(p) == 0 }

// validName reports whether s is an acceptable object key in a path.
func validName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ParsePath reads the path syntax described on Path.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, fmt.Errorf("empty path")
	}
	if s != "$" && !strings.HasPrefix(s, "$.") && !strings.HasPrefix(s, "$[") {
		return nil, fmt.Errorf("path %q must start with $", s)
	}
	rest := s[1:]

	var out Path
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			if name == "" {
				return nil, fmt.Errorf("path %q has an empty segment", s)
			}
			// Names are constrained so that anything resembling a filter or an
			// expression is refused rather than silently read as a field name:
			// "$.a?(@.b)" must not quietly become the field "a?(@".
			if !validName(name) {
				return nil, fmt.Errorf("path %q: segment %q may contain only letters, digits, '_' and '-'", s, name)
			}
			out = append(out, step{name: name})
			rest = rest[end:]

		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q has an unclosed [", s)
			}
			idx, err := strconv.Atoi(rest[1:end])
			if err != nil || idx < 0 {
				// A non-literal index would be a computed path, which is the
				// boundary between a rule and code.
				return nil, fmt.Errorf("path %q: array index must be a non-negative integer", s)
			}
			out = append(out, step{index: idx, array: true})
			rest = rest[end+1:]

		default:
			return nil, fmt.Errorf("path %q: unexpected %q", s, rest[0])
		}
	}
	return out, nil
}

// Get returns the value at the path, and whether it was present.
func (p Path) Get(root any) (any, bool) {
	cur := root
	for _, s := range p {
		if s.array {
			arr, ok := cur.([]any)
			if !ok || s.index >= len(arr) {
				return nil, false
			}
			cur = arr[s.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[s.name]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Set writes a value at the path, creating intermediate objects and arrays.
//
// The root is always an object: a rule maps into a document, not into a bare
// scalar. Arrays are grown with nils up to a fixed index, which is what makes
// `$.GpsInfo[0].Latitude` work against an absent GpsInfo.
func (p Path) Set(root map[string]any, value any) error {
	if p.IsRoot() {
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot set a non-object at the document root")
		}
		for k, v := range obj {
			root[k] = v
		}
		return nil
	}

	var cur any = root
	for i, s := range p[:len(p)-1] {
		next, err := descend(cur, s, p[i+1])
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		cur = next
	}

	last := p[len(p)-1]
	if last.array {
		arr, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%s: parent of %s is not an array", p, last)
		}
		arr[last.index] = value
		return nil
	}
	obj, ok := cur.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: parent of %s is not an object", p, last)
	}
	obj[last.name] = value
	return nil
}

// descend walks one step, creating the container the *next* step needs.
func descend(cur any, s, next step) (any, error) {
	want := func() any {
		if next.array {
			return []any{}
		}
		return map[string]any{}
	}

	if s.array {
		arr, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an array", s)
		}
		if s.index >= len(arr) {
			return nil, fmt.Errorf("%s is out of range", s)
		}
		if arr[s.index] == nil {
			arr[s.index] = want()
		}
		return arr[s.index], nil
	}

	obj, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", s)
	}
	child, ok := obj[s.name]
	if !ok || child == nil {
		child = want()
		obj[s.name] = child
	}
	// An array that has to reach a fixed index is grown here rather than at the
	// leaf, so `$.a[2].b` works against an absent or shorter `a`.
	if next.array {
		arr, ok := child.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an array", s)
		}
		for len(arr) <= next.index {
			arr = append(arr, nil)
		}
		obj[s.name] = arr
		return arr, nil
	}
	return child, nil
}

// Delete removes the value at the path if present.
func (p Path) Delete(root map[string]any) {
	if p.IsRoot() || len(p) == 0 {
		return
	}
	parent := p[:len(p)-1]
	container, ok := parent.Get(root)
	if !ok {
		return
	}
	last := p[len(p)-1]
	if last.array {
		return // removing an element would shift fixed indices; not supported
	}
	if obj, ok := container.(map[string]any); ok {
		delete(obj, last.name)
	}
}
