// Derived from KEDA v2.18.3 pkg/scalers/github_runner_scaler.go.
// KEDA authors; licensed under Apache-2.0 (see the repository LICENSE).
// Local change: discover pending workflows, while counting only queued or
// in-progress jobs whose labels match. See README.md in this directory.
package githubrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gha "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/go-logr/logr"
	v2 "k8s.io/api/autoscaling/v2"
	"k8s.io/metrics/pkg/apis/external_metrics"

	"github.com/kedacore/keda/v2/pkg/scalers"
	"github.com/kedacore/keda/v2/pkg/scalers/scalersconfig"
	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

const (
	ORG                  = "org"
	ENT                  = "ent"
	REPO                 = "repo"
	githubDefaultPerPage = 30
)

var reservedLabels = []string{"self-hosted", "linux", "x64"}

type githubRunnerScaler struct {
	metricType    v2.MetricTargetType
	metadata      *githubRunnerMetadata
	httpClient    *http.Client
	logger        logr.Logger
	etags         map[string]string
	previousRepos []string
	previousWfrs  map[string]map[string]*WorkflowRuns
	previousJobs  map[string][]Job
}

type githubRunnerMetadata struct {
	GithubAPIURL                           string   `keda:"name=githubApiURL, order=triggerMetadata;resolvedEnv, default=https://api.github.com"`
	Owner                                  string   `keda:"name=owner, order=triggerMetadata;resolvedEnv"`
	RunnerScope                            string   `keda:"name=runnerScope, order=triggerMetadata;resolvedEnv, enum=org;ent;repo"`
	PersonalAccessToken                    string   `keda:"name=personalAccessToken, order=authParams, optional"`
	Repos                                  []string `keda:"name=repos, order=triggerMetadata;resolvedEnv, optional"`
	Labels                                 []string `keda:"name=labels, order=triggerMetadata;resolvedEnv, optional"`
	NoDefaultLabels                        bool     `keda:"name=noDefaultLabels, order=triggerMetadata;resolvedEnv, default=false"`
	EnableEtags                            bool     `keda:"name=enableEtags, order=triggerMetadata;resolvedEnv, default=false"`
	MatchUnlabeledJobsWithUnlabeledRunners bool     `keda:"name=matchUnlabeledJobsWithUnlabeledRunners, order=triggerMetadata;resolvedEnv, default=false"`
	TargetWorkflowQueueLength              int64    `keda:"name=targetWorkflowQueueLength, order=triggerMetadata;resolvedEnv, default=1"`
	TriggerIndex                           int
	ApplicationID                          int64  `keda:"name=applicationID, order=triggerMetadata;resolvedEnv, optional"`
	InstallationID                         int64  `keda:"name=installationID, order=triggerMetadata;resolvedEnv, optional"`
	ApplicationKey                         string `keda:"name=appKey, order=authParams, optional"`
}

// Reuse KEDA's API response models so the local change only owns scaler logic.
type WorkflowRuns = scalers.WorkflowRuns
type WorkflowRun = scalers.WorkflowRun
type Jobs = scalers.Jobs
type Job = scalers.Job
type Repo = scalers.Repo

// NewGitHubRunnerScaler creates a new GitHub Runner Scaler
func NewGitHubRunnerScaler(config *scalersconfig.ScalerConfig) (scalers.Scaler, error) {
	httpClient := kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, false)

	metricType, err := scalers.GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %w", err)
	}

	meta, err := parseGitHubRunnerMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing GitHub Runner metadata: %w", err)
	}

	if meta.ApplicationID != 0 && meta.InstallationID != 0 && meta.ApplicationKey != "" {
		httpTrans := kedautil.CreateHTTPTransport(false)
		hc, err := gha.New(httpTrans, meta.ApplicationID, meta.InstallationID, []byte(meta.ApplicationKey))
		if err != nil {
			return nil, fmt.Errorf("error creating GitHub App client: %w, \n appID: %d, instID: %d", err, meta.ApplicationID, meta.InstallationID)
		}
		hc.BaseURL = meta.GithubAPIURL
		httpClient = &http.Client{Transport: hc}
	}

	etags := make(map[string]string)
	previousRepos := []string{}
	previousJobs := make(map[string][]Job)
	previousWfrs := make(map[string]map[string]*WorkflowRuns)

	return &githubRunnerScaler{
		metricType:    metricType,
		metadata:      meta,
		httpClient:    httpClient,
		logger:        scalers.InitializeLogger(config, "github_runner_scaler"),
		etags:         etags,
		previousRepos: previousRepos,
		previousJobs:  previousJobs,
		previousWfrs:  previousWfrs,
	}, nil
}

