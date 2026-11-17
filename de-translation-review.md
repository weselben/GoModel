# German Docs Translation Review — ENTERPILOT/GoModel

**Target:** PR #1254 "docs: add German, Chinese, and Polish translations" (author: SantiagoDePolonia, MERGED)
**Scope:** `docs/de/` on upstream/main — 105 files
**Working language of this draft:** English. German passages are quoted verbatim for reference.
**Output format:** local file only — no GitHub issue. When the review is done, the file goes up as a **private gist**; Ben opens the upstream issue manually from it.
**File contents:** per point — which file, the current German text, the proposed replacement **before → after**, and a short **English explanation** of why.
**Attribution note:** repo rules forbid AI co-authorship; Ben words the final public issue himself.

## Review points

<!-- New points get appended below. Each point:

### Point N: <short title>
- **File:** docs/de/path/to/file.mdx
- **Current (DE):** > quoted German text
- **Proposed:** > quoted German text (the replacement)
- **Why (EN):** short explanation of the problem and the reasoning
- **Verdict:** accept idea / reject idea / needs Ben's call
-->

### Point 1: Formal "Sie" → neutral "du" across the German docs
- **File:** docs/de/getting-started/quickstart.mdx (example; ~1,654 Sie/Ihre hits across 105 files, top: production.mdx 90, configuration.mdx 82)
- **Current (DE):** > "GoModel AI-Gateway: Starten Sie in 20 Sekunden ein OpenAI-kompatibles LLM-Gateway, senden Sie Ihren ersten Request und sehen Sie die Logs in Echtzeit."
- **Proposed:** > "GoModel AI-Gateway: Starte in 20 Sekunden ein OpenAI-kompatibles LLM-Gateway, sende deinen ersten Request und sieh die Logs in Echtzeit."
- **Why (EN):** The German translation uses formal "Sie" throughout. German developer docs and tool READMEs (Docker, Kubernetes, Homebrew, git) overwhelmingly use the neutral "du" — it matches the informal, direct tone of the English source ("Start an OpenAI-compatible LLM gateway in 20 seconds, send your first request, and watch the logs in real time." — second person, no formality marker). "Sie" reads stiff and corporate for a dev tool. Since the English original is informal-neutral, "du" is the faithful register; it is also the dominant convention in the German dev community. The quickstart description is the first sentence every German visitor sees, so it sets the tone. **Scope decision (Ben): full-tree conversion.** Not a blind sed — each "Sie/Ihre/Ihnen" occurrence must be re-derived per sentence so the result is semantically AND idiomatically correct German "du" (verb conjugation, possessives, imperative forms, politeness idioms). ~1,654 occurrences across 105 files; every conversion listed per file below.
- **Verdict:** accept idea — full-tree du conversion, per-occurrence re-derivation, listed per file

## Sie → du conversions (per file)

<!-- Each conversion: file, before, after. Idiomatic check: verb conjugation,
possessives (dein/deine/deinem), imperative forms, set phrases. -->

