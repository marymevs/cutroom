package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mary/cutroom/internal/domain"
)

// modelID is the Claude model used for both transcript analysis and manifest
// generation. Sonnet 4.6 with extended thinking + prompt caching is the
// quality bar v2 calibrates against.
const modelID = "claude-sonnet-4-6"

// thinkingBudget is the per-request thinking-token budget for BuildManifest.
// AnalyzeTranscript runs without thinking; manifest generation needs the
// reasoning headroom on long transcripts.
const thinkingBudget = 8000

// AnthropicClient is the HTTP client for the Anthropic Messages API.
// apiURL is configurable so tests can point it at an httptest.Server.
type AnthropicClient struct {
	apiKey string
	apiURL string
	http   *http.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	return &AnthropicClient{
		apiKey: apiKey,
		apiURL: "https://api.anthropic.com/v1/messages",
		http:   http.DefaultClient,
	}
}

// contentBlock is a single block in the Messages API system or response
// content. cache_control on a block marks it as cacheable across requests.
type contentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	Thinking     string        `json:"thinking,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type thinkingConfig struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    []contentBlock  `json:"system,omitempty"`
	Messages  []message       `json:"messages"`
	Thinking  *thinkingConfig `json:"thinking,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []contentBlock `json:"content"`
}

// complete sends a request to the Messages API and returns the first text
// block from the response. Thinking blocks (when present) are skipped.
func (c *AnthropicClient) complete(ctx context.Context, req anthropicRequest) (string, error) {
	b, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.apiURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(body))
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", err
	}
	for _, block := range ar.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text block in response")
}

// stripJSONFence removes any accidental ```json ... ``` markdown fence so the
// raw response can be unmarshaled.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// AnalyzeTranscript reviews the full transcript and returns editorial suggestions.
func (c *AnthropicClient) AnalyzeTranscript(ctx context.Context, proj *domain.Project, transcript string) (*domain.Analysis, error) {
	system := []contentBlock{{
		Type: "text",
		Text: `You are an expert YouTube video editor and producer. You will be given a timestamped transcript from one or more video clips. Your job is to:
1. Identify sections to cut: filler words (um, uh, like), repeated content, pacing lags (long pauses, rambling), awkward stumbles.
2. Identify 1-3 moments that would make great Reels/Shorts (compelling hooks, punchy statements, peak energy moments).
3. Suggest 5 YouTube title options that are compelling and SEO-friendly.
4. Write a 2-3 sentence YouTube description.

Respond ONLY with a valid JSON object. No preamble, no markdown, no backticks. Schema:
{
  "suggested_cuts": [
    { "clip_name": "string", "start": 0.0, "end": 0.0, "reason": "string" }
  ],
  "reel_moments": [
    { "clip_name": "string", "start": 0.0, "end": 0.0, "hook": "string" }
  ],
  "suggested_titles": ["title1", "title2", "title3", "title4", "title5"],
  "description": "string"
}`,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}}

	user := fmt.Sprintf("Project: %s\n\nTranscript:\n%s", proj.Name, transcript)

	raw, err := c.complete(ctx, anthropicRequest{
		Model:     modelID,
		MaxTokens: 4096,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	})
	if err != nil {
		return nil, err
	}

	var result struct {
		SuggestedCuts []struct {
			ClipName string  `json:"clip_name"`
			Start    float64 `json:"start"`
			End      float64 `json:"end"`
			Reason   string  `json:"reason"`
		} `json:"suggested_cuts"`
		ReelMoments []struct {
			ClipName string  `json:"clip_name"`
			Start    float64 `json:"start"`
			End      float64 `json:"end"`
			Hook     string  `json:"hook"`
		} `json:"reel_moments"`
		SuggestedTitles []string `json:"suggested_titles"`
		Description     string   `json:"description"`
	}

	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &result); err != nil {
		return nil, fmt.Errorf("parse analysis JSON: %w\nraw: %s", err, raw)
	}

	nameToID := make(map[string]string)
	for _, c := range proj.Clips {
		nameToID[c.Name] = c.ID
	}

	analysis := &domain.Analysis{
		SuggestedTitles: result.SuggestedTitles,
		Description:     result.Description,
		RawTranscript:   transcript,
	}

	for _, sc := range result.SuggestedCuts {
		analysis.SuggestedCuts = append(analysis.SuggestedCuts, domain.SuggestedCut{
			ClipID: nameToID[sc.ClipName],
			Start:  sc.Start,
			End:    sc.End,
			Reason: sc.Reason,
		})
	}
	for _, rm := range result.ReelMoments {
		analysis.ReelMoments = append(analysis.ReelMoments, domain.ReelMoment{
			ClipID: nameToID[rm.ClipName],
			Start:  rm.Start,
			End:    rm.End,
			Hook:   rm.Hook,
		})
	}

	return analysis, nil
}

