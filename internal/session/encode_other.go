//go:build !windows

package session

import "io"

// outputWriter はコマンドの出力の書き先を返す。Windows 以外では変換しない。
func outputWriter(w io.Writer) (io.Writer, func()) { return w, func() {} }
