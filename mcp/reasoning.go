package mcp

import (
	"fmt"
	"strings"
)

// validReasoningEfforts lists accepted reasoning/thinking effort values per
// provider (empirically validated against the live relays). Values outside the
// set are ignored by the request builders so a typo can never produce a 400.
var validReasoningEfforts = map[string]map[string]bool{
	ProviderClaude: {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	ProviderOpenAI: {"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true},
}

// ValidReasoningEffort reports whether effort is an accepted value for provider.
func ValidReasoningEffort(provider, effort string) bool {
	set, ok := validReasoningEfforts[provider]
	if !ok {
		return false
	}
	return set[effort]
}

// GuardStrictTruncation returns a non-nil error when a response must be rejected
// fail-closed: truncated is true (the caller detected a provider-specific
// truncation stop/finish reason) or the response carries no usable content.
// Reasoning-enabled trading clients use this so a partial or empty response is
// never parsed into a trading decision. reason is surfaced in the error for logs.
func GuardStrictTruncation(truncated bool, reason, content string, toolCalls []ToolCall) error {
	if truncated {
		return fmt.Errorf("response truncated (%s); failing closed to avoid acting on a partial decision", reason)
	}
	if strings.TrimSpace(content) == "" && len(toolCalls) == 0 {
		return fmt.Errorf("empty response content; failing closed")
	}
	return nil
}
