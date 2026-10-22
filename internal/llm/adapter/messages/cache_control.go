package messages

import (
	"encoding/json"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Cache controls are opaque upstream hints; preserve their fields without
// adding breakpoints or validating provider-specific TTLs.
func applyCacheControl(blocks []anthropicBlock, cache json.RawMessage) {
	if len(blocks) == 0 || !shared.HasContent(cache) {
		return
	}
	last := blocks[len(blocks)-1]
	if last["type"] == "thinking" || last["type"] == "redacted_thinking" {
		return
	}
	if last["cache_control"] == nil {
		last["cache_control"] = cache
	}
}

func hasCacheControl(blocks []anthropicBlock) bool {
	for _, block := range blocks {
		if block["cache_control"] != nil {
			return true
		}
	}
	return false
}

func assistantTextBlocks(blocks []anthropicBlock) []anthropicBlock {
	if hasCacheControl(blocks) {
		return blocks
	}
	if text := shared.JoinTexts(blocks); text != "" {
		return []anthropicBlock{textBlock(text)}
	}
	return nil
}

func toolCacheControl(tool shared.CCTool) json.RawMessage {
	if shared.HasContent(tool.CacheControl) {
		return tool.CacheControl
	}
	return tool.Function.CacheControl
}

func (b *msgBuilder) applyCacheSince(start int, cache json.RawMessage) {
	if b.cur != nil && b.blocksAdded > start {
		applyCacheControl(b.cur.Content.([]anthropicBlock), cache)
	}
}

// Only a hint on the final source part can represent the tool-result boundary.
// Never emit cache_control inside tool_result.content; earlier hints are
// removed and reported by the public request loss checker.
func toolResultBlock(callID string, raw json.RawMessage, blocks []anthropicBlock) (anthropicBlock, *errclass.Error) {
	cache := shared.ToolResultCacheControl(raw)
	for _, block := range blocks {
		delete(block, "cache_control")
	}
	content, err := toolResultContent(blocks)
	if err != nil {
		return nil, err
	}
	block := anthropicBlock{"type": "tool_result", "tool_use_id": callID, "content": content}
	if shared.HasContent(cache) {
		block["cache_control"] = cache
	}
	return block, nil
}
