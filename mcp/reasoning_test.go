package mcp

import "testing"

func TestValidReasoningEffort(t *testing.T) {
	cases := []struct {
		provider, effort string
		want             bool
	}{
		{ProviderClaude, "max", true},
		{ProviderClaude, "xhigh", true},
		{ProviderClaude, "minimal", false}, // minimal is not in the claude set
		{ProviderOpenAI, "xhigh", true},
		{ProviderOpenAI, "minimal", true},
		{ProviderOpenAI, "max", false}, // max is not in the openai set
		{ProviderOpenAI, "bogus", false},
		{ProviderDeepSeek, "high", false}, // unknown provider for reasoning
		{ProviderClaude, "", false},
	}
	for _, c := range cases {
		if got := ValidReasoningEffort(c.provider, c.effort); got != c.want {
			t.Errorf("ValidReasoningEffort(%q,%q)=%v want %v", c.provider, c.effort, got, c.want)
		}
	}
}

func baseClient(opts ...ClientOption) *Client {
	return NewClient(opts...).(*Client)
}

func TestBuildMCPRequestBody_ReasoningEffortInjected(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithModel("gpt-5.5"), WithReasoningEffort("xhigh"))
	body := c.BuildMCPRequestBody("sys", "user")
	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("expected reasoning_effort=xhigh, got %v", body["reasoning_effort"])
	}
	// Existing fields must remain (additive only).
	if _, ok := body["temperature"]; !ok {
		t.Errorf("temperature must still be present")
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Errorf("max_completion_tokens must still be present for openai")
	}
}

func TestBuildMCPRequestBody_NoEffortUnchanged(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithModel("gpt-5.5"))
	body := c.BuildMCPRequestBody("sys", "user")
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort must be absent when effort empty")
	}
}

func TestBuildMCPRequestBody_InvalidEffortNotInjected(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithModel("gpt-5.5"), WithReasoningEffort("ultra"))
	body := c.BuildMCPRequestBody("sys", "user")
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("invalid effort must not be injected")
	}
}

func TestBuildMCPRequestBody_OtherProviderNotInjected(t *testing.T) {
	c := baseClient(WithProvider(ProviderDeepSeek), WithModel("deepseek-chat"), WithReasoningEffort("xhigh"))
	body := c.BuildMCPRequestBody("sys", "user")
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("non-openai provider must not get reasoning_effort even if set")
	}
}

func TestParseMCPResponseFull_TruncationFailsClosed(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithStrictTruncation(true))
	for _, fr := range []string{"length", "max_tokens", "max_output_tokens"} {
		body := `{"choices":[{"message":{"content":"partial"},"finish_reason":"` + fr + `"}],"usage":{}}`
		if _, err := c.ParseMCPResponseFull([]byte(body)); err == nil {
			t.Errorf("expected error on finish_reason=%s with StrictTruncation", fr)
		}
	}
}

func TestParseMCPResponseFull_TruncationIgnoredWithoutStrict(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI)) // StrictTruncation=false
	r, err := c.ParseMCPResponseFull([]byte(`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}],"usage":{}}`))
	if err != nil {
		t.Fatalf("non-strict client must not error on truncation: %v", err)
	}
	if r.Content != "partial" {
		t.Errorf("expected content 'partial', got %q", r.Content)
	}
}

func TestParseMCPResponseFull_NormalOK(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithStrictTruncation(true))
	r, err := c.ParseMCPResponseFull([]byte(`{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Content != "done" {
		t.Errorf("expected 'done', got %q", r.Content)
	}
}

func TestParseMCPResponseFull_EmptyContentFailsClosed(t *testing.T) {
	c := baseClient(WithProvider(ProviderOpenAI), WithStrictTruncation(true))
	if _, err := c.ParseMCPResponseFull([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}],"usage":{}}`)); err == nil {
		t.Fatal("expected error on empty content with StrictTruncation")
	}
}
