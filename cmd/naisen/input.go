package main

import (
	"bufio"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// input は端末からの入力行と Ctrl-C をまとめて受け取る。
type input struct {
	lines <-chan string // 標準入力を読まないときは nil
	sig   <-chan os.Signal
}

func newInput(readLines bool) *input {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	in := &input{sig: sig}
	if readLines {
		ch := make(chan string)
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				ch <- sc.Text()
			}
			close(ch)
		}()
		in.lines = ch
	}
	return in
}

// isTerminal は f が端末かを返す。/dev/null もデバイスファイルなので、ファイルの種類では判定できない。
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// splitArgs は対話プロンプトの1行を、シェルのように空白で分ける（' と " で囲める。\ で次の1文字をそのまま）。
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				esc = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == '\\':
			esc = true
			inArg = true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errQuote
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }

const errQuote = strErr("引用符が閉じていません")
