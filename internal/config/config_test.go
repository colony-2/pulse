package config

import "testing"

const valid = `defaults:
  image: alpine:3
  platform: linux/arm64
  cpu: "1"
  memory: 1Gi
  scratch: 1Gi
providers:
  pool:
    type: remote
    endpoint: https://pool.example
    token_env: POOL_TOKEN
targets:
  - instance_id: test
    jobdb: https://db.example/acme
    cells: [github.com/acme/app]
    launch_services: [{name: pool, priority: 1}]
`

func TestConfig(t *testing.T) {
	c, e := Parse([]byte(valid))
	if e != nil || c.Targets[0].Tenant != "acme" || c.Allocation.MemoryBytes != 1<<30 {
		t.Fatal(c, e)
	}
	if _, e = Parse([]byte(valid + "unknown_option: true\n")); e == nil {
		t.Fatal("unknown field ignored")
	}
	if _, e = Parse([]byte(valid + "---\n{}")); e == nil {
		t.Fatal("multiple documents ignored")
	}
}
