package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mary/cutroom/internal/domain"
)

// ─── fakes ───────────────────────────────────────────────────────────────

type fakeUploader struct {
	uploaded map[string][]byte
	signErr  bool
}

func newFakeUploader() *fakeUploader {
	return &fakeUploader{uploaded: make(map[string][]byte)}
}

func (f *fakeUploader) UploadBytes(ctx context.Context, objectName string, data []byte, contentType string) (string, error) {
	cp := make([]byte, len(data))
	copy(cp, data)
	f.uploaded[objectName] = cp
	return "https://fake/" + objectName, nil
}

func (f *fakeUploader) SignedReadURL(objectName string) (string, error) {
	if f.signErr {
		return "", errors.New("sign error")
	}
	return "https://signed/" + objectName, nil
}

type fakeStore struct {
	cards map[string]*domain.Card
}

func newFakeStore() *fakeStore { return &fakeStore{cards: make(map[string]*domain.Card)} }

func (s *fakeStore) Save(ctx context.Context, c *domain.Card) error {
	cp := *c
	s.cards[c.ID] = &cp
	return nil
}

func (s *fakeStore) Get(ctx context.Context, id string) (*domain.Card, error) {
	c, ok := s.cards[id]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (s *fakeStore) List(ctx context.Context) ([]*domain.Card, error) {
	out := make([]*domain.Card, 0, len(s.cards))
	for _, c := range s.cards {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeStore) Delete(ctx context.Context, id string) error {
	delete(s.cards, id)
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────

// makePNGBytes builds a synthetic PNG of the given dimensions, all-red.
func makePNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func multipartWith(t *testing.T, fields map[string]string, fileField, fileName string, fileBytes []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if fileField != "" {
		fw, err := mw.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(fw, bytes.NewReader(fileBytes)); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	mw.Close()
	return body, mw.FormDataContentType()
}

// loadTemplates loads the actual cards templates from disk so the tests
// exercise real rendering. Path is relative to this test file's package
// (cmd/server).
func loadTemplates(t *testing.T) (*template.Template, *template.Template) {
	t.Helper()
	partialFiles, err := filepath.Glob("../../web/templates/*_partial.html")
	if err != nil {
		t.Fatal(err)
	}
	pageFiles := append([]string{
		"../../web/templates/layout.html",
		"../../web/templates/cards.html",
	}, partialFiles...)
	pages := template.Must(template.New("cards.html").Funcs(templateFuncs).ParseFiles(pageFiles...))
	partials := template.Must(template.New("partials").Funcs(templateFuncs).ParseFiles(partialFiles...))
	return pages, partials
}

func newTestHandler(t *testing.T) (*CardsHandler, *fakeStore, *fakeUploader, chi.Router) {
	t.Helper()
	pages, partials := loadTemplates(t)
	store := newFakeStore()
	upl := newFakeUploader()
	h := NewCardsHandler(store, upl, pages, partials)
	r := chi.NewRouter()
	r.Get("/cards", h.CardsPage)
	r.Get("/cards/grid", h.CardsGrid)
	r.Post("/cards", h.UploadCard)
	r.Delete("/cards/{id}", h.DeleteCard)
	return h, store, upl, r
}

// ─── tests ───────────────────────────────────────────────────────────────

func TestUploadCard_HappyPath_LandscapePNG(t *testing.T) {
	_, store, upl, router := newTestHandler(t)

	body, contentType := multipartWith(t,
		map[string]string{"name": "My Card", "description": "Cooking series intro"},
		"file", "intro.png",
		makePNGBytes(t, 1920, 1080))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "My Card") {
		t.Errorf("expected tile to contain card name, got: %s", w.Body.String())
	}
	if len(store.cards) != 1 {
		t.Fatalf("expected 1 card stored, got %d", len(store.cards))
	}
	var saved *domain.Card
	for _, c := range store.cards {
		saved = c
	}
	if saved.Name != "My Card" {
		t.Errorf("name: got %q want %q", saved.Name, "My Card")
	}
	if saved.Width != 1920 || saved.Height != 1080 {
		t.Errorf("dims: %dx%d", saved.Width, saved.Height)
	}
	if saved.GCSPath == "" || saved.ThumbGCSPath == "" {
		t.Errorf("missing GCS paths: %+v", saved)
	}
	// Both original AND thumbnail uploaded.
	if _, ok := upl.uploaded[saved.GCSPath]; !ok {
		t.Errorf("original not uploaded: paths=%v", keysOf(upl.uploaded))
	}
	if _, ok := upl.uploaded[saved.ThumbGCSPath]; !ok {
		t.Errorf("thumbnail not uploaded: paths=%v", keysOf(upl.uploaded))
	}
}

func TestUploadCard_DefaultsNameFromFilename(t *testing.T) {
	_, store, _, router := newTestHandler(t)

	body, contentType := multipartWith(t, nil, "file", "Title Card - Episode 3.png",
		makePNGBytes(t, 1920, 1080))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var saved *domain.Card
	for _, c := range store.cards {
		saved = c
	}
	if saved == nil {
		t.Fatal("nothing saved")
	}
	if saved.Name != "Title Card - Episode 3" {
		t.Errorf("name default: got %q want %q", saved.Name, "Title Card - Episode 3")
	}
}

func TestUploadCard_RejectsNonPNG(t *testing.T) {
	_, store, _, router := newTestHandler(t)

	body, contentType := multipartWith(t, nil, "file", "fake.png",
		[]byte("this is plain text, not a PNG"))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Not a PNG") {
		t.Errorf("expected 'Not a PNG' message, got: %s", w.Body.String())
	}
	if len(store.cards) != 0 {
		t.Error("nothing should have been stored on rejection")
	}
}

func TestUploadCard_RejectsOversizedFile(t *testing.T) {
	_, _, _, router := newTestHandler(t)

	// 11 MB of valid-prefix-but-otherwise-junk bytes. We only need the
	// first 8 to be PNG magic so the handler gets past the magic check
	// and trips on the size check.
	big := make([]byte, 11*1024*1024)
	copy(big, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	body, contentType := multipartWith(t, nil, "file", "big.png", big)

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "too large") {
		t.Errorf("expected 'too large' message, got: %s", w.Body.String())
	}
}

func TestUploadCard_RejectsOversizedDimensions(t *testing.T) {
	_, _, _, router := newTestHandler(t)

	// 4097 wide is just over MaxDimension. Use a tiny height to keep the
	// generated PNG small in test memory.
	body, contentType := multipartWith(t, nil, "file", "huge.png",
		makePNGBytes(t, 4097, 100))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "too big") {
		t.Errorf("expected 'too big' message, got: %s", w.Body.String())
	}
}

func TestUploadCard_PortraitWarnsBeforeCommit(t *testing.T) {
	// CRITICAL: portrait Canva exports must trigger the warn UI BEFORE
	// committing. Otherwise the user uploads a tall card, opens the
	// rendered video, sees a tiny postage stamp, loses trust.
	_, store, upl, router := newTestHandler(t)

	body, contentType := multipartWith(t,
		map[string]string{"name": "Portrait Card"},
		"file", "tall.png",
		makePNGBytes(t, 540, 960))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "card-warn") {
		t.Errorf("expected warn UI, got: %s", w.Body.String())
	}
	if len(store.cards) != 0 {
		t.Errorf("portrait must not commit before force=1, got %d cards", len(store.cards))
	}
	if len(upl.uploaded) != 0 {
		t.Errorf("portrait must not upload before force=1, got %d objects", len(upl.uploaded))
	}
}

