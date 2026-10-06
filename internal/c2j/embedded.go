package c2j

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/c2j/pkg/joblist"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
)

// Connection describes one explicitly configured remote tenant.
type Connection struct {
	URI      string
	TokenEnv string
}

// Embedded uses c2j discovery and JobDB's public remote lease API. Once constructed it is
// immutable and safe for concurrent calls. Call contexts bound HTTP work.
type Embedded struct {
	clients  map[string]*joblist.Client
	runtimes map[string]*remote.Runtime
}

func NewEmbedded(connections []Connection) (*Embedded, error) {
	e := &Embedded{clients: make(map[string]*joblist.Client), runtimes: make(map[string]*remote.Runtime)}
	tokens := map[string]string{}
	for _, c := range connections {
		if _, exists := e.clients[c.URI]; exists {
			if tokens[c.URI] != c.TokenEnv {
				return nil, fmt.Errorf("conflicting authentication settings for JobDB target")
			}
			continue
		}
		if c.TokenEnv != "" && os.Getenv(c.TokenEnv) == "" {
			return nil, fmt.Errorf("missing JobDB token environment %s", c.TokenEnv)
		}
		httpClient := &http.Client{
			Transport:     tokenTransport{base: http.DefaultTransport, env: c.TokenEnv},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		client, err := joblist.New(joblist.Config{JobDBURI: c.URI, HTTPClient: httpClient})
		if err != nil {
			return nil, err
		}
		e.clients[c.URI], tokens[c.URI] = client, c.TokenEnv
		u, _ := url.Parse(c.URI) // Already validated by joblist.New.
		runtime, err := remote.New((&url.URL{Scheme: u.Scheme, Host: u.Host}).String(), httpClient)
		if err != nil {
			return nil, err
		}
		e.runtimes[c.URI] = runtime
	}
	return e, nil
}

// Claim owns exactly the selected route before any compute is submitted. It
// starts no heartbeat: the receiver takes over before this capability expires.
func (e *Embedded) Claim(ctx context.Context, uri string, job Job, worker string, duration time.Duration) (*Claimed, error) {
	runtime, ok := e.runtimes[uri]
	if !ok || job.Next == nil {
		return nil, fmt.Errorf("invalid lease target")
	}
	lease, err := runtime.GetJobLease(ctx, jobdb.GetJobLeaseRequest{
		JobKey: jobdb.JobKey{TenantId: job.Tenant, JobId: job.ID}, WorkerID: worker,
		Routes: []jobdb.Route{{JobType: job.Next.JobType, TaskType: job.Next.TaskType}}, LeaseDuration: duration,
	})
	if err != nil {
		return nil, errors.New("JobDB lease acquisition failed; any acquired lease will expire")
	}
	if lease == nil {
		return nil, nil
	}
	claimed := &Claimed{Lease: lease, Job: job}
	// The listing contains the immutable submission hints. Current execution
	// demand, when present, must come from the leased snapshot instead.
	var metadata json.RawMessage
	if job.Execution != nil && job.Execution.Initial != nil {
		metadata, _ = json.Marshal(map[string]any{"execution": job.Execution.Initial})
	}
	view := execution.Inspect(metadata, lease.ClientPayload())
	claimed.Job.Execution = &view
	capability, err := remote.ExportLease(lease)
	if err != nil {
		return claimed, errors.New("JobDB lease cannot be exported")
	}
	claimed.Encoded, err = capability.Encode()
	if err != nil {
		return claimed, errors.New("JobDB lease cannot be encoded")
	}
	return claimed, nil
}

type Claimed struct {
	Lease   jobdb.ExecutionLease
	Job     Job
	Encoded []byte
}

func (*Claimed) String() string     { return "claimed job (capability redacted)" }
func (c *Claimed) GoString() string { return c.String() }

// Release is only used after confirmed non-submission. Preserve task coordinates
// and let JobDB persist provisioning backoff across controller instances.
func (c *Claimed) Release(ctx context.Context, backoff time.Duration) error {
	until := time.Now().Add(backoff)
	err := c.Lease.Reschedule(ctx, jobdb.RescheduleExecutionRequest{NextRoute: c.Lease.Route(), TaskWait: c.Lease.ExecutionState().TaskWait, WaitUntil: &until})
	if err != nil {
		return errors.New("JobDB lease release failed; lease will expire")
	}
	return nil
}

func listingQuery(repo, token string) joblist.Query {
	return joblist.Query{
		Repository: repo, JobTypes: []string{"recipe"},
		Statuses: []jobdb.JobStatus{jobdb.JobStatusReady, jobdb.JobStatusCrashConcern},
		PageSize: 100, PageToken: token,
	}
}

// ValidateRepository checks explicit identity syntax without configuration
// discovery, filesystem access, or a network request.
func ValidateRepository(tenant, repo string) error {
	_, err := joblist.BuildRequest(tenant, listingQuery(repo, ""))
	return err
}

func (e *Embedded) List(ctx context.Context, uri, repo, token string) (Page, error) {
	client, ok := e.clients[uri]
	if !ok {
		return Page{}, fmt.Errorf("unconfigured JobDB target")
	}
	p, err := client.List(ctx, listingQuery(repo, token))
	if err != nil {
		return Page{}, err
	}
	out := Page{Jobs: make([]Job, 0, len(p.Jobs)), Next: p.NextPageToken}
	for _, j := range p.Jobs {
		view := j.Execution
		job := Job{Tenant: j.TenantID, ID: j.JobID, Repository: j.RepositorySource,
			Status: string(j.Status), AvailableAt: j.AvailableAt,
			CancelRequested: j.CancelRequested, Execution: &view}
		if j.NextRoute != nil {
			job.Next = &Route{JobType: j.NextRoute.JobType, TaskType: j.NextRoute.TaskType}
		}
		out.Jobs = append(out.Jobs, job)
	}
	return out, nil
}

type tokenTransport struct {
	base http.RoundTripper
	env  string
}

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.env == "" {
		return t.base.RoundTrip(req)
	}
	token := os.Getenv(t.env)
	if token == "" {
		return nil, fmt.Errorf("missing JobDB token environment %s", t.env)
	}
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(copy)
}