func (meta *githubRunnerMetadata) Validate() error {
	if meta.ApplicationKey == "" && meta.PersonalAccessToken == "" {
		return fmt.Errorf("no personalAccessToken or appKey given")
	}
	if meta.ApplicationID != 0 || meta.InstallationID != 0 || meta.ApplicationKey != "" {
		if err := validateGitHubApp(meta); err != nil {
			return err
		}
	}
	return nil
}

func parseGitHubRunnerMetadata(config *scalersconfig.ScalerConfig) (*githubRunnerMetadata, error) {
	meta := &githubRunnerMetadata{}
	if err := config.TypedConfig(meta); err != nil {
		return nil, fmt.Errorf("error parsing github runner metadata: %w", err)
	}

	meta.TriggerIndex = config.TriggerIndex

	return meta, nil
}

func validateGitHubApp(meta *githubRunnerMetadata) error {
	if meta.ApplicationID == 0 {
		return fmt.Errorf("no applicationID given")
	}
	if meta.InstallationID == 0 {
		return fmt.Errorf("no installationID given")
	}
	if meta.ApplicationKey == "" {
		return fmt.Errorf("no appKey given")
	}
	return nil
}

// getRepositories returns a list of repositories for a given organization, user or enterprise
func (s *githubRunnerScaler) getRepositories(ctx context.Context) ([]string, error) {
	if s.metadata.Repos != nil {
		return s.metadata.Repos, nil
	}

	page := 1
	var repoList []string

	for {
		var url string
		switch s.metadata.RunnerScope {
		case ORG, ENT:
			url = fmt.Sprintf("%s/orgs/%s/repos?page=%s", s.metadata.GithubAPIURL, s.metadata.Owner, strconv.Itoa(page))
		case REPO:
			url = fmt.Sprintf("%s/users/%s/repos?page=%s", s.metadata.GithubAPIURL, s.metadata.Owner, strconv.Itoa(page))
		default:
			return nil, fmt.Errorf("runnerScope %s not supported", s.metadata.RunnerScope)
		}

		body, statusCode, err := s.getGithubRequest(ctx, url, s.metadata, s.httpClient)
		if err != nil {
			return nil, err
		}
		if statusCode == 304 && s.metadata.EnableEtags {
			if s.previousRepos != nil {
				return s.previousRepos, nil
			}

			return nil, fmt.Errorf("request for repositories returned status: %d %s but previous repositories is not set", statusCode, http.StatusText(statusCode))
		}

		var repos []Repo

		err = json.Unmarshal(body, &repos)
		if err != nil {
			return nil, err
		}

		for _, repo := range repos {
			repoList = append(repoList, repo.Name)
		}

		// GitHub returned less than 30 repos per page, so consider no repos left
		if len(repos) < githubDefaultPerPage {
			break
		}

		page++
	}

	if s.metadata.EnableEtags {
		s.previousRepos = repoList
	}

	return repoList, nil
}

