#!/usr/bin/env bash
# Stops a CI job unless the repository owner started it. GitHub Free can't
# put reviewers on an Environment for a private repo, so this is the gate in
# front of every job that holds AWS credentials or writes to the repo.
#
# GitHub sets GITHUB_ACTOR (whose event this is) and GITHUB_TRIGGERING_ACTOR
# (who started this run, e.g. on a re-run); both must be the owner. On pull
# requests, PR_AUTHOR (from the event payload) must be too, so re-running
# someone else's PR doesn't hand it credentials.
#
# It's a convention, not GitHub-enforced: anyone with write access could edit
# the workflow to drop the call. Here only the owner has write access.
set -euo pipefail

OWNER="${OWNER:-Calebache}"
fail=0
for var in GITHUB_ACTOR GITHUB_TRIGGERING_ACTOR PR_AUTHOR; do
  value="${!var-__unset__}"
  [[ "$var" == PR_AUTHOR && "$value" == __unset__ ]] && continue
  if [[ "$value" != "$OWNER" ]]; then
    printf '::error::%s is "%s", not the owner "%s"; this job only runs for the owner\n' "$var" "$value" "$OWNER"
    fail=1
  fi
done
(( fail == 0 )) || exit 1
echo "Started by the owner ($OWNER)"
