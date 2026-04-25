// Package main is the one-shot Firestore migration for the PR-5 schema
// change. Walks every Project document, drops legacy text-only TitleCard
// fields (Text, Style), and computes ReferencedCardIDs from the new
// ImageID field (which will be nil on every existing manifest because no
// project was authored against the new schema yet).
//
// Run once before deploying the PR-5 server. Idempotent — re-running
// doesn't break anything; it just re-saves identical documents.
//
// Usage:
//
//	go run ./cmd/migrate
//
// Picks up FIRESTORE_PROJECT_ID and GOOGLE_APPLICATION_CREDENTIALS from
// the environment the same way the main server does.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

const projectsCollection = "projects"

// projectDoc is a partial view of the Firestore Project document — only
// the fields we touch. Using a flexible map for everything else preserves
// any field we don't know about (forward-compatible).
type projectDoc struct {
	Manifest          map[string]any `firestore:"Manifest,omitempty"`
	ReferencedCardIDs []string       `firestore:"ReferencedCardIDs,omitempty"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	projectID := os.Getenv("FIRESTORE_PROJECT_ID")
	var opts []option.ClientOption
	if cred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); cred != "" {
		opts = append(opts, option.WithCredentialsFile(cred))
	}
	if projectID == "" {
		projectID = firestore.DetectProjectID
	}

	client, err := firestore.NewClient(ctx, projectID, opts...)
	if err != nil {
		log.Fatalf("firestore.NewClient: %v", err)
	}
	defer client.Close()

	iter := client.Collection(projectsCollection).Documents(ctx)
	defer iter.Stop()

	var migrated, skipped, errors int
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatalf("iter: %v", err)
		}

		updates, why, err := computeUpdates(doc.Data())
		if err != nil {
			log.Printf("[ERROR] %s: %v", doc.Ref.ID, err)
			errors++
			continue
		}
		if len(updates) == 0 {
			fmt.Printf("[SKIP]    %s — already on new schema\n", doc.Ref.ID)
			skipped++
			continue
		}

		if _, err := doc.Ref.Update(ctx, updates); err != nil {
			log.Printf("[ERROR] %s: update: %v", doc.Ref.ID, err)
			errors++
			continue
		}
		fmt.Printf("[MIGRATE] %s — %s\n", doc.Ref.ID, why)
		migrated++
	}

	fmt.Printf("\nDone. migrated=%d skipped=%d errors=%d\n", migrated, skipped, errors)
	if errors > 0 {
		os.Exit(1)
	}
}

// computeUpdates inspects a project document and returns the Firestore
// update list needed to move it to the new schema. Returns (nil, "", nil)
// when no update is needed (already migrated).
//
// The migration:
//   - For every TitleCard inside the manifest: clear the legacy Text and
//     Style fields. Leave ImageID alone (will be nil for legacy data —
//     the empty-library bridge in the AI prompt handles this).
//   - Recompute Project.ReferencedCardIDs from the manifest's title cards.
func computeUpdates(data map[string]any) ([]firestore.Update, string, error) {
	manifestRaw, hasManifest := data["Manifest"]
	if !hasManifest || manifestRaw == nil {
		// No manifest = nothing to migrate. Still set ReferencedCardIDs
		// to empty so the field exists for array-contains queries.
		if _, ok := data["ReferencedCardIDs"]; ok {
			return nil, "", nil
		}
		return []firestore.Update{
			{Path: "ReferencedCardIDs", Value: []string{}},
		}, "no manifest, set empty ReferencedCardIDs", nil
	}

	manifest, ok := manifestRaw.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("manifest is not a map: %T", manifestRaw)
	}

	tcsRaw, _ := manifest["TitleCards"].([]any)
	migratedTCs := make([]map[string]any, 0, len(tcsRaw))
	hadLegacyFields := false
	var refIDs []string
	seen := make(map[string]bool)

	for _, tcRaw := range tcsRaw {
		tc, ok := tcRaw.(map[string]any)
		if !ok {
			continue
		}
		// Detect legacy fields. If present, clear them in the migrated copy.
		if _, has := tc["Text"]; has {
			hadLegacyFields = true
			delete(tc, "Text")
		}
		if _, has := tc["Style"]; has {
			hadLegacyFields = true
			delete(tc, "Style")
		}
		// Carry over ImageID (may be nil) and other fields untouched.
		if id, ok := tc["ImageID"].(string); ok && id != "" && !seen[id] {
			seen[id] = true
			refIDs = append(refIDs, id)
		}
		migratedTCs = append(migratedTCs, tc)
	}
	if refIDs == nil {
		refIDs = []string{}
	}

	// Compare against what's already there to avoid pointless writes.
	existingRefs, _ := data["ReferencedCardIDs"].([]any)
	refsAlreadyMatch := len(existingRefs) == len(refIDs)
	if refsAlreadyMatch {
		for i, v := range existingRefs {
			s, _ := v.(string)
			if s != refIDs[i] {
				refsAlreadyMatch = false
				break
			}
		}
	}

	if !hadLegacyFields && refsAlreadyMatch {
		return nil, "", nil
	}

	updates := []firestore.Update{
		{Path: "ReferencedCardIDs", Value: refIDs},
	}
	if hadLegacyFields {
		manifest["TitleCards"] = migratedTCs
		updates = append(updates, firestore.Update{Path: "Manifest", Value: manifest})
	}

	reasons := ""
	if hadLegacyFields {
		reasons = "cleared Text/Style on title cards; "
	}
	reasons += fmt.Sprintf("set ReferencedCardIDs=%v", refIDs)
	return updates, reasons, nil
}
