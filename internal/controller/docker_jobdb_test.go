package controller

import (
	"bytes"
	"context"
	provider "github.com/colony-2/pulse-provider-api"
	"log/slog"
	"strings"
	"testing"
)

func TestDockerJobDBEndpoint(t *testing.T) {
	for _, tt := range []struct{ kind, source, want string }{
		{"docker", "http://localhost:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://LOCALHOST.:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://127.0.0.2:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://[::1]:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "https://localhost/c2", "https://host.docker.internal/c2"},
		{"docker", "https://jobdb.example/c2", "https://jobdb.example/c2"},
		{"docker", "http://localhost.example/c2", "http://localhost.example/c2"},
		{"remote", "http://localhost/c2", "http://localhost/c2"},
		{"cloudrun", "http://localhost/c2", "http://localhost/c2"},
	} {
		t.Run(tt.kind+tt.source, func(t *testing.T) {
			sink := &accept{}
			var logs bytes.Buffer
			p := dockerJobDBEndpoint(tt.kind, sink, tt.source, slog.New(slog.NewTextHandler(&logs, nil)))
			ls := []provider.Launch{{LaunchID: "launch", Job: provider.JobRef{JobDB: tt.source, InstanceID: "db", TenantID: "c2", JobID: "job"}, Lease: "private-lease"}}
			_, err := p.Submit(context.Background(), ls)
			if err != nil {
				t.Fatal(err)
			}
			if sink.requests[0].Job.JobDB != tt.want || ls[0].Job.JobDB != tt.source {
				t.Fatal("incorrect endpoint rewrite or mutation")
			}
			if sink.requests[0].Lease != ls[0].Lease || strings.Contains(logs.String(), string(ls[0].Lease)) {
				t.Fatal("lease changed or leaked")
			}
			if tt.source != tt.want && (!strings.Contains(logs.String(), tt.source) || !strings.Contains(logs.String(), tt.want)) {
				t.Fatal("missing endpoint diagnostic")
			}
			other := ls[0]
			other.Job.JobDB = "http://other.example/c2"
			_, err = p.Submit(context.Background(), []provider.Launch{other})
			if err != nil || sink.requests[1].Job.JobDB != other.Job.JobDB {
				t.Fatal("rewrote another target")
			}
		})
	}
}
