package shared

import "github.com/dillonzq/apix/internal/errclass"

// ConversionContext contains independent tool identities, request policy and
// diagnostics. Each instance belongs to one execution and is sequential-use only.
type ConversionContext struct {
	Options     ConversionOptions
	Tools       ToolRegistry
	diagnostics diagnosticCollector
}

// ToolRegistry contains only wire-name identities used to restore responses.
// It is independent of request policy and diagnostic collection.
type ToolRegistry struct {
	names map[string]responseToolIdentity
}

func NewConversionContext() *ConversionContext {
	return &ConversionContext{Tools: ToolRegistry{names: make(map[string]responseToolIdentity)}}
}

func (t *ToolRegistry) clone() ToolRegistry {
	copy := ToolRegistry{names: make(map[string]responseToolIdentity, len(t.names))}
	for name, identity := range t.names {
		copy.names[name] = identity
	}
	return copy
}

func (c *ConversionContext) Clone() *ConversionContext {
	copy := NewConversionContext()
	if c != nil {
		copy.Options = c.Options
		copy.Tools = c.Tools.clone()
		for _, d := range c.Diagnostics() {
			copy.Record(d)
		}
	}
	return copy
}

// WithToolTransaction commits identities only on success. Diagnostics survive
// failure so callers can inspect why conversion was rejected.
func (c *ConversionContext) WithToolTransaction(convert func(*ConversionContext) ([]byte, *errclass.Error)) ([]byte, *errclass.Error) {
	work := c.Clone()
	body, err := convert(work)
	for _, d := range work.Diagnostics() {
		c.Record(d)
	}
	if err == nil {
		c.Tools = work.Tools
	}
	return body, err
}

// ContextOrNew supplies temporary execution state when the caller needs no
// response identity restoration or externally observable diagnostics.
func ContextOrNew(contexts []*ConversionContext) *ConversionContext {
	for _, c := range contexts {
		if c != nil {
			return c
		}
	}
	return NewConversionContext()
}

func (c *ConversionContext) Identity(name string) responseToolIdentity {
	if c == nil {
		return responseToolIdentity{name: name}
	}
	return c.Tools.Identity(name)
}
func (c *ConversionContext) IsCustom(name string) bool { return c.Identity(name).isCustom }
func (c *ConversionContext) IsShell(name string) bool  { return c.Identity(name).isShell }
