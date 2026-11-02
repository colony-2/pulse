package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Docker Official Image: scratch plus the /hello executable. Pin the Linux
// multi-platform index so both probes use reproducible, tiny image variants.
const probeImage = "docker.io/library/hello-world@sha256:5e22040d441e5fb3aed38368acbe8486b575d7018df38dbdfbc7311fbb2ef3a9"
const probeCommand = "/hello"

var errImagePlatform = errors.New("image platform differs")

type imageInfo struct {
	ID           string `json:"Id"`
	RepoDigests  []string
	OS           string `json:"Os"`
	Architecture string
	Variant      string
	Config       struct {
		User string
		Env  []string
	}
}

func (e *engine) platformInspection() bool {
	minor, err := strconv.Atoi(strings.TrimPrefix(e.version, "/v1."))
	return err == nil && minor >= 49
}

func (i imageInfo) matches(platform string) bool {
	parts := strings.Split(platform, "/")
	return len(parts) >= 2 && i.OS == parts[0] && i.Architecture == parts[1] &&
		(len(parts) == 2 || i.Variant == parts[2])
}

// resolveImage never uses a tag's default variant as a substitute for the
// requested platform. Its immutable ID is used for every subsequent create.
func (e *engine) resolveImage(ctx context.Context, ref, platform string) (imageInfo, error) {
	path := "/images/" + url.PathEscape(ref) + "/json"
	query := ""
	if e.platformInspection() {
		parts := strings.SplitN(platform, "/", 3)
		if len(parts) < 2 {
			return imageInfo{}, errImagePlatform
		}
		value := map[string]string{"os": parts[0], "architecture": parts[1]}
		if len(parts) == 3 {
			value["variant"] = parts[2]
		}
		encoded, _ := json.Marshal(value)
		query = "?" + url.Values{"platform": {string(encoded)}}.Encode()
	}
	inspect := func() (imageInfo, error) {
		var root imageInfo
		if err := e.call(ctx, "GET", path, nil, &root); err != nil {
			return imageInfo{}, err
		}
		if root.ID == "" {
			return imageInfo{}, fmt.Errorf("Docker image has no ID")
		}
		if query == "" {
			return root, nil
		}
		var selected imageInfo
		// Containerd stores can return a child manifest ID from platform
		// inspection that is not independently addressable by create. Select
		// from the immutable, locally addressable root ID and create with that
		// root plus the explicit platform. Never re-read a mutable tag here.
		if err := e.call(ctx, "GET", "/images/"+url.PathEscape(root.ID)+"/json"+query, nil, &selected); err != nil {
			return imageInfo{}, err
		}
		if selected.ID == "" {
			return imageInfo{}, fmt.Errorf("Docker platform image has no ID")
		}
		selected.ID = root.ID
		selected.RepoDigests = root.RepoDigests
		return selected, nil
	}
	image, err := inspect()
	// Classic stores may return the other variant instead of a 404. Pull that
	// platform explicitly, then inspect again and verify before any execution.
	if apiStatus(err, 404) || (err == nil && !image.matches(platform)) {
		if err = e.pull(ctx, ref, platform); err != nil {
			return imageInfo{}, err
		}
		image, err = inspect()
	}
	if err != nil {
		return imageInfo{}, err
	}
	if image.ID == "" {
		return imageInfo{}, fmt.Errorf("Docker image has no ID")
	}
	if !image.matches(platform) {
		return imageInfo{}, errImagePlatform
	}
	return image, nil
}

func containerCreatePath(name, platform string) string {
	return "/containers/create?" + url.Values{"name": {name}, "platform": {platform}}.Encode()
}
