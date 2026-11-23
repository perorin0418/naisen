package session

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
)

// ErrNoCall は通話中でないときのエラー。
var ErrNoCall = errors.New("通話中ではありません（先に、ホストでは naisen、koya の中では naisen call で通話を始めてください）")

// SockDir は制御ソケットを置くディレクトリを返す。電話ごとに <番号>.sock を作り、単発コマンドや gui がつなぐ。
// 環境変数 NAISEN_SOCK_DIR で変えられる。
func SockDir() string {
	if d := os.Getenv("NAISEN_SOCK_DIR"); d != "" {
		return d
	}
	dir := os.TempDir()
	if runtime.GOOS != "windows" {
		if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
			dir = d
		}
	}
	name := "user"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	name = regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(name, "_")
	return filepath.Join(dir, "naisen-"+name)
}

func sockPath(n int) string { return filepath.Join(SockDir(), strconv.Itoa(n)+".sock") }

func alive(p string) bool {
	c, err := net.DialTimeout("unix", p, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ClaimLine は番号 n の制御ソケットで待ち受ける。ほかの電話が使っていればエラーにする。
func ClaimLine(n int) (net.Listener, error) {
	dir := SockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := sockPath(n)
	if alive(p) {
		return nil, fmt.Errorf("番号 %d はこのマシンのほかの naisen が使っています", n)
	}
	_ = os.Remove(p) // 前回の残り
	l, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(p, 0o600)
	return l, nil
}

// ReleaseLine は番号 n の制御ソケットを片付ける。
func ReleaseLine(n int, l net.Listener) {
	l.Close()
	_ = os.Remove(sockPath(n))
}

// Lines はこのマシン（このユーザー）で動いている電話の番号を返す。
func Lines() []int {
	ents, err := os.ReadDir(SockDir())
	if err != nil {
		return nil
	}
	var ns []int
	for _, e := range ents {
		s, ok := strings.CutSuffix(e.Name(), ".sock")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < proto.MinNumber || n > proto.MaxNumber {
			continue
		}
		if alive(sockPath(n)) {
			ns = append(ns, n)
		}
	}
	sort.Ints(ns)
	return ns
}

// DialLine は番号 n の電話につなぐ。
func DialLine(n int) (net.Conn, error) {
	c, err := net.DialTimeout("unix", sockPath(n), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("このマシンに番号 %d の naisen はありません", n)
	}
	return c, nil
}
