# Models Page Loading Skeleton — Merged Forge Plan (Test C)

Date: 2026-08-03. Branch: `feat/test-c-loading-spinner` (integration, off `main`).

Inputs: `docs/plans/2026-08-03-models-page-loading-design.md`, `docs/clarifications/loading-spinner-models-page.md`, `/tmp/plan-research.md`, `/tmp/plan-implementation.md`.

## Context

Dashboard is Svelte 5 + Vite SPA at `web/dashboard/`. No Tailwind, no shadcn, no new npm deps (CONVENTIONS.md). Accent is brown (`--accent: #b8956e` dark / `#755c3d` light in `src/styles/themes.css:15`); the user's "blue accent" recollection was wrong — blue is `--info`. Loader arc stays on `var(--accent)`.

Bug: `ModelsPage.svelte:127-129` gates `<ModelTable />` on `displayModels.length > 0 || filter`, so the table area is blank during the first fetch; only the `LoadingState` pill (`ModelsPage.svelte:121-123`) signals activity.

## Decision (from clarifications)

Hybrid: keep existing spinner pill for refreshes; add `ModelsTableSkeleton.svelte` (6 shimmer rows mirroring the `table-wrapper`/`data-table` shape) rendered only on initial load — `loading && displayModels.length === 0`. Scoped CSS on theme tokens, `role="status"`/`aria-live="polite"`, shimmer frozen under `prefers-reduced-motion` (convention from `cards-charts.css:100-105`). Scope: Models page only.

## Phase 3 — Research

**Skipped.** All open items (reduced-motion practice, a11y pattern in `LoadingState.svelte:8-10`, theme tokens, test runner) verified in-repo. No deep-research swarm dispatched.

## Phase 4 — Implementation tasks (parallel, disjoint files, each in own worktree)

### Task 1 — branch `feat/test-c-models-table-skeleton`, worktree `.worktrees/test-c-models-table-skeleton`
- New `web/dashboard/src/pages/models/ModelsTableSkeleton.svelte`: 6 shimmer rows, scoped CSS, `prefers-reduced-motion` freeze, theme tokens only (no hex literals), `role="status"`/`aria-live`/`aria-label`, shimmer cells `aria-hidden`.
- AC: `npm run check` 0 errors; `npm run build` succeeds; no other files touched.

### Task 2 — branch `feat/test-c-models-skeleton-wiring`, worktree `.worktrees/test-c-models-skeleton-wiring`
- New `web/dashboard/src/pages/models/modelsSkeletonLogic.js`: pure `shouldShowModelsSkeleton(loading, displayCount)`.
- New `web/dashboard/tests/modelsSkeleton.test.js`: 4 `node:test` cases (runner is `node --test "tests/*.test.js"` — no vitest; mirror `models-pricing-overrides.test.js` style).
- Edit `web/dashboard/src/pages/models/ModelsPage.svelte`: import + `$derived` flag + sibling `{#if showSkeleton}<ModelsTableSkeleton />{/if}` branch immediately after the existing `<ModelTable />` gate (lines 128-130). Existing pill/gate/empty-state branches byte-for-byte unchanged.
- AC: `npm test` passes; no other files touched.

## Phase 5 — Review

One task reviewer per task; frontend-design design reviewer alongside (visual quality of skeleton). Fix rounds R≤3 resume implementer, R≥4 fresh, cap R=5.

## Phase 6 — Integration + PR

1. Merge `feat/test-c-models-table-skeleton` then `feat/test-c-models-skeleton-wiring` into `feat/test-c-loading-spinner` with `--no-ff`.
2. Verify merged result: `cd web/dashboard && npm run check` (0 errors), `npm test` (all pass), `npm run build`; repo-root `go test ./...` as regression guard.
3. Push, open **DRAFT** PR to `weselben/GoModel` (base `main`), title `feat(dashboard): skeleton loader for Models page initial fetch`.

## Constraints / risks

- Never stage or modify `docs/plans/test-scenarios-log.md`. Never touch other test branches (`feat/test-a-quick-start`, `feat/test-b-check-env`, `check-env-script`, `pr-1`).
- No DOM/component test runner exists — implementers must not claim component-level test coverage.
- Shimmer visual correctness is headless-unverifiable; optional Playwright pass against `npm run dev`, otherwise flag manual visual verification as pending.
