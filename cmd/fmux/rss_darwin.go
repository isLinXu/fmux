//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// procPidTASKINFO selects struct proc_taskinfo from proc_pidinfo(2).
const procPidTASKINFO = 4

// procInfoCallPIDInfo is PROC_INFO_CALL_PIDINFO, the first argument of the
// raw syscall. proc_info(2) takes six arguments -- callnum precedes pid -- and
// omitting it shifts everything by one, which the kernel reports as EINVAL.
const procInfoCallPIDInfo = 2

// procTaskInfo mirrors Darwin's struct proc_taskinfo.
//
// The layout has to match the kernel's exactly: six uint64 counters followed by
// twelve int32 ones, 96 bytes in total. A wrong field order reads a plausible
// number out of the wrong offset, which is worse than an error.
type procTaskInfo struct {
	VirtualSize   uint64
	ResidentSize  uint64
	TotalUser     uint64
	TotalSystem   uint64
	ThreadsUser   uint64
	ThreadsSystem uint64
	Policy        int32
	Faults        int32
	Pageins       int32
	CowFaults     int32
	MessagesSent  int32
	MessagesRecv  int32
	SyscallsMach  int32
	SyscallsUnix  int32
	Csw           int32
	Threadnum     int32
	Numrunning    int32
	Priority      int32
}

// rssMB reads a process's resident size in megabytes.
//
// This asks the kernel directly instead of shelling out to ps: ps is an
// external dependency that is unavailable in restricted environments (and its
// output has to be parsed, which is its own failure mode). proc_pidinfo costs
// one syscall and returns the number in a field.
func rssMB(pid int) (int, error) {
	var info procTaskInfo
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		uintptr(procInfoCallPIDInfo), // callnum
		uintptr(pid),
		uintptr(procPidTASKINFO), // flavor
		0,                        // arg
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(info.ResidentSize / (1024 * 1024)), nil
}
