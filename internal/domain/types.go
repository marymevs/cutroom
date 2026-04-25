package domain

import "time"

// Card is an uploaded title-card asset (typically a Canva-designed PNG).
// Cards live in a per-user library at /cards and (in PR-5) are referenced
// from EditManifest.TitleCards via ImageID.
//
// GCSPath is the original full-res PNG; ThumbGCSPath is a 320x180 thumbnail
// generated server-side at upload time so the library grid doesn't load
// 30x10MB originals on every page view.
//
// ThumbURL is a freshly-signed read URL populated by the handler before
// rendering; it is NOT persisted (firestore:"-").
type Card struct {
	ID           string
	Name         string
	Description  string
	GCSPath      string
	ThumbGCSPath string
	Width        int
	Height       int
	CreatedAt    time.Time
	ThumbURL     string `firestore:"-"`
}

// Status values for a project
type Status string

const (
	StatusCreated       Status = "created"
	StatusUploaded      Status = "uploaded"
	StatusAnalyzing     Status = "analyzing"
	StatusAnalyzed      Status = "analyzed"
	StatusManifestReady Status = "manifest_ready"
	StatusRendering     Status = "rendering"
	StatusDone          Status = "done"
	StatusError         Status = "error"
)

// Project is the top-level unit of work.
type Project struct {
	ID         string
	Name       string
	Status     Status
	Error      string
	Clips      []Clip
	Analysis   *Analysis     // populated after Analyze()
	Manifest   *EditManifest // populated after BuildManifest()
	OutputURL  string        // GCS signed URL for final video
	ReelsURL   string        // GCS signed URL for reel clip
	CaptionURL string        // GCS signed URL for .srt file

	// ReferencedCardIDs is a denormalized index of every card referenced
	// by Manifest.TitleCards. Maintained on every manifest save so the
	// /cards delete handler can find referencing projects in O(1) via a
	// Firestore array-contains query instead of scanning every project.
	ReferencedCardIDs []string
}

// Clip is a single uploaded video file.
type Clip struct {
	ID          string
	Name        string
	GCSPath     string
	GCSURL      string
	Duration    float64 // seconds, populated after probe
	Transcript  []TranscriptSegment
}

// TranscriptSegment is a word or phrase with timestamps from Whisper.
type TranscriptSegment struct {
	Start float64
	End   float64
	Text  string
}

// Analysis is the AI editorial review of all clips.
//
// As of the per-clip analysis change: ClipAnalyses is the structured,
// per-clip detail (each clip got its own Anthropic call with full
// attention). SuggestedCuts and ReelMoments are derived for backward
// compatibility — SuggestedCuts is the flat union of every clip's cuts,
// and ReelMoments is the project-level top picks chosen by the synthesis
// pass from across all clips' reel candidates.
type Analysis struct {
	ClipAnalyses    []*ClipAnalysis
	SuggestedCuts   []SuggestedCut
	SuggestedTitles []string
	Description     string
	ReelMoments     []ReelMoment
	RawTranscript   string
}

// ClipAnalysis is the per-clip output of the editorial review pass. Each
// clip gets its own Anthropic call with the clip's transcript only, so
// the model's full attention is on just that clip — no dilution from a
// concatenated multi-clip blob.
type ClipAnalysis struct {
	ClipID         string
	ClipName       string
	Notes          string         // 1-2 sentence editorial summary of this clip
	SuggestedCuts  []SuggestedCut // cuts within THIS clip
	ReelCandidates []ReelMoment   // reel-worthy moments in THIS clip (synthesis picks the best across clips)
}

// SuggestedCut is a section Claude recommends removing.
type SuggestedCut struct {
	ClipID string
	Start  float64
	End    float64
	Reason string // "filler words", "pacing lag", "repeated content", etc.
}

// ReelMoment is a timestamp range Claude thinks would make a great short.
type ReelMoment struct {
	ClipID string
	Start  float64
	End    float64
	Hook   string // one-line description of why this works as a reel
}

// EditManifest is the full structured edit plan Claude generates from
// user instructions + analysis. FFmpeg executes against this.
type EditManifest struct {
	Segments    []Segment
	TitleCards  []TitleCard
	OutputCuts  []Cut        // confirmed cuts (from analysis or user)
	ReelSegment *ReelSegment // which part to export as a short
	CaptionFile string       // path to generated .srt
}

// Segment is a contiguous chunk of a source clip to include.
type Segment struct {
	ClipID      string
	Start       float64
	End         float64
	Order       int
	Description string // plain-English summary shown in the edit-plan UI
}

// TitleCard is a full-frame card inserted between segments. The card
// content is an uploaded PNG identified by ImageID; the legacy text-only
// path was removed in PR-5 alongside the picker UI.
//
// ImageID is a pointer because a TitleCard with no card selected (e.g. a
// freshly-deserialized manifest where the picker hasn't been touched) is
// allowed in the schema; renders fail fast in that case (orphan check).
type TitleCard struct {
	AfterSegment int     // insert after this segment index (0-based)
	ImageID      *string // pointer into the user's card library
	Duration     float64 // seconds
}

// Cut is a confirmed removal from a clip.
type Cut struct {
	ClipID      string
	Start       float64
	End         float64
	Description string // why this section was cut (filler, pacing, etc.)
}

// ReelSegment defines what to export as a vertical short.
type ReelSegment struct {
	ClipID string
	Start  float64
	End    float64
}
