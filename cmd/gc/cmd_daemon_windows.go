//go:build windows

package main

import "syscall"

// backgroundSysProcAttr returns nil on Windows (no process group detachment).
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return nil
}
