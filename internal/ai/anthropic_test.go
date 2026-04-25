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

func TestAnalyzeClip_PerClipShape(t *testing.T) {
	canned := `{
		"notes": "Strong intro with one filler stumble.",
		"suggested_cuts": [
			{"start": 1.5, "end": 2.0, "reason": "filler word 'um'"}
		],
		"reel_candidates": [
			{"start": 5.0, "end": 35.0, "hook": "punchy opening line"}
		]
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	clip := &domain.Clip{ID: "clip-1", Name: "intro.mp4", Duration: 60.0}
	ca, err := c.AnalyzeClip(context.Background(), clip, "[0.0-2.0] Hello, um, world.")
	if err != nil {
		t.Fatalf("AnalyzeClip: %v", err)
	}

	if ca.ClipID != "clip-1" || ca.ClipName != "intro.mp4" {
		t.Errorf("clip identity mismatch: %+v", ca)
	}
	if ca.Notes != "Strong intro with one filler stumble." {
		t.Errorf("notes mismatch: %q", ca.Notes)
	}
	if len(ca.SuggestedCuts) != 1 || ca.SuggestedCuts[0].ClipID != "clip-1" {
		t.Errorf("expected one cut stamped with clip-1, got %+v", ca.SuggestedCuts)
	}
	if ca.SuggestedCuts[0].Reason != "filler word 'um'" {
		t.Errorf("reason: %q", ca.SuggestedCuts[0].Reason)
	}
	if len(ca.ReelCandidates) != 1 || ca.ReelCandidates[0].ClipID != "clip-1" {
		t.Errorf("expected reel candidate stamped with clip-1, got %+v", ca.ReelCandidates)
	}

	// Per-clip system prompt must be cacheable so N clips don't pay N×
	// input cost on the system block.
	if len(got.System) != 1 || got.System[0].CacheControl == nil ||
		got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("expected cache_control: ephemeral on system block, got %+v", got.System)
	}
	// The user message should reference the clip name + duration so the
	// model knows what it's looking at without pulling that from the
	// system prompt (which is cached and clip-agnostic).
	user := got.Messages[0].Content
	if !strings.Contains(user, "intro.mp4") || !strings.Contains(user, "60.0s") {
		t.Errorf("user message missing clip identity: %q", user)
	}
}

func TestSynthesizeProject_PicksAcrossClips(t *testing.T) {
	canned := `{
		"suggested_titles": ["A", "B", "C", "D", "E"],
		"description": "Short and punchy.",
		"reel_moments": [
			{"clip_name": "intro.mp4", "start": 5.0, "end": 35.0, "hook": "the hook"},
			{"clip_name": "outro.mp4", "start": 100.0, "end": 130.0, "hook": "second hook"}
		]
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	proj := &domain.Project{
		Name: "Test",
		Clips: []domain.Clip{
			{ID: "c1", Name: "intro.mp4"},
			{ID: "c2", Name: "outro.mp4"},
		},
	}
	perClip := []*domain.ClipAnalysis{
		{ClipID: "c1", ClipName: "intro.mp4", Notes: "intro vibe", ReelCandidates: []domain.ReelMoment{{ClipID: "c1", Start: 5, End: 35, Hook: "the hook"}}},
		{ClipID: "c2", ClipName: "outro.mp4", Notes: "outro vibe", ReelCandidates: []domain.ReelMoment{{ClipID: "c2", Start: 100, End: 130, Hook: "second hook"}}},
	}

	titles, description, topReels, err := c.SynthesizeProject(context.Background(), proj, perClip)
	if err != nil {
		t.Fatalf("SynthesizeProject: %v", err)
	}
	if len(titles) != 5 {
		t.Errorf("expected 5 titles, got %d", len(titles))
	}
	if description != "Short and punchy." {
		t.Errorf("description: %q", description)
	}
	if len(topReels) != 2 {
		t.Fatalf("expected 2 top reels, got %d", len(topReels))
	}
	// Reel moments must be re-mapped from clip_name back to clip_id.
	if topReels[0].ClipID != "c1" || topReels[1].ClipID != "c2" {
		t.Errorf("clip_name → clip_id mapping wrong: %+v", topReels)
	}

	// User message should compactly summarize per-clip notes + reel
	// candidates — that's what the synthesis pass reads.
	user := got.Messages[0].Content
	for _, expect := range []string{"intro vibe", "outro vibe", "the hook", "second hook"} {
		if !strings.Contains(user, expect) {
			t.Errorf("user message missing %q", expect)
		}
	}
}

func TestBuildManifest_GoldenOutput(t *testing.T) {
	canned := `{
		"segments": [
			{"clip_id": "clip-1", "start": 0.0, "end": 5.0, "order": 0, "description": "intro handshake"}
		],
		"title_cards": [
			{"after_segment": 0, "image_id": "card-history", "duration": 3.0}
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
	library := []*domain.Card{{ID: "card-history", Name: "History card"}}
	manifest, err := c.BuildManifest(context.Background(), proj, library, "Use the intro, add a title card.")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	if len(manifest.Segments) != 1 || manifest.Segments[0].ClipID != "clip-1" {
		t.Errorf("segments mismatch: %+v", manifest.Segments)
	}
	if len(manifest.TitleCards) != 1 || manifest.TitleCards[0].ImageID == nil || *manifest.TitleCards[0].ImageID != "card-history" {
		t.Errorf("title cards mismatch: %+v", manifest.TitleCards)
	}
	if manifest.ReelSegment == nil || manifest.ReelSegment.ClipID != "clip-1" {
		t.Errorf("reel segment mismatch: %+v", manifest.ReelSegment)
	}
}

func TestBuildManifest_DropsHallucinatedImageIDs(t *testing.T) {
	// CRITICAL: even with a valid library context, the model can return
	// an image_id that's not in the library. The defensive filter must
	// drop those entries instead of letting them crash render later.
	canned := `{
		"segments": [{"clip_id":"c1","start":0,"end":5,"order":0,"description":"x"}],
		"title_cards": [
			{"after_segment": 0, "image_id": "card-real", "duration": 3.0},
			{"after_segment": 0, "image_id": "card-fake", "duration": 3.0}
		],
		"output_cuts": [], "reel_segment": null
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	library := []*domain.Card{{ID: "card-real", Name: "Real"}}
	manifest, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, library, "x")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(manifest.TitleCards) != 1 {
		t.Fatalf("expected 1 valid title card, got %d: %+v", len(manifest.TitleCards), manifest.TitleCards)
	}
	if manifest.TitleCards[0].ImageID == nil || *manifest.TitleCards[0].ImageID != "card-real" {
		t.Errorf("expected card-real, got %+v", manifest.TitleCards[0])
	}
}

func TestBuildManifest_EmptyLibraryGatesPromptToSkipCards(t *testing.T) {
	// CRITICAL: empty-library bridge. With zero cards uploaded the system
	// prompt must instruct the model to return an empty title_cards list,
	// AND the response parser must produce no cards even if the model
	// disobeys.
	canned := `{
		"segments": [{"clip_id":"c1","start":0,"end":5,"order":0,"description":"x"}],
		"title_cards": [
			{"after_segment": 0, "image_id": "card-fake", "duration": 3.0}
		],
		"output_cuts": [], "reel_segment": null
	}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	manifest, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, nil, "x")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(manifest.TitleCards) != 0 {
		t.Errorf("empty library: expected zero title cards even if model disobeys, got %d", len(manifest.TitleCards))
	}
	// Prompt should mention that the user has no cards uploaded.
	if !strings.Contains(got.System[0].Text, "MUST be the empty array") {
		t.Errorf("expected empty-library guard in system prompt, got: %s", got.System[0].Text)
	}
}

func TestBuildManifest_RequestShape(t *testing.T) {
	canned := `{"segments":[],"title_cards":[],"output_cuts":[],"reel_segment":null}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	_, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, nil, "no-op")
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

func TestAnalyzeClip_RequestShape(t *testing.T) {
	canned := `{"notes":"","suggested_cuts":[],"reel_candidates":[]}`
	var got anthropicRequest
	c, _ := newTestClient(t, captureRequest(t, &got, canned))

	_, err := c.AnalyzeClip(context.Background(), &domain.Clip{ID: "x", Name: "x.mp4"}, "")
	if err != nil {
		t.Fatalf("AnalyzeClip: %v", err)
	}

	if got.Model != modelID {
		t.Errorf("model mismatch: got %q", got.Model)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens mismatch: got %d want 4096", got.MaxTokens)
	}
	// Per-clip pass deliberately skips extended thinking — thinking is on
	// BuildManifest where the stakes are higher and the cost is justified.
	if got.Thinking != nil {
		t.Errorf("expected no thinking field on AnalyzeClip, got %+v", got.Thinking)
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

	manifest, err := c.BuildManifest(context.Background(), &domain.Project{Name: "P"}, nil, "no-op")
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

	_, err := c.AnalyzeClip(context.Background(), &domain.Clip{ID: "x", Name: "x.mp4"}, "x")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to mention 401, got: %v", err)
	}
}
