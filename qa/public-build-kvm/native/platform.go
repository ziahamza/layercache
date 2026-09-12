// Package native contains host checks shared by the opt-in KVM qualification
// programs. It does not provide an emulated execution path.
package native

import (
	"debug/elf"
	"fmt"
	"runtime"

	"github.com/layercache/layercache/internal/publicbuild"
)

func Platform() publicbuild.Platform {
	return publicbuild.Platform(runtime.GOOS + "/" + runtime.GOARCH)
}

func QEMUPath() string {
	if runtime.GOARCH == "arm64" {
		return "/usr/bin/qemu-system-aarch64"
	}
	return "/usr/bin/qemu-system-x86_64"
}

// ValidateExecutable reads the ELF header without executing the file. Static
// BusyBox is required because the BuildKit fixture starts FROM scratch.
func ValidateExecutable(path string, platform publicbuild.Platform, static bool) error {
	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("inspect native executable %s: %w", path, err)
	}
	defer file.Close()
	want := elf.EM_X86_64
	if platform == publicbuild.PlatformLinuxARM64 {
		want = elf.EM_AARCH64
	} else if platform != publicbuild.PlatformLinuxAMD64 {
		return fmt.Errorf("unsupported native qualification platform %s", platform)
	}
	if file.Class != elf.ELFCLASS64 || file.Machine != want {
		return fmt.Errorf("executable %s is %s/%s, expected native %s", path, file.Class, file.Machine, platform)
	}
	if static {
		for _, program := range file.Progs {
			if program.Type == elf.PT_INTERP {
				return fmt.Errorf("executable %s must be static for the offline scratch fixture", path)
			}
		}
	}
	return nil
}
