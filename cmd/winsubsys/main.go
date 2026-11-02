// Command winsubsys lowers the PE optional header version fields of a
// Go-built Windows binary so the loader accepts it on Windows 8 / Server 2012
// R2.
//
// Since Go 1.21 the linker marks binaries as requiring Windows 10
// (MajorSubsystemVersion 10.0). Windows compares that value with its own
// version and refuses to start the process with "This app can't run on your
// PC" before any code runs, so the import table alone is not enough. The
// runtime does not read these fields; only the loader does.
package main

import (
	"debug/pe"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

// Optional header offsets, identical for PE32 and PE32+ up to the subsystem
// version: Magic(2) LinkerVersion(2) SizeOfCode(4) SizeOfInitializedData(4)
// SizeOfUninitializedData(4) AddressOfEntryPoint(4) BaseOfCode(4)
// [PE32 only: BaseOfData(4)] ImageBase(4|8) SectionAlignment(4) FileAlignment(4)
// MajorOperatingSystemVersion(2) MinorOperatingSystemVersion(2)
// MajorImageVersion(2) MinorImageVersion(2) MajorSubsystemVersion(2)
// MinorSubsystemVersion(2)
const (
	osVersionOffset        = 40
	subsystemVersionOffset = 48
	targetMajor            = 6
	targetMinor            = 2
)

func main() {
	binaryPath := flag.String("binary", "", "path to the Windows executable to patch")
	flag.Parse()
	if *binaryPath == "" {
		fmt.Fprintln(os.Stderr, "usage: winsubsys -binary <path-to-exe>")
		os.Exit(2)
	}
	if err := patch(*binaryPath); err != nil {
		fmt.Fprintf(os.Stderr, "winsubsys: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("winsubsys: %s: header now requires Windows %d.%02d\n", *binaryPath, targetMajor, targetMinor)
}

func patch(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// e_lfanew lives at 0x3c; the optional header follows the 4-byte signature
	// and the 20-byte file header.
	if len(data) < 0x40 {
		return fmt.Errorf("%s: file too small to be a PE image", path)
	}
	optOff := int(binary.LittleEndian.Uint32(data[0x3c:])) + 4 + 20
	if optOff+subsystemVersionOffset+4 > len(data) {
		return fmt.Errorf("%s: optional header out of range", path)
	}
	magic := binary.LittleEndian.Uint16(data[optOff:])
	if magic != 0x10b && magic != 0x20b {
		return fmt.Errorf("%s: unexpected optional header magic 0x%x", path, magic)
	}

	binary.LittleEndian.PutUint16(data[optOff+osVersionOffset:], targetMajor)
	binary.LittleEndian.PutUint16(data[optOff+osVersionOffset+2:], 0)
	binary.LittleEndian.PutUint16(data[optOff+subsystemVersionOffset:], targetMajor)
	binary.LittleEndian.PutUint16(data[optOff+subsystemVersionOffset+2:], targetMinor)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := os.WriteFile(path, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
