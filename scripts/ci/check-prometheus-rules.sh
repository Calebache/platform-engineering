#!/usr/bin/env bash
# Checks every PrometheusRule in gitops/ with promtool. The Prometheus
# Operator's admission webhook is off (see CLAUDE.md), so nothing else
# catches a broken rule before it's silently dropped.
#
# Needs yq (mikefarah, preinstalled on GitHub runners) and promtool; set
# PROMTOOL to run it some other way, e.g. through Docker.
set -euo pipefail

PROMTOOL="${PROMTOOL:-promtool}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"
out="$(mktemp -d "$REPO_ROOT/.rules.XXXXXX")"
trap 'rm -rf "$out"' EXIT

files=()
while IFS= read -r f; do
  base="$(echo "$f" | tr '/' '_')"
  # One rule file per PrometheusRule document: its spec is the rule file.
  yq -s "\"$out/${base%.yaml}-\" + \$index" 'select(.kind == "PrometheusRule") | .spec' "$f" </dev/null
done < <(git ls-files 'gitops/*.yaml' | xargs grep -l '^kind: PrometheusRule')
for f in "$out"/*.yml; do
  [[ -s "$f" ]] && files+=("${f#"$REPO_ROOT"/}")
done
(( ${#files[@]} > 0 )) || { echo "no PrometheusRules found" >&2; exit 1; }
$PROMTOOL check rules "${files[@]}"
