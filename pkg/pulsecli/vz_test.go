package pulsecli

import "testing"

func TestVZManagementDefaults(t *testing.T) {
	root := NewCommand("test")
	for _, name := range []string{"setup", "upgrade"} {
		cmd, _, err := root.Find([]string{"vz", name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("find vz %s: %v", name, err)
		}
		for flagName, want := range map[string]string{
			"owner":                 "server",
			"macos-docker-capacity": "0",
		} {
			flag := cmd.Flags().Lookup(flagName)
			if flag == nil || flag.Value.String() != want || flag.Changed {
				t.Fatalf("vz %s --%s should default to %q: %+v", name, flagName, want, flag)
			}
		}
	}
}
