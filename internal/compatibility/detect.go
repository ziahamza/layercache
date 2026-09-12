package compatibility

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

var versionNumber = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)

func Detect(ctx context.Context) string {
	parts := []string{runtime.GOOS, canonicalArchitecture(runtime.GOARCH)}
	if runtime.GOOS == "linux" {
		if libc := detectLibc(ctx); libc != "" {
			parts = append(parts, libc)
		}
	} else if runtime.GOOS == "darwin" {
		if version := commandVersion(ctx, "sw_vers", "-productVersion"); version != "" {
			parts = append(parts, "macos"+majorVersion(version))
		}
	}
	if version := commandVersion(ctx, "node", "--version"); version != "" {
		parts = append(parts, "node@"+majorVersion(version))
	}
	if version := strings.TrimPrefix(runtime.Version(), "go"); version != "" {
		parts = append(parts, "go@"+majorMinorVersion(version))
	}
	parts = append(parts, "schema1")
	identity := strings.ToLower(strings.Join(parts, "-"))
	if Validate(identity) != nil {
		return runtime.GOOS + "-" + canonicalArchitecture(runtime.GOARCH) + "-schema1"
	}
	return identity
}

func detectLibc(ctx context.Context) string {
	output := commandOutput(ctx, "ldd", "--version")
	lower := strings.ToLower(output)
	version := versionNumber.FindString(output)
	if version == "" {
		return ""
	}
	if strings.Contains(lower, "musl") {
		return "musl" + majorMinorVersion(version)
	}
	if strings.Contains(lower, "glibc") || strings.Contains(lower, "gnu libc") || strings.Contains(lower, "ubuntu glibc") {
		return "glibc" + majorMinorVersion(version)
	}
	return "libc" + majorMinorVersion(version)
}

func commandVersion(ctx context.Context, name string, arguments ...string) string {
	return versionNumber.FindString(commandOutput(ctx, name, arguments...))
}

func commandOutput(ctx context.Context, name string, arguments ...string) string {
	if _, err := exec.LookPath(name); err != nil {
		return ""
	}
	output, _ := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	return strings.TrimSpace(string(output))
}

func majorVersion(value string) string {
	value = strings.TrimPrefix(value, "v")
	if index := strings.IndexByte(value, '.'); index >= 0 {
		return value[:index]
	}
	return value
}

func majorMinorVersion(value string) string {
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return value
}

func canonicalArchitecture(value string) string {
	if value == "x86_64" || value == "x64" {
		return "amd64"
	}
	if value == "aarch64" {
		return "arm64"
	}
	return value
}
