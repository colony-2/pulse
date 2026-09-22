package c2j

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/colony-2/c2j/pkg/joblist"
	"github.com/colony-2/jobdb/pkg/jobdb"
)

// Connection describes one explicitly configured remote tenant.
type Connection struct {
	URI      string
	TokenEnv string
}

// Embedded uses the supported read-only c2j library. Once constructed it is
// immutable and safe for concurrent calls. Call contexts bound HTTP work.
type Embedded struct {
	clients map[string]*joblist.Client
}

func NewEmbedded(connections []Connection) (*Embedded, error) {
	e := &Embedded{clients: make(map[string]*joblist.Client)}
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
		client, err := joblist.New(joblist.Config{
			JobDBURI: c.URI,
			HTTPClient: &http.Client{
				Transport:     tokenTransport{base: http.DefaultTransport, env: c.TokenEnv},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		})
		if err != nil {
			return nil, err
		}
		e.clients[c.URI], tokens[c.URI] = client, c.TokenEnv
	}
	return e, nil
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
