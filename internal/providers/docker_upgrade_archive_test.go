package providers_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerHistoricalArchiveIncludesModuleRoot(t *testing.T) {
	repo := t.TempDir()
	pkg := filepath.Join(repo, "internal", "providers")
	if err := os.MkdirAll(pkg, 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":                        "module historical\n",
		"internal/providers/fixture.go": "package providers\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	tree := git("write-tree")
	// Reproduce the live test's package-directory invocation without a daemon,
	// network access, or historical backend changes.
	archive, err := archiveDockerHistory(context.Background(), pkg, tree)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			files[header.Name] = string(body)
		}
	}
	if files["go.mod"] != "module historical\n" || files["internal/providers/fixture.go"] != "package providers\n" {
		t.Fatalf("historical archive omitted or relocated module files: %v", files)
	}
}