// BuildManifest takes user instructions + analysis + the user's title card
// library and builds a complete EditManifest. Runs with extended thinking
// enabled because manifest generation is the highest-stakes call in the
// pipeline — wrong cuts mean a wrong video.
//
// Library handling — the empty-library bridge:
//   - When the user has zero cards uploaded, the system prompt is gated to
//     return `title_cards: []`. There's no point asking Claude to pick
//     image_ids that don't exist.
//   - When the library has 1+ cards, Claude receives the library as part of
//     the user message and is told to pick `image_id` from those entries.
func (c *AnthropicClient) BuildManifest(ctx context.Context, proj *domain.Project, library []*domain.Card, instructions string) (*domain.EditManifest, error) {
	var clipsInfo strings.Builder
	for _, clip := range proj.Clips {
		clipsInfo.WriteString(fmt.Sprintf("- Clip: %s (ID: %s, Duration: %.1fs)\n", clip.Name, clip.ID, clip.Duration))
	}

	var analysisInfo string
	if proj.Analysis != nil {
		analysisJSON, _ := json.MarshalIndent(proj.Analysis.SuggestedCuts, "", "  ")
		analysisInfo = fmt.Sprintf("Suggested cuts from transcript analysis:\n%s", string(analysisJSON))
	}

	hasLibrary := len(library) > 0

	// The system prompt branches on whether the user has any title cards
	// uploaded. With cards: include image_id in the title_cards schema and
	// tell Claude to pick from the library. Without cards: explicitly tell
	// Claude to skip title_cards entirely so the manifest doesn't reference
	// non-existent assets (the empty-library bridge).
	var systemText string
	if hasLibrary {
		systemText = `You are a YouTube video editor. You will receive a list of clips, editorial analysis, the user's title card library, and freeform editing instructions. Produce a structured edit manifest.

Respond ONLY with valid JSON. No preamble, no markdown. Schema:
{
  "segments": [
    { "clip_id": "string", "start": 0.0, "end": 0.0, "order": 0, "description": "string" }
  ],
  "title_cards": [
    { "after_segment": 0, "image_id": "string", "duration": 3.0 }
  ],
  "output_cuts": [
    { "clip_id": "string", "start": 0.0, "end": 0.0, "description": "string" }
  ],
  "reel_segment": { "clip_id": "string", "start": 0.0, "end": 0.0 }
}

Rules:
- segments are ordered video sections to include (after removing cuts)
- title_cards are full-frame cards inserted BETWEEN segments using after_segment index
- image_id MUST be the ID of one of the cards in the user's library — never invent one
- pick a card whose name and description fit the moment; if no card fits, omit the card entirely instead of using a wrong one
- duration is in seconds (typical 2.5–4 seconds)
- output_cuts are confirmed removals (can include suggested cuts from analysis)
- reel_segment is the best 30-60s moment for a Short/Reel
- description: one short plain-English sentence (≤12 words) describing what happens in that segment or why the cut is being made. The user reads these to sanity-check the plan before rendering, so be concrete about on-screen action, not vague ("intro clip").`
	} else {
		systemText = `You are a YouTube video editor. You will receive a list of clips, editorial analysis, and freeform editing instructions. Produce a structured edit manifest.

Respond ONLY with valid JSON. No preamble, no markdown. Schema:
{
  "segments": [
    { "clip_id": "string", "start": 0.0, "end": 0.0, "order": 0, "description": "string" }
  ],
  "title_cards": [],
  "output_cuts": [
    { "clip_id": "string", "start": 0.0, "end": 0.0, "description": "string" }
  ],
  "reel_segment": { "clip_id": "string", "start": 0.0, "end": 0.0 }
}

Rules:
- segments are ordered video sections to include (after removing cuts)
- title_cards MUST be the empty array — the user has not uploaded any title cards yet, so don't reference any
- output_cuts are confirmed removals (can include suggested cuts from analysis)
- reel_segment is the best 30-60s moment for a Short/Reel
- description: one short plain-English sentence (≤12 words) describing what happens in that segment or why the cut is being made. The user reads these to sanity-check the plan before rendering, so be concrete about on-screen action, not vague ("intro clip").`
	}

	system := []contentBlock{{
		Type:         "text",
		Text:         systemText,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}}

	// Build the per-call user message. Library entries (if any) live here
	// because they vary per request and shouldn't poison the prompt cache.
	var libraryInfo string
	if hasLibrary {
		var lb strings.Builder
		lb.WriteString("Title card library (pick image_id from these):\n")
		for _, card := range library {
			lb.WriteString(fmt.Sprintf("- ID: %s | Name: %s", card.ID, card.Name))
			if card.Description != "" {
				lb.WriteString(" | Description: " + card.Description)
			}
			lb.WriteString("\n")
		}
		libraryInfo = lb.String()
	}

	user := fmt.Sprintf(`Clips:
%s

%s

%s

Editor instructions:
%s`, clipsInfo.String(), analysisInfo, libraryInfo, instructions)

	raw, err := c.complete(ctx, anthropicRequest{
		Model:     modelID,
		MaxTokens: 16384,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
		Thinking: &thinkingConfig{
			Type:         "enabled",
			BudgetTokens: thinkingBudget,
		},
	})
	if err != nil {
		return nil, err
	}

	var result struct {
		Segments []struct {
			ClipID      string  `json:"clip_id"`
			Start       float64 `json:"start"`
			End         float64 `json:"end"`
			Order       int     `json:"order"`
			Description string  `json:"description"`
		} `json:"segments"`
		TitleCards []struct {
			AfterSegment int     `json:"after_segment"`
			ImageID      string  `json:"image_id"`
			Duration     float64 `json:"duration"`
		} `json:"title_cards"`
		OutputCuts []struct {
			ClipID      string  `json:"clip_id"`
			Start       float64 `json:"start"`
			End         float64 `json:"end"`
			Description string  `json:"description"`
		} `json:"output_cuts"`
		ReelSegment *struct {
			ClipID string  `json:"clip_id"`
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
		} `json:"reel_segment"`
	}

	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &result); err != nil {
		return nil, fmt.Errorf("parse manifest JSON: %w\nraw: %s", err, raw)
	}

	manifest := &domain.EditManifest{}

	// Build a set of valid library IDs so we can defensively drop
	// hallucinated image_ids from Claude's response. Without this filter
	// a stray ID slips into the manifest and the orphan check fires at
	// render time — the orphan check IS the load-bearing safety net but
	// dropping bad refs at parse time is friendlier UX.
	validIDs := make(map[string]bool, len(library))
	for _, c := range library {
		validIDs[c.ID] = true
	}

	for _, s := range result.Segments {
		manifest.Segments = append(manifest.Segments, domain.Segment{
			ClipID:      s.ClipID,
			Start:       s.Start,
			End:         s.End,
			Order:       s.Order,
			Description: s.Description,
		})
	}
	for _, tc := range result.TitleCards {
		var imageID *string
		if tc.ImageID != "" && validIDs[tc.ImageID] {
			id := tc.ImageID
			imageID = &id
		}
		// If the model returned a card-shaped entry but the image_id is
		// blank or hallucinated, skip the entry entirely. A title card
		// with no image is meaningless.
		if imageID == nil {
			continue
		}
		manifest.TitleCards = append(manifest.TitleCards, domain.TitleCard{
			AfterSegment: tc.AfterSegment,
			ImageID:      imageID,
			Duration:     tc.Duration,
		})
	}
	for _, oc := range result.OutputCuts {
		manifest.OutputCuts = append(manifest.OutputCuts, domain.Cut{
			ClipID:      oc.ClipID,
			Start:       oc.Start,
			End:         oc.End,
			Description: oc.Description,
		})
	}
	if result.ReelSegment != nil {
		manifest.ReelSegment = &domain.ReelSegment{
			ClipID: result.ReelSegment.ClipID,
			Start:  result.ReelSegment.Start,
			End:    result.ReelSegment.End,
		}
	}

	return manifest, nil
}
