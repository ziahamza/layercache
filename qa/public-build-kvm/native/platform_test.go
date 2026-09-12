package native

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestRejectForeignExecutablesWithoutRunningThem(t *testing.T) {
	for _, platform := range []publicbuild.Platform{publicbuild.PlatformLinuxAMD64, publicbuild.PlatformLinuxARM64} {
		t.Run(string(platform), func(t *testing.T) {
			// A minimal ELF with the opposite architecture is enough to catch a
			// mislabeled downloaded tool before QEMU or binfmt can execute it.
			machine := elf.EM_AARCH64
			if platform == publicbuild.PlatformLinuxARM64 {
				machine = elf.EM_X86_64
			}
			header := make([]byte, 64)
			copy(header, "\x7fELF")
			header[4], header[5], header[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
			binary.LittleEndian.PutUint16(header[18:20], uint16(machine))
			binary.LittleEndian.PutUint32(header[20:24], uint32(elf.EV_CURRENT))
			binary.LittleEndian.PutUint16(header[52:54], 64)
			path := filepath.Join(t.TempDir(), "foreign-tool")
			if err := os.WriteFile(path, header, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateExecutable(path, platform, true); err == nil || !strings.Contains(err.Error(), "expected native") {
				t.Fatalf("foreign executable validation = %v", err)
			}
		})
	}
}
