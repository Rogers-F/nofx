package provider

import (
	"testing"

	"nofx/mcp"
)

func claudeClient(opts ...mcp.ClientOption) *ClaudeClient {
	return NewClaudeClientWithOptions(opts...).(*ClaudeClient)
}

func TestClaudeBuildBody_ThinkingInjection(t *testing.T) {
	c := claudeClient(mcp.WithReasoningEffort("max"))
	body := c.BuildMCPRequestBody("sys", "user")

	th, ok := body["thinking"].(map[string]any)
	if !ok || th["type"] != "adaptive" {
		t.Fatalf("expected thinking={type:adaptive}, got %v", body["thinking"])
	}
	oc, ok := body["output_config"].(map[string]any)
	if !ok || oc["effort"] != "max" {
		t.Fatalf("expected output_config={effort:max}, got %v", body["output_config"])
	}
	// this model rejects the temperature parameter; this path must never send it.
	if _, ok := body["temperature"]; ok {
		t.Errorf("claude path must not send temperature")
	}
}

func TestClaudeBuildBody_NoEffortUnchanged(t *testing.T) {
	c := claudeClient()
	body := c.BuildMCPRequestBody("sys", "user")
	if _, ok := body["thinking"]; ok {
		t.Errorf("thinking must be absent when effort empty")
	}
	if _, ok := body["output_config"]; ok {
		t.Errorf("output_config must be absent when effort empty")
	}
}

func TestClaudeBuildBody_InvalidEffortNotInjected(t *testing.T) {
	c := claudeClient(mcp.WithReasoningEffort("ultra"))
	body := c.BuildMCPRequestBody("sys", "user")
	if _, ok := body["thinking"]; ok {
		t.Errorf("invalid effort must not inject thinking")
	}
	if _, ok := body["output_config"]; ok {
		t.Errorf("invalid effort must not inject output_config")
	}
}

func TestClaudeParse_TruncationFailsClosed(t *testing.T) {
	c := claudeClient(mcp.WithReasoningEffort("max"), mcp.WithStrictTruncation(true))
	body := `{"content":[{"type":"text","text":"part"}],"stop_reason":"max_tokens","usage":{}}`
	if _, err := c.ParseMCPResponseFull([]byte(body)); err == nil {
		t.Fatal("expected error on stop_reason=max_tokens with StrictTruncation")
	}
}

func TestClaudeParse_ConcatTextBlocks(t *testing.T) {
	c := claudeClient(mcp.WithStrictTruncation(true))
	body := `{"content":[{"type":"thinking","text":""},{"type":"text","text":"a"},{"type":"text","text":"b"}],"stop_reason":"end_turn","usage":{}}`
	r, err := c.ParseMCPResponseFull([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Content != "ab" {
		t.Errorf("expected concatenated 'ab', got %q", r.Content)
	}
}

func TestClaudeParse_EmptyFailsClosed(t *testing.T) {
	c := claudeClient(mcp.WithStrictTruncation(true))
	body := `{"content":[{"type":"thinking","text":""}],"stop_reason":"end_turn","usage":{}}`
	if _, err := c.ParseMCPResponseFull([]byte(body)); err == nil {
		t.Fatal("expected error on empty text content with StrictTruncation")
	}
}