func (s *githubRunnerScaler) getGithubRequest(ctx context.Context, url string, metadata *githubRunnerMetadata, httpClient *http.Client) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return []byte{}, -1, err
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if metadata.ApplicationID == 0 && metadata.PersonalAccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+metadata.PersonalAccessToken)
	}

	if s.metadata.EnableEtags {
		if etag, found := s.etags[url]; found {
			req.Header.Set("If-None-Match", etag)
		}
	}

	r, err := httpClient.Do(req)
	if err != nil {
		return []byte{}, -1, err
	}

	b, err := io.ReadAll(r.Body)
	if err != nil {
		return []byte{}, -1, err
	}
	_ = r.Body.Close()

	if r.StatusCode != 200 {
		if r.StatusCode == 304 && s.metadata.EnableEtags {
			s.logger.V(1).Info(fmt.Sprintf("The github rest api for the url: %s returned status %d %s", url, r.StatusCode, http.StatusText(r.StatusCode)))
			return []byte{}, r.StatusCode, nil
		}

		if r.Header.Get("X-RateLimit-Remaining") != "" {
			githubAPIRemaining, _ := strconv.Atoi(r.Header.Get("X-RateLimit-Remaining"))

			if githubAPIRemaining == 0 {
				resetTime, _ := strconv.ParseInt(r.Header.Get("X-RateLimit-Reset"), 10, 64)
				return []byte{}, r.StatusCode, fmt.Errorf("GitHub API rate limit exceeded, resets at %s", time.Unix(resetTime, 0))
			}
		}

		return []byte{}, r.StatusCode, fmt.Errorf("the GitHub REST API returned error. url: %s status: %d response: %s", url, r.StatusCode, string(b))
	}

	if s.metadata.EnableEtags {
		if etag := r.Header.Get("ETag"); etag != "" {
			s.etags[url] = etag
		}
	}

	return b, r.StatusCode, nil
}

func stripDeadRuns(allWfrs []WorkflowRuns) []WorkflowRun {
	var filtered []WorkflowRun
	for _, wfrs := range allWfrs {
		for _, wfr := range wfrs.WorkflowRuns {
			if wfr.Status == "queued" || wfr.Status == "in_progress" || wfr.Status == "pending" {
				filtered = append(filtered, wfr)
			}
		}
	}
	return filtered
}

// getWorkflowRunJobs returns a list of jobs for a given workflow run
func (s *githubRunnerScaler) getWorkflowRunJobs(ctx context.Context, workflowRunID int64, repoName string) ([]Job, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=100", s.metadata.GithubAPIURL, s.metadata.Owner, repoName, workflowRunID)
	body, statusCode, err := s.getGithubRequest(ctx, url, s.metadata, s.httpClient)
	if err != nil {
		return nil, err
	}
	if statusCode == 304 && s.metadata.EnableEtags {
		if s.previousJobs[repoName] != nil {
			return s.previousJobs[repoName], nil
		}

		return nil, fmt.Errorf("request for jobs returned status: %d %s but previous jobs is not set", statusCode, http.StatusText(statusCode))
	}

	var jobs Jobs
	err = json.Unmarshal(body, &jobs)
	if err != nil {
		return nil, err
	}

	if s.metadata.EnableEtags {
		s.previousJobs[repoName] = jobs.Jobs
	}

	return jobs.Jobs, nil
}

// getWorkflowRuns returns a list of workflow runs for a given repository
func (s *githubRunnerScaler) getWorkflowRuns(ctx context.Context, repoName string, status string) (*WorkflowRuns, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=%s&per_page=100", s.metadata.GithubAPIURL, s.metadata.Owner, repoName, status)
	body, statusCode, err := s.getGithubRequest(ctx, url, s.metadata, s.httpClient)
	if err != nil && statusCode == 404 {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if statusCode == 304 && s.metadata.EnableEtags {
		if s.previousWfrs[repoName][status] != nil {
			return s.previousWfrs[repoName][status], nil
		}

		return nil, fmt.Errorf("request for workflow runs returned status: %d %s but previous workflow runs is not set. Repo: %s, Status: %s", statusCode, http.StatusText(statusCode), repoName, status)
	}

	var wfrs WorkflowRuns
	err = json.Unmarshal(body, &wfrs)
	if err != nil {
		return nil, err
	}

	if s.metadata.EnableEtags {
		if _, repoFound := s.previousWfrs[repoName]; !repoFound {
			s.previousWfrs[repoName] = map[string]*WorkflowRuns{status: &wfrs}
		} else {
			s.previousWfrs[repoName][status] = &wfrs
		}
	}

	return &wfrs, nil
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if strings.EqualFold(a, e) {
			return true
		}
	}
	return false
}

