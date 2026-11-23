//go:build !windows

package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// shellCommand は cmdline をシェルで実行するコマンドを作る（既定は sh -c。NAISEN_SHELL で変えられる）。
// 取り消したときは、子プロセスもまとめて止めるため、プロセスグループごとに止める。
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	shell := []string{"sh", "-c"}
	if v := strings.Fields(os.Getenv("NAISEN_SHELL")); len(v) > 0 {
		shell = v
	}
	cmd := exec.CommandContext(ctx, shell[0], append(shell[1:], cmdline)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
