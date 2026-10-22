package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

const customToolParameters = `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`

// wireToolName is shared by declarations, history and tool-choice references.
func wireToolName(id responseToolIdentity) string {
	if id.namespace != "" {
		return qualifiedToolName(id.name, id.namespace)
	}
	return id.name
}

func (t *ToolRegistry) registerTool(wire string, id responseToolIdentity) *errclass.Error {
	if strings.TrimSpace(id.name) == "" {
		return errclass.Translation("Responses tool requires a name")
	}
	if prev, ok := t.names[wire]; ok && prev != id {
		return errclass.Translation("Responses tool names collide after namespace conversion")
	}
	if t.names == nil {
		t.names = make(map[string]responseToolIdentity)
	}
	t.names[wire] = id
	return nil
}

// Declaration deduplication is local to this request, separate from identities
// carried into normalization by shell adaptation or an earlier explicit pass.
func (t *ToolRegistry) declareTool(declared map[string]bool, id responseToolIdentity) (string, bool, *errclass.Error) {
	wire := wireToolName(id)
	if id.namespace == "" && !id.isCustom && t.IsShell(wire) {
		id = t.Identity(wire)
	}
	if err := t.registerTool(wire, id); err != nil {
		return "", false, err
	}
	if declared[wire] {
		return wire, false, nil
	}
	declared[wire] = true
	return wire, true, nil
}

func (t *ToolRegistry) referenceTool(id responseToolIdentity) (string, *errclass.Error) {
	wire := wireToolName(id)
	if prev, ok := t.names[wire]; ok {
		if prev.name != id.name || prev.namespace != id.namespace || id.isCustom && !prev.isCustom {
			return "", errclass.Translation("Responses tool reference conflicts with a flattened tool name")
		}
		id = prev
	}
	return wire, t.registerTool(wire, id)
}

// Identity resolves a wire tool name to its original identity. Unknown
// names (and a nil registry) pass through as a plain name.
func (t *ToolRegistry) Identity(name string) responseToolIdentity {
	if t == nil {
		return responseToolIdentity{name: name}
	}
	if id, ok := t.names[name]; ok {
		return id
	}
	return responseToolIdentity{name: name}
}

// IsCustom reports whether a wire tool name was originally declared as
// type "custom". Unknown names (and a nil registry) report false.
func (t *ToolRegistry) IsCustom(name string) bool {
	if t != nil && t.names != nil {
		if id, ok := t.names[name]; ok {
			return id.isCustom
		}
	}
	return false
}

// IsShell identifies the function used to emulate a local Responses shell.
func (t *ToolRegistry) IsShell(name string) bool { return t.Identity(name).isShell }

// qualifiedToolName flattens one namespaced child to its wire name. Names
// longer than 64 characters truncate to 50 chars plus "__" plus the first
// 6 bytes of the SHA-256 digest hex-encoded (50+2+12 = 64).
func qualifiedToolName(name, namespace string) string {
	qualified := namespace + "__" + name
	if len(qualified) > 64 {
		sum := sha256.Sum256([]byte(qualified))
		qualified = qualified[:50] + "__" + hex.EncodeToString(sum[:6])
	}
	return qualified
}

// responseToolIdentity is the original (name, namespace) pair behind one
// flattened wire name. A plain function tool carries an empty namespace.
// isCustom marks tools originally declared as type "custom" so the
// outbound path restores custom_tool_call items.
type responseToolIdentity struct {
	name      string
	namespace string
	isCustom  bool
	isShell   bool
}