// canRunnerMatchLabels check Agent Label array will match runner label array
func (s *githubRunnerScaler) canRunnerMatchLabels(jobLabels []string, runnerLabels []string, noDefaultLabels bool) bool {
	if s.metadata.MatchUnlabeledJobsWithUnlabeledRunners && len(jobLabels) == 0 {
		return len(runnerLabels) == 0
	}
	allLabels := runnerLabels
	if !noDefaultLabels {
		allLabels = append(allLabels, reservedLabels...)
	}
	for _, jobLabel := range jobLabels {
		if !contains(allLabels, jobLabel) {
			return false
		}
	}
	return true
}

// GetWorkflowQueueLength returns the number of workflow jobs in the queue
func (s *githubRunnerScaler) GetWorkflowQueueLength(ctx context.Context) (int64, error) {
	var repos []string
	var err error

	repos, err = s.getRepositories(ctx)
	if err != nil {
		return -1, err
	}

	var allWfrs []WorkflowRuns

	for _, repo := range repos {
		wfrsQueued, err := s.getWorkflowRuns(ctx, repo, "queued")
		if err != nil {
			return -1, err
		}
		if wfrsQueued != nil {
			allWfrs = append(allWfrs, *wfrsQueued)
		}
		wfrsInProgress, err := s.getWorkflowRuns(ctx, repo, "in_progress")
		if err != nil {
			return -1, err
		}
		if wfrsInProgress != nil {
			allWfrs = append(allWfrs, *wfrsInProgress)
		}
		// A workflow waiting on a concurrency group can still contain running
		// jobs in another group. Inspect it without counting pending jobs.
		wfrsPending, err := s.getWorkflowRuns(ctx, repo, "pending")
		if err != nil {
			return -1, err
		}
		if wfrsPending != nil {
			allWfrs = append(allWfrs, *wfrsPending)
		}
	}

	var queueCount int64

	wfrs := stripDeadRuns(allWfrs)
	for _, wfr := range wfrs {
		jobs, err := s.getWorkflowRunJobs(ctx, wfr.ID, wfr.Repository.Name)
		if err != nil {
			return -1, err
		}
		for _, job := range jobs {
			if (job.Status == "queued" || job.Status == "in_progress") && s.canRunnerMatchLabels(job.Labels, s.metadata.Labels, s.metadata.NoDefaultLabels) {
				queueCount++
			}
		}
	}

	return queueCount, nil
}

func (s *githubRunnerScaler) GetMetricsAndActivity(ctx context.Context, metricName string) ([]external_metrics.ExternalMetricValue, bool, error) {
	queueLen, err := s.GetWorkflowQueueLength(ctx)

	if err != nil {
		s.logger.Error(err, "error getting workflow queue length")
		return []external_metrics.ExternalMetricValue{}, false, err
	}

	metric := scalers.GenerateMetricInMili(metricName, float64(queueLen))

	return []external_metrics.ExternalMetricValue{metric}, queueLen >= s.metadata.TargetWorkflowQueueLength, nil
}

func (s *githubRunnerScaler) GetMetricSpecForScaling(_ context.Context) []v2.MetricSpec {
	externalMetric := &v2.ExternalMetricSource{
		Metric: v2.MetricIdentifier{
			Name: scalers.GenerateMetricNameWithIndex(s.metadata.TriggerIndex, kedautil.NormalizeString(fmt.Sprintf("github-runner-%s", s.metadata.Owner))),
		},
		Target: scalers.GetMetricTarget(s.metricType, s.metadata.TargetWorkflowQueueLength),
	}
	metricSpec := v2.MetricSpec{External: externalMetric, Type: v2.ExternalMetricSourceType}
	return []v2.MetricSpec{metricSpec}
}

func (s *githubRunnerScaler) Close(_ context.Context) error {
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	return nil
}
