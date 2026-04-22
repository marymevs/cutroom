package main

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mary/cutroom/internal/domain"
	"github.com/mary/cutroom/internal/editor"
	"github.com/mary/cutroom/internal/gcs"
	"github.com/mary/cutroom/internal/store"
)

// templateFuncs are helpers exposed to every template set.
var templateFuncs = template.FuncMap{
	// clipName resolves a clip UUID to its uploaded filename for display.
	// Falls back to the ID itself if the clip is missing (stale manifest ref).
	"clipName": func(clips []domain.Clip, id string) string {
		for _, c := range clips {
			if c.ID == id {
				return c.Name
			}
		}
		return id
	},
	"add1": func(i int) int { return i + 1 },
}

func renderTemplate(w http.ResponseWriter, t *template.Template, name string, data any) {
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %q execution error: %v", name, err)
	}
}

type Handler struct {
	pipeline *editor.Pipeline
	gcs      *gcs.Client
	store    *store.ProjectStore
	mu       sync.RWMutex
	projects map[string]*domain.Project   // in-memory cache of Firestore docs
	pages    map[string]*template.Template // page name -> layout+page set
	partials *template.Template            // partials rendered standalone (HTMX)
}

func NewHandler(pipeline *editor.Pipeline, gcsClient *gcs.Client, projectStore *store.ProjectStore) *Handler {
	partialFiles, err := filepath.Glob("web/templates/*_partial.html")
	if err != nil {
		panic(err)
	}
	pageNames := []string{"index.html", "project.html"}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, p := range pageNames {
		files := append([]string{
			"web/templates/layout.html",
			"web/templates/" + p,
		}, partialFiles...)
		pages[p] = template.Must(template.New(p).Funcs(templateFuncs).ParseFiles(files...))
	}
	partials := template.Must(template.New("partials").Funcs(templateFuncs).ParseFiles(partialFiles...))
	return &Handler{
		pipeline: pipeline,
		gcs:      gcsClient,
		store:    projectStore,
		projects: make(map[string]*domain.Project),
		pages:    pages,
		partials: partials,
	}
}

// lookup returns the project from the in-memory cache, falling back to
// Firestore and warming the cache on a hit. Returns (nil, nil) if the
// project does not exist anywhere.
func (h *Handler) lookup(ctx context.Context, id string) (*domain.Project, error) {
	h.mu.RLock()
	p, ok := h.projects[id]
	h.mu.RUnlock()
	if ok {
		return p, nil
	}

	p, err := h.store.Get(ctx, id)
	if err != nil || p == nil {
		return p, err
	}

	h.mu.Lock()
	if existing, ok := h.projects[id]; ok {
		// Another request beat us to the cache; prefer the existing pointer
		// so goroutines writing to it keep targeting the live object.
		p = existing
	} else {
		h.projects[id] = p
	}
	h.mu.Unlock()
	return p, nil
}

func (h *Handler) cache(p *domain.Project) {
	h.mu.Lock()
	h.projects[p.ID] = p
	h.mu.Unlock()
}

// persistAsync saves a project without blocking the caller. Used after
// background goroutines (Analyze, Render) finish mutating the project.
func (h *Handler) persistAsync(p *domain.Project) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.store.Save(ctx, p); err != nil {
		log.Printf("persist project %s: %v", p.ID, err)
	}
}

// loadOr404 is a common prologue: look up the project, 404 on miss,
// 500 on store error.
func (h *Handler) loadOr404(w http.ResponseWriter, r *http.Request) *domain.Project {
	id := chi.URLParam(r, "id")
	p, err := h.lookup(r.Context(), id)
	if err != nil {
		http.Error(w, "load project: "+err.Error(), http.StatusInternalServerError)
		return nil
	}
	if p == nil {
		http.NotFound(w, r)
		return nil
	}
	return p
}

