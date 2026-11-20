package config

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func supportsVZ() bool {
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		return false
	}
	major, _, _ := strings.Cut(release, ".")
	version, err := strconv.Atoi(major)
	return err == nil && version >= 23 // Darwin 23 is macOS 14.
}
