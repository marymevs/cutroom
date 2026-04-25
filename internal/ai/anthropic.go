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

// AnalyzeClip reviews a SINGLE clip's transcript with full model attention.
// Replaces the old multi-clip-blob approach where one prompt covered every
// clip — there, attention was split across N sections and subtle issues in
// less-eventful clips got lost. Per-clip calls give every clip the same
// thorough review.
//
// The system prompt is cacheable across clips (and across runs of the same
// project), so prompt caching makes this nearly free on the input side.
// Only the per-clip transcript pays full token cost.
func (c *AnthropicClient) AnalyzeClip(ctx context.Context, clip *domain.Clip, transcript string) (*domain.ClipAnalysis, error) {
	system := []contentBlock{{
		Type: "text",
		Text: `You are an expert YouTube video editor reviewing a SINGLE video clip from a creator's footage. You have your full attention on just this clip — be thorough and specific.

Your job:
1. Identify sections to CUT: filler words (um, uh, like), repeated content, pacing lags (long pauses, rambling), awkward stumbles. Don't filter for project-level relevance — surface everything notable.
2. Identify reel/short candidates: 1-3 standout moments from THIS clip that could be a 30-60s Short (compelling hooks, punchy statements, peak energy). The project-level synthesis pass will pick the strongest candidates across all clips later — your job is to surface options.
3. Write a brief 1-2 sentence editorial note: what's good about this clip, what's weak, what's its likely role in the final video.

Be specific. Cite content from the transcript when explaining cuts. Use the timestamps from the transcript verbatim.

Respond ONLY with a valid JSON object. No preamble, no markdown, no backticks. Schema:
{
  "notes": "string",
  "suggested_cuts": [
    { "start": 0.0, "end": 0.0, "reason": "string" }
  ],
  "reel_candidates": [
    { "start": 0.0, "end": 0.0, "hook": "string" }
  ]
}`,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}}

	user := fmt.Sprintf("Clip: %s\nDuration: %.1fs\n\nTranscript:\n%s", clip.Name, clip.Duration, transcript)

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
		Notes         string `json:"notes"`
		SuggestedCuts []struct {
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
			Reason string  `json:"reason"`
		} `json:"suggested_cuts"`
		ReelCandidates []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Hook  string  `json:"hook"`
		} `json:"reel_candidates"`
	}

	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &result); err != nil {
		return nil, fmt.Errorf("parse clip analysis JSON for %s: %w\nraw: %s", clip.Name, err, raw)
	}

	ca := &domain.ClipAnalysis{
		ClipID:   clip.ID,
		ClipName: clip.Name,
		Notes:    result.Notes,
	}
	for _, sc := range result.SuggestedCuts {
		ca.SuggestedCuts = append(ca.SuggestedCuts, domain.SuggestedCut{
			ClipID: clip.ID,
			Start:  sc.Start,
			End:    sc.End,
			Reason: sc.Reason,
		})
	}
	for _, rc := range result.ReelCandidates {
		ca.ReelCandidates = append(ca.ReelCandidates, domain.ReelMoment{
			ClipID: clip.ID,
			Start:  rc.Start,
			End:    rc.End,
			Hook:   rc.Hook,
		})
	}
	return ca, nil
}

// SynthesizeProject takes per-clip analyses and produces project-level
// outputs: 5 title suggestions, a YouTube description, and 1-3 best reel
// picks chosen from across all clips' reel candidates. Cheap call — input
// is just per-clip notes/candidates, no full transcripts.
func (c *AnthropicClient) SynthesizeProject(ctx context.Context, proj *domain.Project, perClip []*domain.ClipAnalysis) (titles []string, description string, topReels []domain.ReelMoment, err error) {
	system := []contentBlock{{
		Type: "text",
		Text: `You are synthesizing a YouTube video from per-clip editorial analyses. The creator already analyzed each clip individually; your job is to produce project-level outputs by reading across the per-clip notes and reel candidates.

Pick:
1. 5 compelling, SEO-friendly title options for the overall video.
2. A 2-3 sentence YouTube description.
3. 1-3 reel moments chosen from across the per-clip reel candidates — pick the strongest, most distinct moments. Use the clip_name field from the input to identify which clip a moment came from. Don't invent new moments outside the candidate list.

Respond ONLY with a valid JSON object. No preamble, no markdown, no backticks. Schema:
{
  "suggested_titles": ["t1", "t2", "t3", "t4", "t5"],
  "description": "string",
  "reel_moments": [
    { "clip_name": "string", "start": 0.0, "end": 0.0, "hook": "string" }
  ]
}`,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}}

	// Build a compact per-clip summary the model can read across.
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\n", proj.Name)
	for _, ca := range perClip {
		fmt.Fprintf(&b, "[CLIP: %s]\n", ca.ClipName)
		if ca.Notes != "" {
			fmt.Fprintf(&b, "Notes: %s\n", ca.Notes)
		}
		if len(ca.ReelCandidates) > 0 {
			b.WriteString("Reel candidates:\n")
			for _, rc := range ca.ReelCandidates {
				fmt.Fprintf(&b, "  [%.2f-%.2f] %s\n", rc.Start, rc.End, rc.Hook)
			}
		}
		b.WriteString("\n")
	}

	raw, err := c.complete(ctx, anthropicRequest{
		Model:     modelID,
		MaxTokens: 2048,
		System:    system,
		Messages:  []message{{Role: "user", Content: b.String()}},
	})
	if err != nil {
		return nil, "", nil, err
	}

	var result struct {
		SuggestedTitles []string `json:"suggested_titles"`
		Description     string   `json:"description"`
		ReelMoments     []struct {
			ClipName string  `json:"clip_name"`
			Start    float64 `json:"start"`
			End      float64 `json:"end"`
			Hook     string  `json:"hook"`
		} `json:"reel_moments"`
	}
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &result); err != nil {
		return nil, "", nil, fmt.Errorf("parse synthesis JSON: %w\nraw: %s", err, raw)
	}

	nameToID := make(map[string]string, len(proj.Clips))
	for _, cl := range proj.Clips {
		nameToID[cl.Name] = cl.ID
	}
	for _, rm := range result.ReelMoments {
		topReels = append(topReels, domain.ReelMoment{
			ClipID: nameToID[rm.ClipName],
			Start:  rm.Start,
			End:    rm.End,
			Hook:   rm.Hook,
		})
	}
	return result.SuggestedTitles, result.Description, topReels, nil
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
