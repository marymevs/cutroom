package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mary/cutroom/internal/editor"
)

type AnthropicClient struct {
	apiKey string
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	return &AnthropicClient{apiKey: apiKey}
}

type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *AnthropicClient) complete(ctx context.Context, system, user string) (string, error) {
	req := anthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 4096,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	}

	b, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(b))
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", err
	}
	if len(ar.Content) == 0 {
		return "", fmt.Errorf("empty response")
	}
	return ar.Content[0].Text, nil
}

// AnalyzeTranscript reviews the full transcript and returns editorial suggestions.
func (c *AnthropicClient) AnalyzeTranscript(ctx context.Context, proj *editor.Project, transcript string) (*editor.Analysis, error) {
	system := `You are an expert YouTube video editor and producer. You will be given a timestamped transcript from one or more video clips. Your job is to:
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
}`

	user := fmt.Sprintf("Project: %s\n\nTranscript:\n%s", proj.Name, transcript)

	raw, err := c.complete(ctx, system, user)
	if err != nil {
		return nil, err
	}

	// Strip any accidental markdown fences
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

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

	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse analysis JSON: %w\nraw: %s", err, raw)
	}

	// Map clip names back to clip IDs
	nameToID := make(map[string]string)
	for _, c := range proj.Clips {
		nameToID[c.Name] = c.ID
	}

	analysis := &editor.Analysis{
		SuggestedTitles: result.SuggestedTitles,
		Description:     result.Description,
		RawTranscript:   transcript,
	}

	for _, sc := range result.SuggestedCuts {
		analysis.SuggestedCuts = append(analysis.SuggestedCuts, editor.SuggestedCut{
			ClipID: nameToID[sc.ClipName],
			Start:  sc.Start,
			End:    sc.End,
			Reason: sc.Reason,
		})
	}
	for _, rm := range result.ReelMoments {
		analysis.ReelMoments = append(analysis.ReelMoments, editor.ReelMoment{
			ClipID: nameToID[rm.ClipName],
			Start:  rm.Start,
			End:    rm.End,
			Hook:   rm.Hook,
		})
	}

	return analysis, nil
}

// BuildManifest takes user instructions + analysis and builds a complete EditManifest.
func (c *AnthropicClient) BuildManifest(ctx context.Context, proj *editor.Project, instructions string) (*editor.EditManifest, error) {
	// Summarize what we know about the project for context
	var clipsInfo strings.Builder
	for _, clip := range proj.Clips {
		clipsInfo.WriteString(fmt.Sprintf("- Clip: %s (ID: %s, Duration: %.1fs)\n", clip.Name, clip.ID, clip.Duration))
	}

	var analysisInfo string
	if proj.Analysis != nil {
		analysisJSON, _ := json.MarshalIndent(proj.Analysis.SuggestedCuts, "", "  ")
		analysisInfo = fmt.Sprintf("Suggested cuts from transcript analysis:\n%s", string(analysisJSON))
	}

	system := `You are a YouTube video editor. You will receive a list of clips, editorial analysis, and freeform editing instructions. Produce a structured edit manifest.

Respond ONLY with valid JSON. No preamble, no markdown. Schema:
{
  "segments": [
    { "clip_id": "string", "start": 0.0, "end": 0.0, "order": 0 }
  ],
  "title_cards": [
    { "after_segment": 0, "text": "string", "duration": 3.0, "style": "default" }
  ],
  "output_cuts": [
    { "clip_id": "string", "start": 0.0, "end": 0.0 }
  ],
  "reel_segment": { "clip_id": "string", "start": 0.0, "end": 0.0 }
}

Rules:
- segments are ordered video sections to include (after removing cuts)
- title_cards are inserted BETWEEN segments using after_segment index
- output_cuts are confirmed removals (can include suggested cuts from analysis)
- reel_segment is the best 30-60s moment for a Short/Reel
- style options: "default" (white text, black bg), "minimal" (small caps), "bold" (large centered)`

	user := fmt.Sprintf(`Clips:
%s

%s

Editor instructions:
%s`, clipsInfo.String(), analysisInfo, instructions)

	raw, err := c.complete(ctx, system, user)
	if err != nil {
		return nil, err
	}

	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var result struct {
		Segments []struct {
			ClipID string  `json:"clip_id"`
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
			Order  int     `json:"order"`
		} `json:"segments"`
		TitleCards []struct {
			AfterSegment int     `json:"after_segment"`
			Text         string  `json:"text"`
			Duration     float64 `json:"duration"`
			Style        string  `json:"style"`
		} `json:"title_cards"`
		OutputCuts []struct {
			ClipID string  `json:"clip_id"`
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
		} `json:"output_cuts"`
		ReelSegment *struct {
			ClipID string  `json:"clip_id"`
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
		} `json:"reel_segment"`
	}

	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse manifest JSON: %w\nraw: %s", err, raw)
	}

	manifest := &editor.EditManifest{}

	for _, s := range result.Segments {
		manifest.Segments = append(manifest.Segments, editor.Segment{
			ClipID: s.ClipID,
			Start:  s.Start,
			End:    s.End,
			Order:  s.Order,
		})
	}
	for _, tc := range result.TitleCards {
		manifest.TitleCards = append(manifest.TitleCards, editor.TitleCard{
			AfterSegment: tc.AfterSegment,
			Text:         tc.Text,
			Duration:     tc.Duration,
			Style:        tc.Style,
		})
	}
	for _, oc := range result.OutputCuts {
		manifest.OutputCuts = append(manifest.OutputCuts, editor.Cut{
			ClipID: oc.ClipID,
			Start:  oc.Start,
			End:    oc.End,
		})
	}
	if result.ReelSegment != nil {
		manifest.ReelSegment = &editor.ReelSegment{
			ClipID: result.ReelSegment.ClipID,
			Start:  result.ReelSegment.Start,
			End:    result.ReelSegment.End,
		}
	}

	return manifest, nil
}
