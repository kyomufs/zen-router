#!/usr/bin/env bash
# Usage: review-package.sh <repo> <base> <head> <task-slice>
# Prints: commit list, full range diff, plan slice for the task.
set -euo pipefail
REPO="$1"; BASE="$2"; HEAD="$3"; TASK="$4"
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
echo "=== PLAN SLICE: $TASK ==="
PLAN="$REPO/docs/superpowers/plans/2026-10-06-zen-router-core.md"
START=$(grep -n "^### $TASK" "$PLAN" | head -1 | cut -d: -f1)
NEXT=$(awk -v s="$START" 'NR>s && /^### Task /{print NR; exit}' "$PLAN")
if [ -n "${NEXT:-}" ]; then sed -n "${START},$((NEXT-1))p" "$PLAN"; else sed -n "${START},\$p" "$PLAN"; fi