func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	projects, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, "list projects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	renderTemplate(w, h.pages["index.html"], "layout.html", map[string]any{
		"projects": projects,
	})
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := r.FormValue("name")
	if name == "" {
		name = "Untitled Project"
	}

	p := &domain.Project{
		ID:     uuid.New().String(),
		Name:   name,
		Status: domain.StatusCreated,
		Clips:  []domain.Clip{},
	}

	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "create project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.cache(p)

	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}
	renderTemplate(w, h.pages["project.html"], "layout.html", p)
}

// SignClipUpload returns a time-limited URL the browser PUTs the file to directly,
// so video bytes never pass through Cloud Run (which caps request bodies at 32 MiB).
// The browser follows up with RegisterClip once the PUT succeeds.
func (h *Handler) SignClipUpload(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	var req struct {
		Filename    string `json:"filename"`
		ContentType string `json:"contentType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Filename == "" {
		http.Error(w, "missing filename or contentType", http.StatusBadRequest)
		return
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	objectName := "projects/" + p.ID + "/clips/" + uuid.New().String() + "-" + filepath.Base(req.Filename)
	url, err := h.gcs.SignedUploadURL(objectName, req.ContentType)
	if err != nil {
		http.Error(w, "sign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"uploadURL":   url,
		"objectName":  objectName,
		"contentType": req.ContentType,
	})
}

// RegisterClip records a clip on the project after the browser has PUT the bytes
// directly to GCS. It re-signs a read URL and appends to the clip list.
func (h *Handler) RegisterClip(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	var req struct {
		Filename   string `json:"filename"`
		ObjectName string `json:"objectName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ObjectName == "" {
		http.Error(w, "missing objectName", http.StatusBadRequest)
		return
	}

	readURL, err := h.gcs.ReadSignedURL(req.ObjectName)
	if err != nil {
		http.Error(w, "sign read URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	name := req.Filename
	if name == "" {
		name = filepath.Base(req.ObjectName)
	}
	p.Clips = append(p.Clips, domain.Clip{
		ID:       uuid.New().String(),
		Name:     name,
		GCSPath:  req.ObjectName,
		GCSURL:   readURL,
		Duration: 0,
	})
	p.Status = domain.StatusUploaded

	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "save project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	renderTemplate(w, h.partials, "clips_partial.html", p)
}

// AnalyzeClips transcribes all clips and runs AI editorial analysis.
func (h *Handler) AnalyzeClips(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	// Guard against duplicate submits (double-click, HTMX retry) spawning
	// concurrent analyze goroutines that would race on the same local files.
	if p.Status == domain.StatusAnalyzing {
		renderTemplate(w, h.partials, "analysis_status_partial.html", p)
		return
	}

	p.Status = domain.StatusAnalyzing
	p.Error = ""
	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "save project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		if err := h.pipeline.Analyze(p); err != nil {
			p.Status = domain.StatusError
			p.Error = err.Error()
		}
		h.persistAsync(p)
	}()

	renderTemplate(w, h.partials, "analysis_status_partial.html", p)
}

// GetAnalysisStatus returns an HTML fragment reflecting the current analyze state.
// HTMX polls this while status == "analyzing"; once the goroutine flips status
// to "analyzed" or "error", this swaps the polling shell out for the final UI.
func (h *Handler) GetAnalysisStatus(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}
	renderTemplate(w, h.partials, "analysis_status_partial.html", p)
}

