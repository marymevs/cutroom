package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mary/cutroom/internal/cards"
	"github.com/mary/cutroom/internal/domain"
)

// cardUploader is the minimal contract the cards handler needs from GCS:
// upload a byte slice and produce a fresh signed read URL for the thumb.
// Defined as an interface so tests can swap in a fake without touching real
// GCS. *gcs.Client doesn't satisfy this directly (its Upload takes
// io.Reader) — main.go wraps it with NewGCSCardUploader below.
type cardUploader interface {
	UploadBytes(ctx context.Context, objectName string, data []byte, contentType string) (string, error)
	SignedReadURL(objectName string) (string, error)
}

// cardStorage is the persistence contract — *cards.CardStore satisfies it,
// and tests provide an in-memory fake.
type cardStorage interface {
	Save(ctx context.Context, c *domain.Card) error
	Get(ctx context.Context, id string) (*domain.Card, error)
	List(ctx context.Context) ([]*domain.Card, error)
	Delete(ctx context.Context, id string) error
}

// CardsHandler owns the /cards routes (page render, upload, delete). The
// picker integration into manifest_partial.html lands in PR-5 alongside the
// TitleCard.ImageID schema change — PR-6 ships the standalone library only.
type CardsHandler struct {
	store    cardStorage
	uploader cardUploader
	pages    *template.Template
	partials *template.Template
}

func NewCardsHandler(store cardStorage, uploader cardUploader, pages, partials *template.Template) *CardsHandler {
	return &CardsHandler{store: store, uploader: uploader, pages: pages, partials: partials}
}

// ─── routes ──────────────────────────────────────────────────────────────

// CardsPage renders the full /cards library page.
func (h *CardsHandler) CardsPage(w http.ResponseWriter, r *http.Request) {
	list, err := h.listWithThumbs(r.Context())
	if err != nil {
		http.Error(w, "list cards: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.pages.ExecuteTemplate(w, "cards.html", struct {
		Cards []*domain.Card
	}{Cards: list}); err != nil {
		// Templates handle their own logging; never leak a half-written response.
		fmt.Println("cards page render error:", err)
	}
}

// CardsGrid renders only the inner grid partial (for HTMX swaps after a
// delete or upload completes).
func (h *CardsHandler) CardsGrid(w http.ResponseWriter, r *http.Request) {
	list, err := h.listWithThumbs(r.Context())
	if err != nil {
		http.Error(w, "list cards: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.partials.ExecuteTemplate(w, "cards_grid_partial.html", struct {
		Cards []*domain.Card
	}{Cards: list})
}

// listWithThumbs reads cards from the store and stamps each one with a
// fresh signed thumb URL. Signing is local (HMAC over the object path) so
// this stays cheap even with 30+ cards.
func (h *CardsHandler) listWithThumbs(ctx context.Context) ([]*domain.Card, error) {
	list, err := h.store.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		url, err := h.uploader.SignedReadURL(c.ThumbGCSPath)
		if err != nil {
			// Don't fail the whole list — just leave ThumbURL blank and the
			// tile renders with a broken-image affordance. Worth flagging.
			fmt.Println("sign thumb url for", c.ID, ":", err)
			continue
		}
		c.ThumbURL = url
	}
	return list, nil
}

