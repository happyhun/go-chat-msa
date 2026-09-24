#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."
revision="$(git rev-parse --short=12 HEAD)"
content="$(git ls-files --cached --others --exclude-standard -z | while IFS= read -r -d '' file; do
  if [[ -f "${file}" ]]; then
    printf '%s\n' "${file}"
    git hash-object "${file}"
  fi
done | shasum -a 256 | cut -c 1-12)"
printf '%s-%s\n' "${revision}" "${content}"
