package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mary/cutroom/internal/domain"
)

// newTestClient returns an AnthropicClient pointed at a test server. The
// caller is responsible for shutting down the server.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*AnthropicClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewAnthropicClient("test-key")
	c.apiURL = srv.URL
	return c, srv
}

// captureRequest returns a handler that records the parsed request body and
// responds with the given content blocks (already JSON-encoded as the
// Anthropic Messages API would).
func captureRequest(t *testing.T, captured *anthropicRequest, responseText string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, captured); err != nil {
			t.Fatalf("decode request body: %v\nraw: %s", err, body)
		}
		resp := anthropicResponse{
			Content: []contentBlock{{Type: "text", Text: responseText}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestAnalyzeTranscript_GoldenOutput(t *testing.T) {
	canned := `{
		"suggested_cuts": [
			{"clip_name": "intro.mp4", "start": 1.5, "end": 2.0, "reason": "filler word 'um'"}
		],
		"reel_moments": [
			{"clip_name": "intro.mp4", "start": 5.0, "end": 35.0, "hook": "punchy opening line"}
		],
		"suggested_titles": ["Title A", "Title B", "Title C", "Title D", "Title E"],
		"description": "A short test description."
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	proj := &domain.Project{
		Name: "Test Project",
		Clips: []domain.Clip{{ID: "clip-1", Name: "intro.mp4"}},
	}
	analysis, err := c.AnalyzeTranscript(context.Background(), proj, "[0.0-2.0] Hello, um, world.")
	if err != nil {
		t.Fatalf("AnalyzeTranscript: %v", err)
	}

	if len(analysis.SuggestedCuts) != 1 || analysis.SuggestedCuts[0].ClipID != "clip-1" {
		t.Errorf("expected one cut mapped to clip-1, got %+v", analysis.SuggestedCuts)
	}
	if analysis.SuggestedCuts[0].Reason != "filler word 'um'" {
		t.Errorf("reason mismatch: %q", analysis.SuggestedCuts[0].Reason)
	}
	if len(analysis.ReelMoments) != 1 || analysis.ReelMoments[0].ClipID != "clip-1" {
		t.Errorf("expected reel mapped to clip-1, got %+v", analysis.ReelMoments)
	}
	if len(analysis.SuggestedTitles) != 5 {
		t.Errorf("expected 5 titles, got %d", len(analysis.SuggestedTitles))
	}
	if analysis.RawTranscript != "[0.0-2.0] Hello, um, world." {
		t.Errorf("transcript not preserved: %q", analysis.RawTranscript)
	}
}

func TestBuildManifest_GoldenOutput(t *testing.T) {
	canned := `{
		"segments": [
			{"clip_id": "clip-1", "start": 0.0, "end": 5.0, "order": 0, "description": "intro handshake"}
		],
		"title_cards": [
			{"after_segment": 0, "text": "The History", "duration": 3.0, "style": "default"}
		],
		"output_cuts": [],
		"reel_segment": {"clip_id": "clip-1", "start": 5.0, "end": 35.0}
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	proj := &domain.Project{
		Name:  "Test Project",
		Clips: []domain.Clip{{ID: "clip-1", Name: "intro.mp4", Duration: 60.0}},
	}
	manifest, err := c.BuildManifest(context.Background(), proj, "Use the intro, add a title card.")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	if len(manifest.Segments) != 1 || manifest.Segments[0].ClipID != "clip-1" {
		t.Errorf("segments mismatch: %+v", manifest.Segments)
	}
	if len(manifest.TitleCards) != 1 || manifest.TitleCards[0].Text != "The History" {
		t.Errorf("title cards mismatch: %+v", manifest.TitleCards)
	}
	if manifest.ReelSegment == nil || manifest.ReelSegment.ClipID != "clip-1" {
		t.Errorf("reel segment mismatch: %+v", manifest.ReelSegment)
	}
}

func TestBuildManifest_RequestShape(t *testing.T) {
	canned := `{"segments":[],"title_cards":[],"output_cuts":[],"reel_segment":null}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	_, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, "no-op")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	if got.Model != modelID {
		t.Errorf("model mismatch: got %q want %q", got.Model, modelID)
	}
	if got.MaxTokens != 16384 {
		t.Errorf("max_tokens mismatch: got %d want 16384", got.MaxTokens)
	}

	if got.Thinking == nil {
		t.Fatal("expected thinking field on BuildManifest request, got nil")
	}
	if got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != thinkingBudget {
		t.Errorf("thinking config mismatch: %+v", got.Thinking)
	}

	if len(got.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(got.System))
	}
	if got.System[0].CacheControl == nil {
		t.Error("expected cache_control on system block")
	} else if got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control type mismatch: %q", got.System[0].CacheControl.Type)
	}
}

func TestAnalyzeTranscript_RequestShape(t *testing.T) {
	canned := `{"suggested_cuts":[],"reel_moments":[],"suggested_titles":[],"description":""}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	_, err := c.AnalyzeTranscript(context.Background(), &domain.Project{Name: "P"}, "")
	if err != nil {
		t.Fatalf("AnalyzeTranscript: %v", err)
	}

	if got.Model != modelID {
		t.Errorf("model mismatch: got %q", got.Model)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens mismatch: got %d want 4096", got.MaxTokens)
	}
	if got.Thinking != nil {
		t.Errorf("expected no thinking field on AnalyzeTranscript, got %+v", got.Thinking)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil ||
		got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("expected cache_control: ephemeral on system block, got %+v", got.System)
	}
}

func TestComplete_SkipsThinkingBlocksAndReturnsText(t *testing.T) {
	// Anthropic returns thinking blocks before text when extended thinking
	// is enabled. The client must skip thinking and find the text block.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		resp := anthropicResponse{
			Content: []contentBlock{
				{Type: "thinking", Thinking: "let me think about this..."},
				{Type: "text", Text: `{"segments":[],"title_cards":[],"output_cuts":[],"reel_segment":null}`},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	manifest, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, "no-op")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
}

func TestStripJSONFence(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"  ```json{\"a\":1}```  ", `{"a":1}`},
	}
	for _, tc := range cases {
		if got := stripJSONFence(tc.in); got != tc.want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestComplete_ErrorOnNon200(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	})

	_, err := c.AnalyzeTranscript(context.Background(), &domain.Project{Name: "P"}, "x")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to mention 401, got: %v", err)
	}
}
