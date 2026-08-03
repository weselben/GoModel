# Models page — loading-state design

Date: 2026-08-03
Branch: feat/test-c-loading-spinner
Scope: `web/dashboard/src/pages/models/` only (no global refactor, no new
npm deps).

## Background

User report: the Models page "feels empty while it loads". The current
page already wires `LoadingState` in `ModelsPage.svelte` at line 121, but
it renders only a small spinner pill above the (blank) table area, so the
bulk of the viewport stays empty during the first fetch.

Stack reality check (from `web/dashboard/CONVENTIONS.md` and
`package.json`): Svelte 5 + Vite, no Tailwind, no shadcn, no new
dependencies allowed. The "blue accent" the user remembered does not
exist — the accent is brown (`--accent: #b8956e` dark / `#755c3d` light);
blue is the `--info` token. The existing spinner arc already uses
`var(--accent)`, so it automatically matches the real accent in both
themes.

## Chosen pattern — hybrid, scoped to Models page

Keep the existing spinner for refreshes and incremental rendering. Add a
table-row skeleton for the initial load only.

- **Initial load** (`modelsStore.loading && displayModels.length === 0`):
  render `ModelsTableSkeleton.svelte` in the table slot. Six shimmer
  rows mirror `ModelTable`'s column shape so the empty area is filled
  while data arrives. The skeleton's render branch must live *outside*
  the existing `{#if displayModels.length > 0 || filter}<ModelTable />
  {/if}` gate — that gate is intentionally a "have data yet?" check and
  must keep its current behaviour for `<ModelTable />` only.
- **Refresh / incremental render** (`modelsStore.loading` with data, or
  `modelsRendering`): keep the existing `<LoadingState />` pill; real
  rows are already visible and the pill communicates progress.
- **Error / empty / filter-empty**: unchanged. The existing alerts and
  `empty-state` paragraphs handle those cases; the skeleton disappears as
  soon as `loading` flips to false.

## Components

| Component | Action | Notes |
|-----------|--------|-------|
| `src/pages/models/ModelsPage.svelte` | edit | Add a `showSkeleton` derived flag (`modelsStore.loading && virtualModels.displayModels.length === 0`) and render `<ModelsTableSkeleton />` in a branch *outside* the existing `<ModelTable />` gate while it is true. Keep the existing `<LoadingState />` wiring untouched. |
| `src/pages/models/ModelsTableSkeleton.svelte` | new | Presentational component. Renders a `<table>` with the same `data-table` class as `ModelTable` and six skeleton rows. `role="status"` + `aria-label="Loading models"` on the wrapper, shimmer cells `aria-hidden`. Scoped `<style>` block with the shimmer keyframes; respect `prefers-reduced-motion: reduce` by freezing the shimmer, matching the convention in `src/styles/cards-charts.css` and `src/lib/utils/motion.js`. |
| `src/pages/models/ModelTable.svelte` | unchanged | Read-only reference for the skeleton's column shape. |
| `src/lib/components/molecules/LoadingState.svelte` | unchanged | Already used for refresh/render states; no edits. |
| `src/lib/components/atoms/Spinner.svelte` | unchanged | Already uses `var(--accent)`; no edits. |
| `src/styles/tables.css` / `page-globals.css` | unchanged | The existing `.loading-spinner` and `.models-loading-state` rules stay; no global CSS churn. |

No changes to `src/lib/`, no changes to shared stores, no changes to the
Go backend.

## Files expected to change

- `web/dashboard/src/pages/models/ModelsPage.svelte` — small edit
  (skeleton branch in the render tree).
- `web/dashboard/src/pages/models/ModelsTableSkeleton.svelte` — new
  file.
- `web/dashboard/tests/modelsSkeleton.test.js` — optional; only if the
  `showSkeleton` predicate is extracted into a pure helper
  (`pages/models/modelsSkeletonLogic.js`). Pure logic in plain `.js`,
  tested with `node:test` per `CONVENTIONS.md`. If the predicate stays
  inline in the Svelte component, no new test file is needed.

## Data flow

```
modelsStore.loading  ─┐
                      ├─► ModelsPage derived flag ──► <ModelsTableSkeleton />  (initial load)
virtualModels.        │
  displayModels       │
                      └─► <ModelTable />  (data ready)
```

`modelsStore.loading` is already `$state` in
`src/lib/stores/models.svelte.js`; no new fetch logic is introduced.

## Error / empty states (unchanged)

- Fetch error → existing `alert alert-warning` banners from
  `virtualModels` / `pricingOverrides` stores render above the table.
- Zero models after load → existing `No models registered.` empty state.
- Filter with no matches → existing `No models match your filter.` empty
  state.
- 401 → global auth dialog (handled by `$lib/api/client.js`).

The skeleton renders only while `loading` is true, so it never competes
with the error or empty branches.

## Accessibility

- Skeleton wrapper: `role="status"`, `aria-live="polite"`,
  `aria-label="Loading models"`.
- Shimmer cells: `aria-hidden="true"`.
- `prefers-reduced-motion: reduce` → shimmer animation is frozen (single
  static frame), matching `cards-charts.css:100` and
  `src/lib/utils/motion.js`.

## Testing approach

1. `cd web/dashboard && npm run check` — svelte-check, zero errors.
2. `cd web/dashboard && npm test` — existing `node:test` suites still
   pass; add `modelsSkeleton.test.js` only if a pure helper is extracted.
3. `cd web/dashboard && npm run build` — keeps the embedded `dist/` in
   sync; CI enforces drift.
4. Manual pass: throttle the network in devtools, open `/models`,
   confirm the skeleton fills the table area on first paint, the spinner
   pill appears on refresh, and both disappear once rows arrive. Toggle
   light/dark theme to confirm the shimmer uses theme tokens.

## Out of scope

- Tailwind / shadcn adoption (forbidden by `CONVENTIONS.md`).
- Global loading-state component for other pages.
- Changes to the accent palette or the spinner's `var(--accent)` arc.
- Any Go backend or admin API changes.
