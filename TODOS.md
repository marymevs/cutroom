# TODOs

## v2.1 follow-ups (not blocking v2 ship)

### Run /design-consultation for a project-wide DESIGN.md
- **What:** Generate a DESIGN.md capturing typography, color system, component
  vocabulary, motion, and spacing principles for Cutroom as a whole.
- **Why:** Today's `main.css` patterns are coherent but undocumented. Future
  contributors (and future-you in 6 months) reverse-engineer them. Formalizing
  prevents drift and sharpens future `/plan-design-review` runs.
- **Effort:** ~1–2h CC.
- **Why deferred:** Pick up after v2 ships and there's ground truth on which
  design choices stuck.

### Tags / favorites on cards
- **What:** Add tag or favorite metadata to cards for power-user library
  scaling (filter input alone covers <30 cards).
- **Why:** When the library grows past ~30 cards, "find the blue one from the
  cooking series" needs more than name filter.
- **Effort:** ~1h CC. Single Firestore field + UI for entry/display/filter.
- **Why deferred:** Triggers only when real library size demands it. Don't
  pre-build for hypothetical scale.

### Parallelize Whisper + GCS clip downloads
- **What:** Convert the serial per-clip loops in `internal/editor/pipeline.go` (`Analyze` line 64, `ensureClipsLocal` line 42) to `errgroup.WithContext`-driven parallel execution.
- **Why:** A 4-clip project today does 4 Whisper API calls in series. Independent calls — parallelizing collapses latency from 4× to 1× max.
- **Effort:** ~30 min CC.
- **Why deferred:** Not blocking the v2 Foundation + Wedge work; pick up immediately after.

### Failed-render observability
- **What:** Detect projects stuck in `StatusRendering` for >15 minutes with no recent log activity. Add a "Restart render" button that resets `Status` and re-enqueues.
- **Why:** Cloud Run instances can die mid-render. The goroutine has no checkpoint; the project sits in `rendering` forever and the user sees a spinner that never resolves.
- **Effort:** ~1h CC.
- **Why deferred:** Cloud Run instance death is rare. Defensive but not critical for v2.

## Approach C — deferred to a future office hours session

These were considered during /office-hours and explicitly deferred to keep v2
scope honest. Capture for future work:

- Brand profile per project (title cards + colors + fonts persisted in Firestore)
- Thumbnail generation via image model (3 options per video)
- Full UI design polish (typography, spacing, motion)
- SVG title-card support (requires librsvg in the Cloud Run image)
- Parallel per-segment encodes (run two ffmpegs concurrently on 4 vCPU)
- Tami's producer view (only if Tami actually wants to use Cutroom)
