package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/pkg/compute"
)

func TestDockerJobDBEndpoint(t *testing.T) {
	for _, tt := range []struct{ kind, source, want string }{
		{"docker", "http://localhost:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://LOCALHOST.:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://127.0.0.1:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://127.0.0.2:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://[::1]:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "https://localhost/c2", "https://host.docker.internal/c2"},
		{"docker", "https://jobdb.example/c2", "https://jobdb.example/c2"},
		{"docker", "http://host.docker.internal:9047/c2", "http://host.docker.internal:9047/c2"},
		{"docker", "http://localhost.example:9047/c2", "http://localhost.example:9047/c2"},
		{"remote", "http://localhost:9047/c2", "http://localhost:9047/c2"},
		{"cloudrun", "http://localhost:9047/c2", "http://localhost:9047/c2"},
	} {
		t.Run(tt.kind+"/"+tt.source, func(t *testing.T) {
			sink := &accept{}
			var logs bytes.Buffer
			provider := dockerJobDBEndpoint(tt.kind, sink, tt.source, slog.New(slog.NewTextHandler(&logs, nil)))
			process, err := c2j.Process(tt.source, "job", compute.Allocation{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			process.Stdin = "private-lease-capability"
			launches := []compute.Launch{{Request: compute.Request{LaunchID: "launch"}, Process: process}}
			if _, err := provider.Submit(context.Background(), launches); err != nil {
				t.Fatal(err)
			}
			if got := sink.process[0].Args[3]; got != tt.want {
				t.Fatalf("worker endpoint: got %s, want %s", got, tt.want)
			}
			if launches[0].Process.Args[3] != tt.source {
				t.Fatal("mutated the command used by other providers")
			}
			if sink.process[0].Stdin != process.Stdin || strings.Contains(logs.String(), string(process.Stdin)) {
				t.Fatal("lease changed or leaked")
			}
			if tt.source != tt.want && (!strings.Contains(logs.String(), tt.source) || !strings.Contains(logs.String(), tt.want)) {
				t.Fatal("missing endpoint diagnostic", logs.String())
			}
			// An unrelated c2j endpoint must not be rewritten by this tenant's wrapper.
			other := process
			other.Args = append([]string(nil), process.Args...)
			other.Args[3] = "http://other.example/tenant"
			if _, err := provider.Submit(context.Background(), []compute.Launch{{Process: other}}); err != nil {
				t.Fatal(err)
			}
			if sink.process[1].Args[3] != other.Args[3] {
				t.Fatal("rewrote another target's endpoint")
			}
		})
	}
}
