Propose a fix for `{{.service}}`, based on this confirmed hypothesis:

{{.hypothesis}}

Rules (enforced by policy, see `platform://policy/mcp`):

- Never change the running cluster. `patch_deployment` and any other direct
  mutation are refused; the approved path is `create_pull_request`.
- Only files under `gitops/workloads/` for `{{.service}}` may change, on a
  branch named `agent/<short-description>`.
- Never add or change Secrets or credentials. If the cause is a credential
  (for example a password mismatch), do not open a PR: escalate to a human
  with the evidence, as the runbook requires.

Steps:

1. Check the runbook (`platform://services/{{.service}}/runbook`) allows the
   agent to fix this cause. If it says escalate, stop and escalate.
2. Use `get_git_changes` to find the exact file and the commit that
   introduced the problem. Fetch the current file content from that diff's
   context.
3. Make the minimal change: revert the offending value only. Do not refactor,
   reformat, or bump anything else.
4. Call `create_pull_request` with:
   - `title`: `fix({{.service}}): <what is restored>`
   - `body`: the hypothesis, the evidence (error pattern and count, start
     time, offending commit SHA), the exact change, and how recovery will be
     verified (error ratio under 1% for 5 minutes, alert resolved).
   - the complete new content of each changed file.
5. Report the PR URL. A human reviews and merges it after CI passes; Argo CD
   then deploys it.
