# Clarifications — Models page loading state

Autonomous resolution log. Each entry follows the `grilling` Clarifications log
format (`## slug / **Verbatim:** / **Interpreted:**`). The user's verbatim
quote is taken from `/tmp/test-c-prompt.txt`; the **Interpreted** field is the
recommended decision the integration test would carry forward if the user had
answered.

## dashboard-stack
- **Verbatim:** "i found this loading spinner pattern from tailwind docs
  https://tailwindcss.com/docs/animation#spin" / "maybe the skeleton loading
  thing from shadcn? what do you think we should do here?"
- **Interpreted:** The dashboard is **Svelte 5 + Vite, no Tailwind, no
  shadcn**. `web/dashboard/package.json` lists only `svelte`, `chart.js`,
  `lucide`, plus the Vite/svelte-check/typescript dev deps. There is no
  `tailwind.config.*`, no `node_modules/tailwind*`, and `CONVENTIONS.md`
  states verbatim: "Do NOT add new npm dependencies." Therefore the Tailwind
  `animate-spin` snippet and shadcn `<Skeleton />` are both out of scope.
  Recommendation: hand-roll the loading UI in scoped CSS using the project's
  existing tokens and patterns. The CSS technique in the Tailwind snippet
  (a `@keyframes spin` rotating a circular border 360° linear infinite) is
  *already* the pattern this dashboard uses — see
  `web/dashboard/src/lib/components/atoms/Spinner.svelte`
  (`@keyframes spinner-rotate`) and `.loading-spinner` in
  `web/dashboard/src/styles/tables.css` (`@keyframes loading-spin`). The
  user's intent (a visible rotating loader) is already supported; the gap is
  in *placement and presence*, not in the technique.

## accent-color
- **Verbatim:** "the dashboard already has that blue accent color so maybe
  match that"
- **Interpreted:** The user's recollection is incorrect. The dashboard's
  accent is **brown**, not blue. In `web/dashboard/src/styles/themes.css`:
  dark theme `--accent: #b8956e` and light theme `--accent: #755c3d`. The
  blue token is `--info` (`#3b82f6` dark / `#2563eb` light), used for
  informational badges and chart cache colors — not the accent. Every
  existing spinner in the project (`Spinner` atom, `.loading-spinner` in
  tables.css, `.loading-spinner` inside `LoadingState.svelte`) paints its
  rotating arc with `border-top-color: var(--accent)`, so it already
  tracks the real accent in both themes via custom properties. Decision:
  keep `var(--accent)` on the loader arc for consistency with the rest of
  the dashboard. Do **not** swap in `--info` for one component; that would
  break visual consistency. Note this correction in the design doc so the
  implementer doesn't repeat the mistake.

## spinner-vs-skeleton
- **Verbatim:** "the models page feels empty while it loads, like there's
  no indication anything is happening" / "not sure if this is the right
  approach or if we should use a different pattern, maybe the skeleton
  loading thing from shadcn?"
- **Interpreted:** Hybrid, scoped to the Models page.
  - The existing `LoadingState` molecule (spinner pill at top of the table
    area) is already wired in and shown when `virtualModels.modelsBusy()`
    is true (`ModelsPage.svelte:121`). Keep it for **refreshes** and the
    **incremental-render** phase, since real rows already occupy the
    table.
  - Add a **table-row skeleton** that mirrors `ModelTable`'s column shape
    (provider badge, model id, pricing/cost cells, actions), shown only
    on the **initial load** — i.e. when `modelsStore.loading &&
    virtualModels.displayModels.length === 0`. That directly addresses the
    "feels empty" complaint: the table area is currently blank during
    first paint, and 6–8 skeleton rows give immediate visual feedback
    that content is on its way.
  - No shadcn dependency. The skeleton is a small
    `ModelsTableSkeleton.svelte` in `pages/models/` with a scoped CSS
    shimmer (animated `background-position` on `--bg-surface` /
    `--border` tones). Honour `prefers-reduced-motion: reduce` by
    freezing the shimmer, matching the convention used in
    `cards-charts.css` and the live-row motion util.

## scope-models-only-vs-global
- **Verbatim:** (implicit — complaint is about the Models page only)
- **Interpreted:** Models page only. The user's complaint names the
  Models page explicitly, and the spinner/skeleton pattern already exists
  for every other loading site in the dashboard. A global refactor is out
  of scope for a KISS loading-state change and would conflict with the
  "do not add dependencies / keep shared foundation deliberate" rule in
  `CONVENTIONS.md`.

## design-doc-location
- **Verbatim:** (none — user did not specify)
- **Interpreted:** Write the design doc at
  `docs/plans/2026-08-03-models-page-loading-design.md`, as the task
  contract specifies. The repo already has `docs/plans/` (with
  `test-scenarios-log.md`); this location is consistent and keeps
  scenario-related docs together.

## github-issue-recall
- **Verbatim:** "there was a github issue about this somewhere but i cant
  find it now, might have been about the settings page or something else
  entirely"
- **Interpreted:** None found.
  `gh issue list --repo ENTERPILOT/GoModel --search "loading spinner
  skeleton"` returns nothing. The general `gh issue list` of the repo
  (top 20 issues) has no item that matches "loading state", "empty while
  loading", or "skeleton" for any page. Proceed regardless — the
  Models-page complaint stands on its own and the fix is independent of
  any prior issue.

## testing-approach
- **Verbatim:** (none — user did not specify)
- **Interpreted:** Per `CONVENTIONS.md`, the verification commands from
  `web/dashboard/` are `npm run check` (svelte-check, zero errors
  required), `npm test` (`node --test tests/*.test.js`, pure-logic
  tests), and `npm run build` (keeps the embedded dist/ in sync, CI
  enforces drift). The skeleton is pure presentational with no logic to
  unit-test, so it relies on `npm run check` + `npm run build` + a
  manual visual pass. If a tiny pure helper is extracted (e.g. a
  `shouldShowSkeleton(loading, displayCount)` predicate in `pages/models/`),
  it can be covered by a `web/dashboard/tests/modelsSkeleton.test.js`
  mirroring the project's `node:test` style; otherwise no new test file
  is required.