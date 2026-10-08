package docker

import (
	"context"
	"testing"

	"github.com/colony-2/pulse/pkg/compute"
)

func TestHostGatewayMapping(t *testing.T) {
	for _, tt := range []struct {
		name, hostOS, daemonOS, daemonName string
		wantMapping                        bool
	}{
		{"Linux Engine", "linux", "Ubuntu 24.04", "server", true},
		{"macOS Desktop", "darwin", "Docker Desktop", "docker-desktop", false},
		{"macOS Colima", "darwin", "Ubuntu 24.04", "colima", false},
		{"Linux Desktop", "linux", "Docker Desktop", "docker-desktop", false},
		{"Pulse container on Desktop", "linux", "Docker Desktop", "docker-desktop", false},
		{"Pulse container on Colima", "linux", "Ubuntu 24.04", "colima-work", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &daemon{containers: map[string]container{}}
			p, server := testProvider(t, d)
			defer server.Close()
			p.extraHosts = hostGatewayMapping(tt.hostOS, tt.daemonOS, tt.daemonName)
			result, err := p.Submit(context.Background(), launches(requests("network")))
			if err != nil || len(result) != 1 || result[0].Status != compute.Accepted {
				t.Fatal(result, err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			host := d.creates[0]["HostConfig"].(map[string]any)
			extra, exists := host["ExtraHosts"]
			if exists != tt.wantMapping {
				t.Fatalf("ExtraHosts present=%v, want %v", exists, tt.wantMapping)
			}
			if exists {
				values := extra.([]any)
				if len(values) != 1 || values[0] != "host.docker.internal:host-gateway" {
					t.Fatalf("unexpected host mapping: %v", values)
				}
			}
		})
	}
}
