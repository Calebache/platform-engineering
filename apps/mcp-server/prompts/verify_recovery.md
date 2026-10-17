Verify that `{{.service}}` has recovered{{if .pull_request}} after {{.pull_request}} was merged{{end}}.

Deployment completing is not the same as the incident being resolved.

1. `get_service_health`: Argo CD shows the new revision Synced and Healthy,
   the Deployment's pods are all Ready on the new ReplicaSet, and there are no
   new restarts or warning events.
2. `get_metrics` with window `15m`: the current error ratio is below 0.01, and
   has stayed there since the fix rolled out (compare with the window's max
   and start values). Request rate is back to its normal level.
3. `get_error_patterns` with window `10m`: the pattern that caused the
   incident no longer appears in new lines.
4. `get_incident_context`: the alert is no longer pending or firing.

Report one of:

- **Recovered**: every check passes; include the numbers.
- **Not yet**: checks are improving but the window is too short; say when to
  re-check.
- **Not recovered**: something still fails; include the evidence and
  escalate to a human.
