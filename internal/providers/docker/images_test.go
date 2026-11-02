package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveImagePlatform(t *testing.T) {
	for _, tc := range []struct {
		name, version, initial, afterPull string
		wantPull, wantMismatch            bool
	}{
		{"cached variant", "/v1.49", "amd64", "", false, false},
		{"missing variant", "/v1.49", "", "amd64", true, false},
		{"other variant cached", "/v1.49", "arm64", "amd64", true, false},
		{"wrong variant after pull", "/v1.49", "arm64", "arm64", true, true},
		{"legacy store", "/v1.47", "arm64", "amd64", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arch, pulls := tc.initial, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/images/create") {
					pulls++
					if r.Method != "POST" || r.URL.Query().Get("platform") != "linux/amd64" || r.URL.Query().Get("fromImage") != "runner:1" {
						t.Errorf("incorrect pull: %s %s", r.Method, r.URL)
					}
					arch = tc.afterPull
					w.Write([]byte(`{"status":"done"}`))
					return
				}
				if tc.version == "/v1.49" && strings.Contains(r.URL.Path, "/images/sha256:") {
					var platform map[string]string
					if err := json.Unmarshal([]byte(r.URL.Query().Get("platform")), &platform); err != nil || platform["os"] != "linux" || platform["architecture"] != "amd64" {
						t.Errorf("incorrect inspect platform: %s", r.URL)
					}
				} else if r.URL.Query().Has("platform") {
					t.Error("sent unsupported query to old API")
				}
				if arch == "" {
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:" + arch, "Os": "linux", "Architecture": arch, "Config": map[string]any{"User": "1000", "Env": []string{"TEST=ok"}}})
			}))
			defer server.Close()
			e := &engine{base: server.URL, version: tc.version, client: server.Client()}
			image, err := e.resolveImage(t.Context(), "runner:1", "linux/amd64")
			if errors.Is(err, errImagePlatform) != tc.wantMismatch || (err != nil && !tc.wantMismatch) || (pulls == 1) != tc.wantPull {
				t.Fatalf("image=%+v err=%v pulls=%d", image, err, pulls)
			}
			if err == nil && (image.ID != "sha256:amd64" || image.Config.User != "1000" || len(image.Config.Env) != 1) {
				t.Fatal(image)
			}
		})
	}
}

func TestPullErrorDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errorDetail":{"message":"private registry error"}}`))
	}))
	defer server.Close()
	e := &engine{base: server.URL, client: server.Client()}
	if err := e.pull(context.Background(), "runner:1", "linux/arm64"); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("pull must fail without exposing registry error", err)
	}
}

func TestResolveIndexedImagePinsRootAndSelectsConfiguration(t *testing.T) {
	for _, missingVariant := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "missing variant"}[missingVariant], func(t *testing.T) {
			available := !missingVariant
			pulls, roots := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1.49/images/runner:1/json":
					roots++
					if r.URL.Query().Has("platform") {
						t.Error("platform selection must use immutable root, not mutable tag")
					}
					// The tag's default image is arm64; the requested image is amd64.
					w.Write([]byte(`{"Id":"sha256:index","Os":"linux","Architecture":"arm64","RepoDigests":["runner@sha256:index"]}`))
				case "/v1.49/images/sha256:index/json":
					if !available {
						w.WriteHeader(404)
						return
					}
					var platform map[string]string
					if err := json.Unmarshal([]byte(r.URL.Query().Get("platform")), &platform); err != nil || platform["architecture"] != "amd64" {
						t.Error("missing requested variant")
					}
					w.Write([]byte(`{"Id":"sha256:child","Os":"linux","Architecture":"amd64","Config":{"User":"1234","Env":["ARCH=amd64"]}}`))
				case "/v1.49/images/create":
					pulls++
					available = true
					if r.URL.Query().Get("platform") != "linux/amd64" {
						t.Error("wrong pull platform")
					}
					w.Write([]byte(`{"status":"done"}`))
				default:
					t.Error("unexpected image reference", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			e := &engine{base: server.URL, client: server.Client(), version: "/v1.49"}
			image, err := e.resolveImage(t.Context(), "runner:1", "linux/amd64")
			if err != nil || image.ID != "sha256:index" || image.Architecture != "amd64" || image.Config.User != "1234" || len(image.Config.Env) != 1 || len(image.RepoDigests) != 1 {
				t.Fatal(image, err)
			}
			if (pulls == 1) != missingVariant || roots != 1+pulls {
				t.Fatal("unexpected tag lookups or pulls", roots, pulls)
			}
		})
	}
}
