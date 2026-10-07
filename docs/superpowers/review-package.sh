#!/usr/bin/env bash
# Usage: review-package.sh <repo> <base> <head> [task-slice]
# Prints: commit list, full range diff, plan slice for the task.
# The plan-slice section — header included — is emitted only when the
# task matches plan content; an absent or unmatched task arg skips it
# instead of printing an empty "=== PLAN SLICE ===" header.
set -euo pipefail
REPO="$1"; BASE="$2"; HEAD="$3"; TASK="${4:-}"
export PATH=/home/kyomufs/.nix-profile/bin:$PATH
echo "=== COMMITS ($BASE..$HEAD) ==="
git -C "$REPO" log --oneline "$BASE..$HEAD"
echo
echo "=== DIFFSTAT ==="
git -C "$REPO" diff --stat "$BASE..$HEAD"
echo
echo "=== FULL DIFF ==="
git -C "$REPO" diff "$BASE..$HEAD"
echo
if [ -n "$TASK" ]; then
  PLAN="$REPO/docs/superpowers/plans/2026-10-06-zen-router-core.md"
  # START empty = no matching slice: guard before printing anything
  # (grep exits 1 on no match, which pipefail would otherwise propagate).
  START=$(grep -n "^### $TASK" "$PLAN" | head -1 | cut -d: -f1 || true)
  if [ -n "$START" ]; then
    echo "=== PLAN SLICE: $TASK ==="
    NEXT=$(awk -v s="$START" 'NR>s && /^### Task /{print NR; exit}' "$PLAN")
    if [ -n "${NEXT:-}" ]; then sed -n "${START},$((NEXT-1))p" "$PLAN"; else sed -n "${START},\$p" "$PLAN"; fi
  fi
fi
