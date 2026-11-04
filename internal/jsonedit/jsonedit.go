// Package jsonedit reads and edits named fields of a JSON object without
// decoding the rest of the document. Edits keep every other byte as sent, so
// member order and number spelling survive.
//
// The package only takes the fast path where its reading is the one
// encoding/json would make. It reports ok=false, and the caller falls back to
// its encoding/json code, when the document is not valid JSON with an object
// root, when a key on a path is repeated or matches another key
// case-insensitively (encoding/json keeps the last duplicate and matches
// struct fields case-insensitively, while ojson takes the first exact match),
// or when a parent on a path holds something other than an object.
package jsonedit

import (
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Op is one edit of a member of an object.
type Op struct {
	path  []string
	value string // raw JSON; empty for a delete
	del   bool
	prune bool
}

// Set replaces or adds the member at path with the raw JSON value, creating
// missing parent objects. A new member is appended after its siblings.
func Set(value []byte, path ...string) Op {
	return Op{path: path, value: string(value)}
}

// SetString is Set for a value that is already JSON text.
func SetString(value string, path ...string) Op {
	return Op{path: path, value: value}
}

// Delete removes the member at path; a missing member is not an error.
func Delete(path ...string) Op {
	return Op{path: path, del: true}
}

// DeletePrune removes the member at path and then its parent when the parent
// is left empty. Nothing changes when the member is missing.
func DeletePrune(path ...string) Op {
	return Op{path: path, del: true, prune: true}
}

// Compare orders ops by path, for callers that build ops from a map and
// want a reproducible member order.
func (o Op) Compare(p Op) int {
	return slices.Compare(o.path, p.path)
}

// Object reports whether body is valid JSON whose root is an object, and
// returns the parsed root.
func Object(body []byte) (ojson.Result, bool) {
	if !ojson.ValidBytes(body) {
		return ojson.Result{}, false
	}
	root := ojson.ParseBytes(body)
	return root, root.IsObject()
}

// Fields returns the members of body named by keys, as encoding/json would
// read them into a struct. ok is false when the caller must decode body
// itself: body is not a valid object, or one of keys is ambiguous.
func Fields(body []byte, keys ...string) ([]ojson.Result, bool) {
	root, ok := Object(body)
	if !ok {
		return nil, false
	}
	return Members(root, keys...)
}

// Members is Fields for an already parsed object.
func Members(obj ojson.Result, keys ...string) ([]ojson.Result, bool) {
	out := make([]ojson.Result, len(keys))
	counts := make([]int, len(keys))
	exact := true
	obj.ForEach(func(key, value ojson.Result) bool {
		k := key.String()
		for i, want := range keys {
			if strings.EqualFold(k, want) {
				counts[i]++
				exact = exact && k == want
				out[i] = value
			}
		}
		return true
	})
	if !exact {
		return nil, false
	}
	for _, n := range counts {
		if n > 1 {
			return nil, false
		}
	}
	return out, true
}

// Apply applies ops to body in order and returns the edited document. It
// returns ok=false, leaving the edit to the caller's encoding/json path, under
// the conditions in the package documentation.
func Apply(body []byte, ops ...Op) (out []byte, ok bool) {
	root, ok := Object(body)
	if !ok {
		return nil, false
	}
	for _, op := range ops {
		if !unambiguous(root, op.path) {
			return nil, false
		}
	}
	doc := ojson.NewDoc(body)
	for _, op := range ops {
		path := joinPath(op.path)
		if !op.del {
			if doc.SetRaw(path, op.value) != nil {
				return nil, false
			}
			continue
		}
		if !doc.Get(path).Exists() {
			continue
		}
		if doc.Delete(path) != nil {
			return nil, false
		}
		if op.prune && len(op.path) > 1 {
			parent := joinPath(op.path[:len(op.path)-1])
			if p := doc.Get(parent); p.IsObject() && empty(p) {
				if doc.Delete(parent) != nil {
					return nil, false
				}
			}
		}
	}
	return doc.Bytes(), true
}

// unambiguous reports whether every key on path names at most one member,
// spelled exactly, and every existing parent on path is an object.
func unambiguous(root ojson.Result, path []string) bool {
	obj := root
	for i, k := range path {
		m, ok := Members(obj, k)
		if !ok {
			return false
		}
		if !m[0].Exists() {
			return true
		}
		if i < len(path)-1 {
			if !m[0].IsObject() {
				return false
			}
			obj = m[0]
		}
	}
	return true
}

func empty(obj ojson.Result) bool {
	n := 0
	obj.ForEach(func(_, _ ojson.Result) bool {
		n++
		return false
	})
	return n == 0
}

func joinPath(path []string) string {
	if len(path) == 1 {
		return ojson.Escape(path[0])
	}
	var sb strings.Builder
	for i, k := range path {
		if i > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(ojson.Escape(k))
	}
	return sb.String()
}
