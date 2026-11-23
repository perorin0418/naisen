//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach は子プロセスを別のプロセスグループにする（対話プロンプトの Ctrl-C で一緒に止まらないように）。
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
