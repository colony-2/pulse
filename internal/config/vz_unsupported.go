//go:build !darwin || !arm64

package config

func supportsVZ() bool { return false }
