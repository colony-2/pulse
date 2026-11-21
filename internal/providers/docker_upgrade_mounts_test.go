package providers_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// Docker does not promise an order for its inspect Mounts array. Preserve every
// field in each mount while comparing by its unique container destination.
func dockerMountsByDestination(raw string) (map[string]map[string]any, error) {
	var mounts []map[string]any
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		return nil, err
	}
	byDestination := make(map[string]map[string]any, len(mounts))
	for _, mount := range mounts {
		destination, ok := mount["Destination"].(string)
		if !ok || destination == "" || byDestination[destination] != nil {
			return nil, fmt.Errorf("invalid or duplicate mount destination %q", destination)
		}
		byDestination[destination] = mount
	}
	return byDestination, nil
}

func TestDockerHistoricalMountComparison(t *testing.T) {
	before, err := dockerMountsByDestination(`[{"Type":"volume","Name":"cache-old","Source":"/vol/cache-old","Destination":"/cache","RW":true},{"Type":"tmpfs","Destination":"/scratch","RW":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, after string
		equal       bool
	}{
		{"reordered", `[{"RW":true,"Destination":"/scratch","Type":"tmpfs"},{"Destination":"/cache","Source":"/vol/cache-old","RW":true,"Name":"cache-old","Type":"volume"}]`, true},
		{"volume replaced", `[{"Type":"volume","Name":"cache-new","Source":"/vol/cache-new","Destination":"/cache","RW":true},{"Type":"tmpfs","Destination":"/scratch","RW":true}]`, false},
		{"access changed", `[{"Type":"volume","Name":"cache-old","Source":"/vol/cache-old","Destination":"/cache","RW":false},{"Type":"tmpfs","Destination":"/scratch","RW":true}]`, false},
		{"mount removed", `[{"Type":"volume","Name":"cache-old","Source":"/vol/cache-old","Destination":"/cache","RW":true}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after, err := dockerMountsByDestination(tc.after)
			if err != nil || reflect.DeepEqual(before, after) != tc.equal {
				t.Fatalf("mount comparison: equal=%v want=%v error=%v", reflect.DeepEqual(before, after), tc.equal, err)
			}
		})
	}
	for _, raw := range []string{`invalid`, `[{}]`, `[{"Destination":"/cache"},{"Destination":"/cache"}]`} {
		if _, err := dockerMountsByDestination(raw); err == nil {
			t.Fatalf("accepted invalid mounts %s", raw)
		}
	}
}
