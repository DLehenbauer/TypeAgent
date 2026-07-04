package provider

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure.
// https://learn.microsoft.com/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// probeTotalSystemMemory queries the total physical RAM installed on the
// system, in bytes. The undetermined-memory fallback is applied by
// getTotalSystemMemory, so this returns the raw probe error unchanged.
func probeTotalSystemMemory() (int64, error) {
	var status memoryStatusEx
	status.length = uint32(unsafe.Sizeof(status))
	ret, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0, err
	}
	return int64(status.totalPhys), nil
}
