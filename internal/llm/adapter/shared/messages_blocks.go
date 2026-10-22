package shared

// MessagesBlocks owns the close-before-start lifecycle and monotonically
// increasing indexes of synthesized Messages content blocks. Source adapters
// decide when tools are complete; this writer only owns output ordering.
type MessagesBlocks struct {
	next  int
	index int
	kind  string
}

func (b *MessagesBlocks) Active(kind string) bool { return b.kind == kind }
func (b *MessagesBlocks) Index() int              { return b.index }

func (b *MessagesBlocks) Start(em ClaudeEventEmitter, kind string, extra map[string]any) (int, [][]byte) {
	events := b.Close(em)
	b.index = b.next
	b.next++
	b.kind = kind
	return b.index, append(events, em.ContentBlockStart(b.index, kind, extra))
}

// Delta never emits content for a block that has already closed.
func (b *MessagesBlocks) Delta(em ClaudeEventEmitter, delta map[string]any) [][]byte {
	if b.kind == "" {
		return nil
	}
	return [][]byte{em.ContentBlockDelta(b.index, delta)}
}

func (b *MessagesBlocks) Close(em ClaudeEventEmitter) [][]byte {
	if b.kind == "" {
		return nil
	}
	b.kind = ""
	return [][]byte{em.ContentBlockStop(b.index)}
}
