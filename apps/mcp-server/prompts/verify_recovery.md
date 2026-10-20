Verify that `{{.service}}` has recovered{{if .pull_request}} after {{.pull_request}} was merged{{end}}.

Deployment completing is not the same as the incident being resolved. The
worker has already waited until Argo CD synced a revision that includes the
merge and the rollout finished, so the fixed version is what's running now.

1. `get_service_health`: Argo CD shows the new revision Synced and Healthy,
   the Deployment's pods are all Ready on the new ReplicaSet, and there are no
   new restarts or warning events.
2. `get_metrics` with window `15m`: the current error ratio is below 0.01, and
   has stayed there since the fix rolled out (compare with the window's max
   and start values). Request rate is back to its normal level.
3. `get_error_patterns` with window `10m`: the pattern that caused the
   incident no longer appears in new lines.
4. `get_incident_context`: the alert is no longer pending or firing.

Metrics lag the rollout. `error_ratio` is a 5-minute rate, and an alert
resolves only after its own rate window clears, so for several minutes after a
good rollout the numbers can still look bad. You'll be asked again on a fixed
schedule; the prompt says which check this is.

Report one of:

- **Recovered**: every check passes; include the numbers.
- **Not yet**: the fixed version is running and the cause's error pattern has
  stopped in new lines, but the error ratio or the alert hasn't caught up;
  include the numbers.
- **Not recovered**: the cause's errors continue on the new version, or
  something else now fails; include the evidence for a human. On any check but
  the last, the worker treats this as not yet and checks again.
