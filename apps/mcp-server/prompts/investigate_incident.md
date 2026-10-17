The alert `{{.alert}}` is firing for `{{.service}}`. Investigate it.

Work only from evidence returned by the platform's tools. You cannot change
the cluster; this step ends with a hypothesis, not a fix.

1. Call `get_incident_context` with service `{{.service}}`. Note when the
   alert became active, the current and peak error ratio, and the top error
   patterns.
2. Read `platform://services/{{.service}}/runbook`. Match the dominant error
   pattern against its "Known causes" table.
3. Establish when the failure started: compare the first-seen time of the
   dominant error pattern with `get_recent_deployments` (which revision rolled
   out, and which images or env vars changed) and `get_git_changes` (which
   commit introduced that change).
4. Confirm with raw evidence: `get_recent_logs` filtered by one or two sample
   request IDs from the pattern.
5. Rule out alternatives the evidence contradicts (pods not ready, restarts,
   Postgres down) using `get_service_health`.

Then state:

- **Hypothesis**: one sentence naming the cause.
- **Evidence**: the error pattern and count, the time it started, the rollout
  and commit it coincides with, and the raw log lines you checked.
- **Confidence**: high, medium or low, and what would raise it.
- **Runbook path**: whether the runbook allows the agent to fix this (a Git
  change) or requires escalation to a human.

If the evidence doesn't support a single cause, say so and list what is
missing instead of guessing.
