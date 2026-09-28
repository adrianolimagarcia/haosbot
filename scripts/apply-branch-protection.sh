#!/usr/bin/env bash
set -euo pipefail

repo="${1:-adrianolimagarcia/haosbot}"
branch="${2:-main}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v gh >/dev/null 2>&1; then
  echo "gh CLI is required" >&2
  exit 1
fi

gh api   --method PUT   -H "Accept: application/vnd.github+json"   -H "X-GitHub-Api-Version: 2022-11-28"   "repos/${repo}/branches/${branch}/protection"   --input "${root}/.github/branch-protection.json"

echo "Applied branch protection to ${repo}:${branch}"