// SubmitInstructions parses free-text edit instructions into a manifest via Claude.
func (h *Handler) SubmitInstructions(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	r.ParseForm()
	instructions := r.FormValue("instructions")

	manifest, err := h.pipeline.BuildManifest(r.Context(), p, instructions)
	if err != nil {
		http.Error(w, "failed to build edit manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p.Manifest = manifest
	p.Status = domain.StatusManifestReady

	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "save project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	renderTemplate(w, h.partials, "manifest_partial.html", p)
}

// UpdateManifest replaces the project's edit plan with user-edited values
// submitted from the plan-review form. The form uses parallel-array fields
// (seg_start[], seg_end[], seg_description[], …) so each row can be deleted
// client-side by removing its DOM element before submit.
func (h *Handler) UpdateManifest(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}
	if p.Manifest == nil {
		http.Error(w, "no manifest to update", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	manifest := &domain.EditManifest{
		CaptionFile: p.Manifest.CaptionFile,
	}

	segClipIDs := r.Form["seg_clip_id"]
	segStarts := r.Form["seg_start"]
	segEnds := r.Form["seg_end"]
	segDescs := r.Form["seg_description"]
	for i, clipID := range segClipIDs {
		manifest.Segments = append(manifest.Segments, domain.Segment{
			ClipID:      clipID,
			Start:       parseFloatAt(segStarts, i),
			End:         parseFloatAt(segEnds, i),
			Order:       i + 1,
			Description: getAt(segDescs, i),
		})
	}

	tcAfter := r.Form["tc_after_segment"]
	tcText := r.Form["tc_text"]
	tcDur := r.Form["tc_duration"]
	tcStyle := r.Form["tc_style"]
	for i := range tcText {
		manifest.TitleCards = append(manifest.TitleCards, domain.TitleCard{
			AfterSegment: parseIntAt(tcAfter, i),
			Text:         tcText[i],
			Duration:     parseFloatAt(tcDur, i),
			Style:        getAt(tcStyle, i),
		})
	}

	cutClipIDs := r.Form["cut_clip_id"]
	cutStarts := r.Form["cut_start"]
	cutEnds := r.Form["cut_end"]
	cutDescs := r.Form["cut_description"]
	for i, clipID := range cutClipIDs {
		manifest.OutputCuts = append(manifest.OutputCuts, domain.Cut{
			ClipID:      clipID,
			Start:       parseFloatAt(cutStarts, i),
			End:         parseFloatAt(cutEnds, i),
			Description: getAt(cutDescs, i),
		})
	}

	if reelClip := r.FormValue("reel_clip_id"); reelClip != "" {
		manifest.ReelSegment = &domain.ReelSegment{
			ClipID: reelClip,
			Start:  parseFloatAt(r.Form["reel_start"], 0),
			End:    parseFloatAt(r.Form["reel_end"], 0),
		}
	}

	p.Manifest = manifest
	p.Status = domain.StatusManifestReady
	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "save project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	renderTemplate(w, h.partials, "manifest_partial.html", p)
}

func getAt(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return s[i]
}

func parseFloatAt(s []string, i int) float64 {
	f, _ := strconv.ParseFloat(getAt(s, i), 64)
	return f
}

func parseIntAt(s []string, i int) int {
	n, _ := strconv.Atoi(getAt(s, i))
	return n
}

// RenderVideo executes the edit manifest through FFmpeg.
func (h *Handler) RenderVideo(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	// Guard against duplicate submits (double-click, HTMX retry) spawning
	// concurrent render goroutines that would race on the same local files.
	if p.Status == domain.StatusRendering {
		renderTemplate(w, h.partials, "status_partial.html", p)
		return
	}

	p.Status = domain.StatusRendering
	p.Error = ""
	if err := h.store.Save(r.Context(), p); err != nil {
		http.Error(w, "save project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		if err := h.pipeline.Render(p); err != nil {
			p.Status = domain.StatusError
			p.Error = err.Error()
		} else {
			p.Status = domain.StatusDone
		}
		h.persistAsync(p)
	}()

	renderTemplate(w, h.partials, "status_partial.html", p)
}

func (h *Handler) GetStatus(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}
	renderTemplate(w, h.partials, "status_partial.html", p)
}

func (h *Handler) DownloadResult(w http.ResponseWriter, r *http.Request) {
	p := h.loadOr404(w, r)
	if p == nil {
		return
	}

	what := r.URL.Query().Get("type") // "video", "reel", "captions"
	var url string
	switch what {
	case "reel":
		url = p.ReelsURL
	case "captions":
		url = p.CaptionURL
	default:
		url = p.OutputURL
	}

	if url == "" {
		http.Error(w, "not ready", http.StatusNotFound)
		return
	}

	_ = filepath.Base(url)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}
