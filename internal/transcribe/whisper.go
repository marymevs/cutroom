package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mary/cutroom/internal/editor"
)

type WhisperClient struct {
	apiKey string
}

func NewWhisperClient(apiKey string) *WhisperClient {
	return &WhisperClient{apiKey: apiKey}
}

type whisperResponse struct {
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// Transcribe sends a video/audio file to OpenAI Whisper and returns timestamped segments.
func (c *WhisperClient) Transcribe(ctx context.Context, localPath string) ([]editor.TranscriptSegment, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	part, err := w.CreateFormFile("file", filepath.Base(localPath))
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(part, f); err != nil {
		return nil, err
	}
	w.WriteField("model", "whisper-1")
	w.WriteField("response_format", "verbose_json") // gives us timestamps
	w.WriteField("timestamp_granularities[]", "segment")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whisper %d: %s", resp.StatusCode, string(b))
	}

	var wr whisperResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, fmt.Errorf("decode whisper response: %w", err)
	}

	segments := make([]editor.TranscriptSegment, len(wr.Segments))
	for i, s := range wr.Segments {
		segments[i] = editor.TranscriptSegment{
			Start: s.Start,
			End:   s.End,
			Text:  s.Text,
		}
	}
	return segments, nil
}
