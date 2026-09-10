//go:build linux

package processgroup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// reapGroupZombies reaps adopted zombies from pgid. It deliberately uses
// waitid(P_PID) for identified children instead of wait4(-1): os/exec owns
// the direct child represented by excludePID, and a broad wait could steal
// another command's exit status while several commands run concurrently.
//
// The parent check prevents attempting to reap a process that is still owned
// by another process. The process-group check keeps this cleanup scoped to
// the command whose termination just completed.
func reapGroupZombies(pgid, excludePID int) {
	if pgid <= 1 {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	parentPID := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == excludePID {
			continue
		}
		state, processGroupID, processParentID, ok := processState(pid)
		if !ok || state != 'Z' || processGroupID != pgid || processParentID != parentPID {
			continue
		}
		// The process was observed as our adopted zombie immediately before
		// the wait. If it exits or is reparented between the checks, waitid
		// simply reports that it is not our child anymore.
		_ = unix.Waitid(unix.P_PID, pid, nil, unix.WEXITED|unix.WNOHANG, nil)
	}
}

// processState reads the small, stable subset of /proc/<pid>/stat needed by
// reapGroupZombies. The command name is parenthesized and may contain spaces
// or parentheses, so the final ')' is the delimiter for the remaining fields.
func processState(pid int) (state byte, processGroupID, processParentID int, ok bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, 0, false
	}
	stat := string(data)
	rparen := strings.LastIndexByte(stat, ')')
	if rparen < 0 || rparen+2 >= len(stat) {
		return 0, 0, 0, false
	}
	fields := strings.Fields(stat[rparen+2:])
	// After the comm field, fields are: state (3), ppid (4), ..., pgrp (5).
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, 0, false
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, 0, false
	}
	return fields[0][0], group, parent, true
}
