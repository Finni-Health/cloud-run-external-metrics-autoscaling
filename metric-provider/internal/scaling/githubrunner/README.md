# GitHub runner discovery patch

This scaler is adapted from KEDA **v2.18.3**:
https://github.com/kedacore/keda/blob/v2.18.3/pkg/scalers/github_runner_scaler.go

The source is licensed under Apache-2.0, as is this repository. We reuse KEDA's
exported response models and metric helpers. Authentication, label matching,
HTTP handling, and metric calculation retain the upstream implementation.

CREMA's `github-runner` builder uses this local implementation because KEDA has
no configuration option to discover `pending` workflow runs. A workflow can
report `pending` while a job inside it is already `in_progress`, for example
when other jobs wait on a concurrency group.

The behavior changes are limited to querying the GitHub workflow-runs endpoint
for `pending` and retaining those runs in `stripDeadRuns`. The job filter still
counts only `queued` or `in_progress` jobs with matching runner labels. Pending
jobs, completed jobs, and unmatched labels do not themselves create demand.
There is one extra workflow-list API request per configured repository per poll.
The builder preserves KEDA's default trigger ID (`githubRunnerScaler`) for
existing log and Cloud Monitoring labels; explicit trigger names still win.

When updating KEDA, compare this file with the corresponding upstream scaler.
Remove the local implementation once upstream supports this discovery behavior.

## Regression test

From the repository root, generate the same protobuf sources as the Docker build:

```sh
protoc --proto_path=. --go_out=. --go-grpc_out=. proto/*.proto
cd metric-provider
go test ./internal/scaling -run '^TestGitHubRunnerWorkflowDiscovery$' -count=1
```

The test exercises CREMA's real builder, authentication resolver, GitHub scaler,
and state provider against a local HTTP server and a fake secret manager. It
covers pending workflows containing queued/running production jobs, excludes
pending/hosted/unlabeled/completed/mismatched jobs, and verifies that a discovery
error is propagated instead of becoming a valid zero-demand reading. It never
contacts GitHub or changes Cloud Run instance counts.
