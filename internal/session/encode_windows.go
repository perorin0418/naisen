//go:build windows

package session

import (
	"io"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var procIsDBCSLeadByteEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsDBCSLeadByteEx")

type winCP uint32

func (cp winCP) isLead(b byte) bool {
	r, _, _ := procIsDBCSLeadByteEx.Call(uintptr(cp), uintptr(b))
	return r != 0
}

func (cp winCP) decode(p []byte) []byte {
	if len(p) == 0 {
		return nil
	}
	n, err := windows.MultiByteToWideChar(uint32(cp), 0, &p[0], int32(len(p)), nil, 0)
	if err != nil || n == 0 {
		return p
	}
	u := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(uint32(cp), 0, &p[0], int32(len(p)), &u[0], n); err != nil {
		return p
	}
	return []byte(string(utf16.Decode(u)))
}

// outputWriter はコマンドの出力を UTF-8 にそろえる書き先を返す。
// cmd などはコンソールのコードページ（日本語の環境なら CP932）で出力するため。
// 2つ目の戻り値は、コマンドが終わったときに呼ぶ。
func outputWriter(w io.Writer) (io.Writer, func()) {
	cp, err := windows.GetConsoleOutputCP()
	if err != nil || cp == 0 {
		cp = windows.GetACP()
	}
	if cp == 65001 { // UTF-8
		return w, func() {}
	}
	u := &utf8Writer{w: w, cp: winCP(cp)}
	return u, u.Flush
}
