//go:build windows

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// shellCommand は cmdline をシェルで実行するコマンドを作る（既定は cmd /C。NAISEN_SHELL で変えられる。
// 例: NAISEN_SHELL="pwsh -NoProfile -Command"）。
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	shell := []string{"cmd", "/C"}
	if v := strings.Fields(os.Getenv("NAISEN_SHELL")); len(v) > 0 {
		shell = v
	}
	cmd := exec.CommandContext(ctx, shell[0], append(shell[1:], cmdline)...)
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(shell[0]), ".exe"))
	if name == "cmd" {
		// cmd.exe は引数の引用符を独自に解釈するので、コマンドラインをそのまま渡す
		// （/S を付けると、外側の引用符だけを外して残りをそのまま実行する）
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CmdLine: shell[0] + ` /S /C "` + cmdline + `"`,
		}
	}
	return cmd
}
