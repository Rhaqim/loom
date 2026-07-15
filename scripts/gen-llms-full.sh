#!/usr/bin/env bash
# Generates llms-full.txt: the curated llms.txt guide followed by the COMPLETE
# API reference (go doc) for every public package. Run via `make llms-full`.
#
# The engine's real types live in internal/engine and are re-exported 1:1 under
# the same names in the root `loom` package, so the full reference is taken from
# internal/engine (which carries the fields/methods that `go doc` on a facade of
# aliases cannot show). A header explains the loom. ↔ engine. mapping.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=llms-full.txt

rule='################################################################################'

{
  cat llms.txt

  cat <<HDR

$rule
# COMPLETE API REFERENCE  (generated — do not edit; regenerate with: make llms-full)
$rule

The types below are shown from the implementation package \`engine\`. Each is
re-exported under the SAME name in \`github.com/rhaqim/loom\` — e.g.
\`engine.Session\` is \`loom.Session\`, \`engine.RunStep\` is a method on
\`loom.Engine\`. In your code always use the \`loom.\` prefix and import only
\`github.com/rhaqim/loom\` (plus the subpackages shown further below). Never
import internal/engine directly.
HDR

  printf '\n%s\n# github.com/rhaqim/loom  (use these under the loom. prefix)\n%s\n\n' "$rule" "$rule"
  go doc -all ./internal/engine

  for pkg in schema generator/openai generator/anthropic generator/replicate generator/runway generator/echo judge harness cmd/loom-cli; do
    printf '\n%s\n# github.com/rhaqim/loom/%s\n%s\n\n' "$rule" "$pkg" "$rule"
    go doc -all "./$pkg" 2>/dev/null || echo "(no exported API)"
  done
} > "$OUT"

echo "wrote $OUT ($(wc -l < "$OUT" | tr -d ' ') lines)"
