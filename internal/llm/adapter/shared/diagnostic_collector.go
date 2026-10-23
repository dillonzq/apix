package shared

type diagnosticCollector struct {
	items []Diagnostic
	seen  map[Diagnostic]struct{}
}

func (c *ConversionContext) Diagnostics() []Diagnostic {
	if c == nil {
		return nil
	}
	return append([]Diagnostic(nil), c.diagnostics.items...)
}

func (c *ConversionContext) Record(d Diagnostic) {
	if c == nil {
		return
	}
	if c.diagnostics.seen == nil {
		c.diagnostics.seen = make(map[Diagnostic]struct{})
	}
	if _, exists := c.diagnostics.seen[d]; exists {
		return
	}
	c.diagnostics.seen[d] = struct{}{}
	c.diagnostics.items = append(c.diagnostics.items, d)
}
