#!/usr/bin/env bash
# Enforce the project coverage floor on the packages named in AGENTS.md.
set -euo pipefail

threshold="${COVERAGE_THRESHOLD:-80}"
packages=(config auth ratelimit breaker balancer router proxy)
failed=0

printf '%-16s %8s\n' "package" "coverage"
printf '%-16s %8s\n' "-------" "--------"

for pkg in "${packages[@]}"; do
  output="$(go test -count=1 -cover "./internal/${pkg}")"
  pct="$(sed -n 's/.*coverage: \([0-9.]*\)%.*/\1/p' <<<"${output}")"
  if [[ -z "${pct}" ]]; then
    printf '%-16s %8s\n' "${pkg}" "unknown"
    failed=1
    continue
  fi
  printf '%-16s %7s%%\n' "${pkg}" "${pct}"
  if ! awk -v got="${pct}" -v want="${threshold}" 'BEGIN { exit !(got + 0 >= want + 0) }'; then
    failed=1
  fi
done

if (( failed )); then
  echo "coverage gate failed: every package must be at least ${threshold}%" >&2
  exit 1
fi
echo "coverage gate passed (minimum ${threshold}%)"
