//go:build windows

package procjob

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000

	processTerminate = 0x0001
	processSetQuota  = 0x0100
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW     = kernel32.NewProc("CreateJobObjectW")
	procSetInfoJobObject     = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob   = kernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess          = kernel32.NewProc("OpenProcess")
	procCloseHandle          = kernel32.NewProc("CloseHandle")
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformationStruct struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Manager wraps a Windows Job Object configured to kill all assigned child
// processes automatically when the parent process exits or crashes.
type Manager struct {
	mu     sync.Mutex
	handle syscall.Handle
}

// New creates a Windows Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
func New() (*Manager, error) {
	r1, _, err := procCreateJobObjectW.Call(0, 0)
	if r1 == 0 {
		return nil, fmt.Errorf("CreateJobObjectW: %w", err)
	}
	h := syscall.Handle(r1)

	var info jobObjectExtendedLimitInformationStruct
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose

	r2, _, err2 := procSetInfoJobObject.Call(
		uintptr(h),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r2 == 0 {
		_, _, _ = procCloseHandle.Call(uintptr(h))
		return nil, fmt.Errorf("SetInformationJobObject: %w", err2)
	}

	return &Manager{handle: h}, nil
}

// Assign attaches an existing child process PID to the Job Object.
func (m *Manager) Assign(pid int) error {
	if m == nil || pid <= 0 {
		return nil
	}
	m.mu.Lock()
	hJob := m.handle
	m.mu.Unlock()
	if hJob == 0 {
		return fmt.Errorf("job object closed")
	}

	r1, _, err := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate),
		0,
		uintptr(pid),
	)
	if r1 == 0 {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	hProc := syscall.Handle(r1)
	defer func() {
		_, _, _ = procCloseHandle.Call(uintptr(hProc))
	}()

	r2, _, err2 := procAssignProcessToJob.Call(uintptr(hJob), uintptr(hProc))
	if r2 == 0 {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err2)
	}
	return nil
}

// Close closes the Job Object handle, terminating all assigned child processes.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	h := m.handle
	m.handle = 0
	m.mu.Unlock()
	if h == 0 {
		return nil
	}
	r1, _, err := procCloseHandle.Call(uintptr(h))
	if r1 == 0 {
		return fmt.Errorf("CloseHandle: %w", err)
	}
	return nil
}
