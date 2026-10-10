// cache-worker exercises the pinned c2j tool manager inside a real base image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/colony-2/c2j/pkg/toolenv"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	manager, err := toolenv.Default()
	if err != nil {
		return err
	}
	const nixpkgs = "github:NixOS/nixpkgs/b6018f87da91d19d0ab4cf979885689b469cdd41"
	scopes := []toolenv.Scope{
		{ID: "recipe", Packages: []string{"uv:pyfiglet==1.0.2", "pnpm:typescript@5.7.3", "nix:" + nixpkgs + "#gnused"}},
		{ID: "op", Packages: []string{"uv:pyfiglet==1.0.3", "pnpm:typescript@5.8.3", "nix:" + nixpkgs + "#findutils"}},
	}
	env, diag, err := manager.Prepare(ctx, scopes)
	if err != nil {
		return err
	}
	if !env.Ready() {
		return fmt.Errorf("prepared environment not ready")
	}
	if len(os.Args) > 1 && os.Args[1] == "warm" {
		for _, t := range diag.Tools {
			if t.Outcome != "reused" {
				return fmt.Errorf("warm tool reinstalled: %s", t.Reference)
			}
		}
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"pyfiglet", "--version"}, "1.0.3"},
		{[]string{"uvx", "--from", "pyfiglet==1.0.2", "pyfiglet", "--version"}, "1.0.2"},
		{[]string{"tsc", "--version"}, "5.8.3"},
		{[]string{"pnpm", "--package=typescript@5.7.3", "dlx", "tsc", "--version"}, "5.7.3"},
		{[]string{"nix", "run", nixpkgs + "#gnused", "--", "--version"}, "GNU sed"},
		{[]string{"find", "--version"}, "GNU findutils"},
	} {
		cmd := exec.CommandContext(ctx, env.Path+"/"+test.args[0], test.args[1:]...)
		cmd.Env = append(os.Environ(), "PATH="+env.Path+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), test.want) {
			return fmt.Errorf("%v: %v: %s", test.args, err, out)
		}
	}
	// Model an already-published Nix extension output. Cold workers register
	// identical contents concurrently; warm workers must retain its root without
	// invoking Nix or reading an extension source again.
	packageRecord := filepath.Join(manager.Root, "test-nix-op.json")
	var pkg toolenv.NixPackage
	warm := len(os.Args) > 1 && os.Args[1] == "warm"
	if warm {
		data, err := os.ReadFile(packageRecord)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &pkg); err != nil {
			return err
		}
	} else {
		dir, err := os.MkdirTemp("", "op-package-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		// A fixed basename gives concurrent imports the same store path.
		dir = filepath.Join(dir, "fixture-op")
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "share/c2j"), 0700); err != nil {
			return err
		}
		pkg.Manifest = json.RawMessage(`{"name":"fixture","version":"1.0.0","command":["bin/fixture"],"input_schema":{"type":"object"},"output_schema":{"type":"object"}}`)
		if err := os.WriteFile(filepath.Join(dir, "share/c2j/op.json"), pkg.Manifest, 0600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "bin/fixture"), []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"output\":{\"result\":\"packaged-op\"}}'\n"), 0700); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "nix-store", "--add", dir).Output()
		if err != nil {
			return err
		}
		pkg.StorePath = strings.TrimSpace(string(out))
		pkg.System, err = toolenv.NixSystem()
		if err != nil {
			return err
		}
		data, _ := json.Marshal(pkg)
		file, err := os.CreateTemp(manager.Root, "op-record-")
		if err != nil {
			return err
		}
		if _, err = file.Write(data); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		if err = os.Rename(file.Name(), packageRecord); err != nil {
			return err
		}
	}
	root, reused, err := manager.PrepareNixPackage(ctx, pkg)
	if err != nil {
		return err
	}
	if warm && !reused {
		return fmt.Errorf("Nix extension output was not reused")
	}
	cmd := exec.CommandContext(ctx, filepath.Join(root, "bin/fixture"))
	cmd.Stdin = strings.NewReader("{}")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "packaged-op") {
		return fmt.Errorf("Nix extension execution: %v: %s", err, out)
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		toolenv.Diagnostics
		NixOpReused bool `json:"nix_op_reused"`
	}{diag, reused})
}
