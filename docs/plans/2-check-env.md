# Plan — issue #2: scripts/check-env.sh pre-flight env var check

Issue: weselben/GoModel#2 — "Add scripts/check-env.sh for pre-flight env var validation"

## Goal

A small bash script `scripts/check-env.sh` that verifies the environment
variables declared in the repo's env template are set before starting the dev
server, with a clear error message per missing var.

## Facts on the ground

- The repo has `.env.template` (568 lines), **not** `.env.example` as the
  issue says. Every variable line in `.env.template` is commented out
  (`# PORT=8080`), so extraction must match commented `VAR=value` lines.
- All vars in the template are optional today (all commented). A strict
  "every var must be set" check would always fail. The script therefore
  checks only vars the operator has opted into: vars **uncommented** in a
  local `.env`/`.env.example` if present, else it falls back to checking
  nothing and prints a notice. Simpler faithful reading of the issue:
  the script extracts var names from the env file (`.env.example` if it
  exists, else `.env.template`), uncommented lines only, and reports any
  that are not set in the environment.

## Decision (see docs/clarifications/env-file-name.md)

- Source file: prefer `.env.example`, fall back to `.env.template`.
- Only uncommented `VAR=...` lines are treated as required — matches the
  issue's "extract var names, check each is set" without breaking on the
  all-optional template.

## Acceptance criteria (from the issue)

1. bash, no external dependencies (grep/sed/awk coreutils only).
2. Reads the env template, extracts var names, checks each is set.
3. Exit 1 with one helpful message per missing var; exit 0 when all set.
4. Simple — a pre-flight check, not a validator.

## Tasks

1. `scripts/check-env.sh` — new file, `#!/usr/bin/env bash`, `set -euo pipefail`.
   - Resolve the repo root from the script location so it works from any cwd.
   - Pick env file: `.env.example` else `.env.template`; if neither, exit 0
     with a notice.
   - Extract var names: lines matching `^[A-Za-z_][A-Za-z0-9_]*=` (uncommented
     only), ignore blank lines and comments.
   - For each var, check `[ -z "${!VAR+x}" ]`; collect missing.
   - Per missing var print `ERROR: <VAR> is not set — see <envfile>` to stderr.
   - Exit 1 if any missing, else print an OK line and exit 0.
2. `chmod +x scripts/check-env.sh`.
3. Smoke test: run with a dummy unset var present in a temp env file, and run
   clean against the real template (all commented → nothing required → exit 0).

## Out of scope

- Validating values, formats, or defaults.
- Wiring into Makefile/CI (can be a follow-up).
