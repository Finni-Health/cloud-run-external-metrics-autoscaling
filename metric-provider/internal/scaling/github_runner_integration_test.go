package scaling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crema/metric-provider/api"
	"crema/metric-provider/internal/clients"
	"crema/metric-provider/internal/logging"
	"crema/metric-provider/internal/resolvers"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type githubJobFixture struct {
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// Exercise the actual CREMA builder, GitHub HTTP client, label matching, and
// StateProvider. Only GitHub is replaced; no cloud scaling requests are made.
func TestGitHubRunnerWorkflowDiscovery(t *testing.T) {
	testCases := []struct {
		name           string
		workflowStatus string
		jobs           []githubJobFixture
		apiStatus      int
		wantDemand     float64
	}{
		{
			name: "queued production starts a runner", workflowStatus: "queued",
			jobs: []githubJobFixture{{Status: "queued", Labels: []string{"self-hosted"}}}, wantDemand: 1,
		},
		{
			name: "pending workflow retains running production", workflowStatus: "pending",
			jobs: []githubJobFixture{
				{Status: "in_progress", Labels: []string{"self-hosted"}},
				{Status: "pending", Labels: []string{"ubuntu-latest"}},
				{Status: "queued", Labels: []string{"ubuntu-latest"}},
			}, wantDemand: 1,
		},
		{
			name: "pending workflow discovers queued production", workflowStatus: "pending",
			jobs: []githubJobFixture{{Status: "queued", Labels: []string{"self-hosted"}}}, wantDemand: 1,
		},
		{
			name: "pending jobs do not request runners", workflowStatus: "pending",
			jobs: []githubJobFixture{{Status: "pending", Labels: []string{"self-hosted"}}},
		},
		{
			name: "hosted unlabeled completed and mismatched jobs do not count", workflowStatus: "pending",
			jobs: []githubJobFixture{
				{Status: "in_progress", Labels: []string{"ubuntu-latest"}},
				{Status: "queued", Labels: nil},
				{Status: "completed", Labels: []string{"self-hosted"}},
				{Status: "queued", Labels: []string{"self-hosted", "gpu"}},
			},
		},
		{
			name: "in-progress workflow still counts running production", workflowStatus: "in_progress",
			jobs: []githubJobFixture{{Status: "in_progress", Labels: []string{"SELF-HOSTED"}}}, wantDemand: 1,
		},
		{
			name: "completed workflow releases demand", workflowStatus: "completed",
			jobs: []githubJobFixture{{Status: "completed", Labels: []string{"self-hosted"}}},
		},
		{
			name: "pending discovery failure is not reported as zero demand", workflowStatus: "pending",
			apiStatus: http.StatusServiceUnavailable,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "GET", request.Method)
				assert.Equal(t, "Bearer test-token", request.Header.Get("Authorization"))
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/repos/Finni-Health/finni/actions/runs":
					if request.URL.Query().Get("status") == "pending" && testCase.apiStatus != 0 {
						writer.WriteHeader(testCase.apiStatus)
						return
					}
					runs := []map[string]any{}
					if request.URL.Query().Get("status") == testCase.workflowStatus {
						runs = append(runs, map[string]any{
							"id": 42, "status": testCase.workflowStatus,
							"repository": map[string]string{"name": "finni"},
						})
					}
					assert.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"workflow_runs": runs, "total_count": len(runs)}))
				case "/repos/Finni-Health/finni/actions/runs/42/jobs":
					assert.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"jobs": testCase.jobs, "total_count": len(testCase.jobs)}))
				default:
					t.Errorf("unexpected GitHub request: %s", request.URL)
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			logger := logging.NewLogger()
			authResolver := resolvers.NewAuthResolver(&clients.StubSecretManagerClient{
				ProjectID: "test-project",
				Secrets:   map[string]string{"projects/test-project/secrets/github-token/versions/latest": "test-token"},
			})
			factory := NewBuilderFactory(authResolver, time.Second, &logger)
			triggerAuths := []api.TriggerAuthentication{{
				ObjectMeta: metav1.ObjectMeta{Name: "github-auth"},
				Spec: api.TriggerAuthenticationSpec{
					GCPSecretManager: &kedav1alpha1.GCPSecretManager{
						Secrets: []kedav1alpha1.GCPSecretManagerSecret{{Parameter: "personalAccessToken", ID: "github-token", Version: "latest"}},
					},
				},
			}}
			scaledObject := &kedav1alpha1.ScaledObject{
				ObjectMeta: metav1.ObjectMeta{Name: "github-runner"},
				Spec: kedav1alpha1.ScaledObjectSpec{
					ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "github-runner"},
					Triggers: []kedav1alpha1.ScaleTriggers{{
						Type:              "github-runner",
						AuthenticationRef: &kedav1alpha1.AuthenticationRef{Name: "github-auth"},
						Metadata: map[string]string{
							"githubApiURL": server.URL, "owner": "Finni-Health", "repos": "finni", "runnerScope": "repo",
							"labels": "self-hosted", "noDefaultLabels": "true",
							"matchUnlabeledJobsWithUnlabeledRunners": "true", "targetWorkflowQueueLength": "1",
						},
					}},
				},
			}
			ctx := context.Background()
			builders, err := factory.MakeBuilders(ctx, scaledObject, triggerAuths, true)
			require.NoError(t, err)
			require.Len(t, builders, 1)
			defer builders[0].Scaler.Close(ctx)

			state, err := NewStateProvider(&logger).GetScaledObjectState(ctx, scaledObject, builders)
			if testCase.apiStatus != 0 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, state.MetricAndTargetValues, 1)
			assert.Equal(t, testCase.wantDemand, state.MetricAndTargetValues[0].MetricValue)
			assert.Equal(t, testCase.wantDemand > 0, state.IsActive)
		})
	}
}