// UploadCard handles POST /cards. Multipart form with a `file` part and
// optional `name`, `description`, `force` (string "1" to override the
// portrait-aspect warning) fields.
func (h *CardsHandler) UploadCard(w http.ResponseWriter, r *http.Request) {
	// Limit the multipart parser too — defense in depth against a single
	// 10GB upload that ValidateAndDecode's io.LimitReader catches but
	// after we've already buffered too much. 12 MB headroom for form fields.
	if err := r.ParseMultipartForm(cards.MaxFileBytes + 2*1024*1024); err != nil {
		writeCardError(w, http.StatusBadRequest, "Couldn't parse upload: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeCardError(w, http.StatusBadRequest, "Missing 'file' field.")
		return
	}
	defer file.Close()

	decoded, err := cards.ValidateAndDecode(file)
	if err != nil {
		var ve *cards.ValidationError
		if errors.As(err, &ve) {
			writeCardError(w, http.StatusBadRequest, ve.Message)
			return
		}
		http.Error(w, "decode upload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Portrait warn flow: when the PNG is portrait-orientation, return the
	// warn UI without committing unless the client passed force=1. The
	// design spec calls this out specifically because Canva exports
	// vertical regularly when designing for Reels/Stories, and a portrait
	// title card letterboxes badly inside a 16:9 video.
	if decoded.IsPortrait && r.FormValue("force") != "1" {
		w.WriteHeader(http.StatusOK) // 200 with the warn UI
		_ = h.partials.ExecuteTemplate(w, "card_warn_partial.html", struct {
			Filename    string
			Width       int
			Height      int
			Name        string
			Description string
		}{
			Filename:    header.Filename,
			Width:       decoded.Width,
			Height:      decoded.Height,
			Name:        formValueOrDefault(r, "name", cards.SafeName(header.Filename)),
			Description: r.FormValue("description"),
		})
		return
	}

	id := uuid.New().String()
	gcsPath := "cards/" + id + ".png"
	thumbGCSPath := "cards/" + id + "_thumb.png"

	if _, err := h.uploader.UploadBytes(r.Context(), gcsPath, decoded.OriginalBytes, "image/png"); err != nil {
		http.Error(w, "upload original: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := h.uploader.UploadBytes(r.Context(), thumbGCSPath, decoded.ThumbBytes, "image/png"); err != nil {
		http.Error(w, "upload thumbnail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	card := &domain.Card{
		ID:           id,
		Name:         formValueOrDefault(r, "name", cards.SafeName(header.Filename)),
		Description:  strings.TrimSpace(r.FormValue("description")),
		GCSPath:      gcsPath,
		ThumbGCSPath: thumbGCSPath,
		Width:        decoded.Width,
		Height:       decoded.Height,
		CreatedAt:    time.Now().UTC(),
	}
	if err := h.store.Save(r.Context(), card); err != nil {
		http.Error(w, "save card: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Sign a fresh thumb URL so the new tile renders immediately. Best-effort
	// — a missing signed URL just means the tile shows broken-image; the user
	// can refresh.
	if u, err := h.uploader.SignedReadURL(card.ThumbGCSPath); err == nil {
		card.ThumbURL = u
	}

	// Render the new tile so the client can swap it in via HTMX OOB.
	_ = h.partials.ExecuteTemplate(w, "card_tile_partial.html", card)
}

// DeleteCard handles DELETE /cards/{id}.
//
// Reference-check policy: PR-6 ships without TitleCard.ImageID (PR-5 adds it
// alongside Project.ReferencedCardIDs and the soft-unlink UX). For now,
// deletes always proceed. The block-while-referenced gate lands in PR-5.
func (h *CardsHandler) DeleteCard(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	c, err := h.store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "get card: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if c == nil {
		// Treat as already gone — idempotent delete.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.Delete(r.Context(), id); err != nil {
		http.Error(w, "delete card: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Best-effort: leave the GCS objects to the lifecycle policy. The
	// design doc accepts this — orphaned blobs are cheap and a follow-up
	// can sweep them on a schedule.
	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ─────────────────────────────────────────────────────────────

func formValueOrDefault(r *http.Request, key, def string) string {
	v := strings.TrimSpace(r.FormValue(key))
	if v == "" {
		return def
	}
	return v
}

// writeCardError emits a small inline error fragment that HTMX swaps into
// the upload zone's error region. Keeps the JSON-vs-HTML contract simple
// (everything is HTMX-shaped HTML).
func writeCardError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	fmt.Fprintf(w, `<div class="card-upload-error" role="alert">%s</div>`, htmlEscape(msg))
}

func htmlEscape(s string) string {
	var b bytes.Buffer
	template.HTMLEscape(&b, []byte(s))
	return b.String()
}

// gcsBytesAdapter wraps *gcs.Client functions to satisfy cardUploader.
// Cards are ≤10MB so buffering the byte slice in a bytes.Reader is fine.
type gcsBytesAdapter struct {
	upload    func(ctx context.Context, objectName string, data []byte, contentType string) (string, error)
	signedURL func(objectName string) (string, error)
}

func (a *gcsBytesAdapter) UploadBytes(ctx context.Context, objectName string, data []byte, contentType string) (string, error) {
	return a.upload(ctx, objectName, data, contentType)
}

func (a *gcsBytesAdapter) SignedReadURL(objectName string) (string, error) {
	return a.signedURL(objectName)
}

// NewGCSCardUploader builds a cardUploader from upload + signed-read-URL
// closures. main.go wires this with *gcs.Client.Upload (via bytes.NewReader)
// and *gcs.Client.ReadSignedURL.
func NewGCSCardUploader(
	upload func(ctx context.Context, objectName string, data []byte, contentType string) (string, error),
	signedURL func(objectName string) (string, error),
) cardUploader {
	return &gcsBytesAdapter{upload: upload, signedURL: signedURL}
}
