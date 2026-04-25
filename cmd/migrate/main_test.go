package main

import (
	"reflect"
	"testing"
)

// computeUpdates is a pure function (no Firestore calls) so we can test
// the migration logic exhaustively without spinning up an emulator.

func TestComputeUpdates_LegacyTextOnlyManifest(t *testing.T) {
	// CRITICAL REGRESSION: a project authored before PR-5 has TitleCards
	// with Text and Style but no ImageID. Migration must clear Text/Style
	// (they're meaningless now) and set ReferencedCardIDs=[].
	data := map[string]any{
		"Manifest": map[string]any{
			"TitleCards": []any{
				map[string]any{"AfterSegment": int64(0), "Text": "The History", "Duration": 3.0, "Style": "default"},
				map[string]any{"AfterSegment": int64(2), "Text": "Conclusion", "Duration": 4.0, "Style": "minimal"},
			},
		},
	}

	updates, why, err := computeUpdates(data)
	if err != nil {
		t.Fatalf("computeUpdates: %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected updates for legacy doc, got none")
	}

	// Should set ReferencedCardIDs to empty and replace Manifest with cleaned cards.
	var refsUpdate, manifestUpdate any
	for _, u := range updates {
		switch u.Path {
		case "ReferencedCardIDs":
			refsUpdate = u.Value
		case "Manifest":
			manifestUpdate = u.Value
		}
	}
	if refs, ok := refsUpdate.([]string); !ok || len(refs) != 0 {
		t.Errorf("expected ReferencedCardIDs=[], got %v", refsUpdate)
	}
	manifest, ok := manifestUpdate.(map[string]any)
	if !ok {
		t.Fatalf("expected Manifest map update, got %T", manifestUpdate)
	}
	tcs := manifest["TitleCards"].([]map[string]any)
	for i, tc := range tcs {
		if _, has := tc["Text"]; has {
			t.Errorf("title card %d still has Text", i)
		}
		if _, has := tc["Style"]; has {
			t.Errorf("title card %d still has Style", i)
		}
		if tc["AfterSegment"] == nil || tc["Duration"] == nil {
			t.Errorf("title card %d lost a non-legacy field: %v", i, tc)
		}
	}
	if why == "" {
		t.Error("expected non-empty 'why' string for legacy migration")
	}
}

func TestComputeUpdates_NewSchemaWithImageIDsComputesReferencedCardIDs(t *testing.T) {
	// A manifest already on the new schema (ImageID set). Migration should
	// set ReferencedCardIDs and not touch the manifest itself.
	data := map[string]any{
		"Manifest": map[string]any{
			"TitleCards": []any{
				map[string]any{"AfterSegment": int64(0), "ImageID": "card-A", "Duration": 3.0},
				map[string]any{"AfterSegment": int64(1), "ImageID": "card-B", "Duration": 2.5},
				map[string]any{"AfterSegment": int64(2), "ImageID": "card-A", "Duration": 3.0}, // dup
			},
		},
	}

	updates, _, err := computeUpdates(data)
	if err != nil {
		t.Fatalf("computeUpdates: %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected ReferencedCardIDs update")
	}
	for _, u := range updates {
		if u.Path == "ReferencedCardIDs" {
			refs := u.Value.([]string)
			if !reflect.DeepEqual(refs, []string{"card-A", "card-B"}) {
				t.Errorf("expected dedup [card-A, card-B], got %v", refs)
			}
			return
		}
	}
	t.Error("no ReferencedCardIDs update found")
}

func TestComputeUpdates_NoManifestSetsEmptyReferencedCardIDs(t *testing.T) {
	data := map[string]any{} // project with no manifest yet

	updates, _, err := computeUpdates(data)
	if err != nil {
		t.Fatalf("computeUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	refs, ok := updates[0].Value.([]string)
	if !ok || len(refs) != 0 {
		t.Errorf("expected empty ReferencedCardIDs, got %v", updates[0].Value)
	}
}

func TestComputeUpdates_AlreadyMigratedReturnsNoUpdates(t *testing.T) {
	// Idempotent: a doc already migrated should produce zero updates.
	data := map[string]any{
		"Manifest": map[string]any{
			"TitleCards": []any{
				map[string]any{"AfterSegment": int64(0), "ImageID": "card-A", "Duration": 3.0},
			},
		},
		"ReferencedCardIDs": []any{"card-A"},
	}

	updates, why, err := computeUpdates(data)
	if err != nil {
		t.Fatalf("computeUpdates: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates for already-migrated doc, got %d (why=%q)", len(updates), why)
	}
}

func TestComputeUpdates_NoManifestNoExistingRefsSetsEmpty(t *testing.T) {
	data := map[string]any{}
	updates, _, err := computeUpdates(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Path != "ReferencedCardIDs" {
		t.Errorf("expected single ReferencedCardIDs update, got %+v", updates)
	}
}

func TestComputeUpdates_NoManifestExistingRefsSkipsUpdate(t *testing.T) {
	data := map[string]any{
		"ReferencedCardIDs": []any{},
	}
	updates, _, err := computeUpdates(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates when refs field already exists with no manifest, got %+v", updates)
	}
}

func TestComputeUpdates_DropsBlankImageIDs(t *testing.T) {
	// A manifest with both real and blank ImageIDs (e.g. a card the user
	// added but never picked) should produce ReferencedCardIDs without
	// the blanks.
	data := map[string]any{
		"Manifest": map[string]any{
			"TitleCards": []any{
				map[string]any{"AfterSegment": int64(0), "ImageID": "card-A", "Duration": 3.0},
				map[string]any{"AfterSegment": int64(1), "ImageID": "", "Duration": 3.0},
			},
		},
	}

	updates, _, err := computeUpdates(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range updates {
		if u.Path == "ReferencedCardIDs" {
			refs := u.Value.([]string)
			if !reflect.DeepEqual(refs, []string{"card-A"}) {
				t.Errorf("expected [card-A], got %v", refs)
			}
			return
		}
	}
	t.Error("no ReferencedCardIDs update found")
}
