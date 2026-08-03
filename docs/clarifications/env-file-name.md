# Clarification — issue #2: env file is `.env.template`, not `.env.example`

**Question:** The issue says "verifies the required env vars (from
`.env.example`)", but the repo has no `.env.example` — it has `.env.template`,
and every variable line in it is commented out (all optional).

**Decision (auto mode, documented instead of asked):**

- The script prefers `.env.example` when it exists and falls back to
  `.env.template`, so it satisfies the issue text while matching the repo.
- Only **uncommented** `VAR=...` lines count as required. The template ships
  fully commented, so a fresh checkout requires nothing and the script exits
  0 — a sane pre-flight default. Operators who uncomment vars in a local
  `.env.example` get those enforced.

**Trade-off:** If the intent was "every var in the template is required," the
script under-enforces. That reading is implausible — it would make the dev
server unstartable out of the box. Flagged here for the reviewer; easy to
flip by dropping the uncommented-only filter.