func TestUploadCard_PortraitWithForceCommits(t *testing.T) {
	_, store, upl, router := newTestHandler(t)

	body, contentType := multipartWith(t,
		map[string]string{"name": "Portrait Card", "force": "1"},
		"file", "tall.png",
		makePNGBytes(t, 540, 960))

	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "card-warn") {
		t.Error("force=1 must skip the warn UI")
	}
	if len(store.cards) != 1 {
		t.Fatalf("force=1 must commit, got %d cards", len(store.cards))
	}
	if len(upl.uploaded) != 2 {
		t.Errorf("expected original + thumb uploaded, got %d", len(upl.uploaded))
	}
}

func TestUploadCard_MissingFileFieldReturns400(t *testing.T) {
	_, _, _, router := newTestHandler(t)

	body, contentType := multipartWith(t, map[string]string{"name": "no file"},
		"", "", nil)
	req := httptest.NewRequest("POST", "/cards", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

func TestDeleteCard_RemovesCard(t *testing.T) {
	_, store, _, router := newTestHandler(t)

	store.cards["abc"] = &domain.Card{
		ID: "abc", Name: "Test", GCSPath: "cards/abc.png",
		ThumbGCSPath: "cards/abc_thumb.png", Width: 1920, Height: 1080,
		CreatedAt: time.Now().UTC(),
	}

	req := httptest.NewRequest("DELETE", "/cards/abc", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status: got %d want 204", w.Code)
	}
	if _, ok := store.cards["abc"]; ok {
		t.Error("card still in store after delete")
	}
}

func TestDeleteCard_UnknownIDIsIdempotent(t *testing.T) {
	_, _, _, router := newTestHandler(t)

	req := httptest.NewRequest("DELETE", "/cards/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status: got %d want 204 (idempotent), body=%s", w.Code, w.Body.String())
	}
}

func TestCardsGrid_RendersEmptyState(t *testing.T) {
	_, _, _, router := newTestHandler(t)

	req := httptest.NewRequest("GET", "/cards/grid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Upload your first title card") {
		t.Errorf("expected empty-state copy, got: %s", w.Body.String())
	}
}

func TestCardsGrid_RendersCardsWithSignedThumbURLs(t *testing.T) {
	_, store, _, router := newTestHandler(t)

	store.cards["abc"] = &domain.Card{
		ID: "abc", Name: "Cooking Intro",
		GCSPath: "cards/abc.png", ThumbGCSPath: "cards/abc_thumb.png",
		Width: 1920, Height: 1080, CreatedAt: time.Now().UTC(),
	}

	req := httptest.NewRequest("GET", "/cards/grid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status: got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Cooking Intro") {
		t.Errorf("expected card name, got: %s", body)
	}
	if !strings.Contains(body, "https://signed/cards/abc_thumb.png") {
		t.Errorf("expected signed thumb URL, got: %s", body)
	}
}

// ─── trivial helpers ─────────────────────────────────────────────────────

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// formValueOrDefault is exercised indirectly by the upload tests above; this
// nudges any unused-import warning if go vet ever complains.
var _ = fmt.Sprintf
