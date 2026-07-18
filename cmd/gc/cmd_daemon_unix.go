//go:build !windows

package main

import "syscall"

// backgroundSysProcAttr returns SysProcAttr for detaching a background child
// from the parent's process group, so it survives parent exit.
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
