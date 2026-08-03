#!/usr/bin/env bash
# scripts/check-env.sh — pre-flight env var check.
#
# Verifies that variables declared in the repo's env file are set before
# starting the dev server. Prints one helpful ERROR line per missing var to
# stderr and exits 1; prints OK and exits 0 if all required vars are set.
#
# Source file resolution: prefers .env.example, falls back to .env.template.
# Only uncommented VAR=... lines are treated as required — matches the issue
# while keeping the fully-commented template as a no-op (a fresh checkout
# passes by default).
set -euo pipefail

# Resolve repo root from this script's location so it works from any cwd.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

if [ -f "$REPO_ROOT/.env.example" ]; then
    ENV_FILE="$REPO_ROOT/.env.example"
elif [ -f "$REPO_ROOT/.env.template" ]; then
    ENV_FILE="$REPO_ROOT/.env.template"
else
    echo "NOTICE: no .env.example or .env.template found in $REPO_ROOT — nothing to check"
    exit 0
fi

# Extract uncommented VAR=... names. Matches [A-Za-z_][A-Za-z0-9_]*= and
# captures just the name. Skips blank lines and lines starting with '#'.
required_vars=$(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$ENV_FILE" \
    | sed -E 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1/' || true)

if [ -z "$required_vars" ]; then
    echo "OK: no required env vars in $ENV_FILE"
    exit 0
fi

missing=()
total=0
for var in $required_vars; do
    total=$((total + 1))
    # ${!var+x} expands to 'x' if var is set (even empty), empty otherwise.
    if [ -z "${!var+x}" ]; then
        missing+=("$var")
    fi
done

if [ "${#missing[@]}" -gt 0 ]; then
    for var in "${missing[@]}"; do
        echo "ERROR: $var is not set — see $ENV_FILE" >&2
    done
    exit 1
fi

echo "OK: all $total required env var(s) are set"
exit 0
