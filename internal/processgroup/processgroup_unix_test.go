//go:build !windows

package processgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"golang.org/x/sys/unix"
)

func TestTerminateEscalatesToSIGKILL(t *testing.T) {
	killed := false
	var signals []syscall.Signal
	opts := Options{
		CurrentGroupID: func() int { return 12345 },
		PollPeriod:     time.Millisecond,
		Kill: func(_ int, sig syscall.Signal) error {
			switch sig {
			case syscall.SIGTERM, syscall.SIGKILL:
				signals = append(signals, sig)
				if sig == syscall.SIGKILL {
					killed = true
				}
				return nil
			case 0:
				if killed {
					return syscall.ESRCH
				}
				return nil
			default:
				t.Fatalf("unexpected signal %v", sig)
				return nil
			}
		},
	}

	if err := Terminate(45678, 0, opts); err != nil {
		t.Fatalf("Terminate() error = %v, want nil", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}

func TestTerminateTreatsESRCHAsAlreadyStopped(t *testing.T) {
	opts := Options{
		CurrentGroupID: func() int { return 12345 },
		Kill: func(_ int, _ syscall.Signal) error {
			return syscall.ESRCH
		},
	}

	if err := Terminate(45678, time.Millisecond, opts); err != nil {
		t.Fatalf("Terminate() ESRCH error = %v, want nil", err)
	}
}

func TestTerminateRefusesCurrentProcessGroup(t *testing.T) {
	opts := Options{CurrentGroupID: func() int { return 45678 }}

	if err := Terminate(45678, time.Millisecond, opts); err == nil {
		t.Fatal("Terminate() current process group error = nil, want refusal")
	}
}

func TestTerminateCommandPreservesGroupFailureAfterDirectKill(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	err := TerminateCommand(cmd, syscall.Getpgrp(), time.Millisecond, Options{})
	if err == nil {
		t.Fatal("TerminateCommand() error = nil, want unsafe process group error")
	}
	if !strings.Contains(err.Error(), "refusing to signal unsafe process group") {
		t.Fatalf("TerminateCommand() error = %v, want unsafe process group detail", err)
	}
	_ = cmd.Wait()
}

func TestTerminateCommandReapsAdoptedGroupZombies(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("child subreaper unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })

	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 30 & echo $! > "+childPath+"; wait")
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("timed out waiting for child pid")
	}

	_ = TerminateCommand(cmd, cmd.Process.Pid, 100*time.Millisecond, Options{})
	_ = cmd.Wait()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(childPID))); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("adopted process %d was not reaped", childPID)
}
