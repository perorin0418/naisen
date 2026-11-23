package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/perorin0418/naisen/internal/session"
)

const replHelp = `対話プロンプトのコマンド:
  run COMMAND                 相手側でコマンドを実行する（行の残りをそのまま相手のシェルに渡す）
  send [-f] LOCAL [REMOTE]    こちらのファイル・ディレクトリを相手側に送る
  get [-f] REMOTE [LOCAL]     相手側のファイル・ディレクトリを受け取る
  status                      通話の状態を表示する
  gui                         ドラッグ&ドロップでやり取りするウィンドウを開く（naisen gui と同じ）
  help                        この説明を表示する
  bye                         電話を切る（exit・quit・Ctrl-D でも切れます）

  -f は、置き先にすでにあれば上書きします。相手側の相対パスは相手の作業ディレクトリが基準です。
  空白を含むパスは ' か " で囲みます。実行中の run・send・get は Ctrl-C で止めます。
  相手側のコマンドに標準入力は渡しません（渡すときは別の端末から naisen run を使います）。
`

// repl は通話中の対話プロンプト。電話が切れるか、bye で戻る。number は自分の番号。
func repl(s *session.Session, in *input, number int) {
	prompt := fmt.Sprintf("naisen(%d)> ", number)
	for {
		fmt.Fprint(os.Stderr, prompt)
		select {
		case <-s.Done():
			fmt.Fprintln(os.Stderr)
			return
		case <-in.sig:
			fmt.Fprintln(os.Stderr, "\n（電話を切るには bye）")
		case line, ok := <-in.lines:
			if !ok {
				fmt.Fprintln(os.Stderr)
				s.Hangup()
				return
			}
			if quit := replLine(s, in, line, number); quit {
				s.Hangup()
				return
			}
		}
	}
}

func replLine(s *session.Session, in *input, line string, number int) (quit bool) {
	line = strings.TrimSpace(line)
	name, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch name {
	case "":
	case "bye", "exit", "quit", "hangup":
		return true
	case "help", "?":
		fmt.Fprint(os.Stderr, replHelp)
	case "status":
		fmt.Fprintln(os.Stderr, s.Status())
	case "gui":
		startGui(number)
	case "run":
		if rest == "" {
			fmt.Fprintln(os.Stderr, "使い方: run COMMAND")
			return false
		}
		doOp(s, in, func(c net.Conn, cancel <-chan struct{}) {
			code, err := session.Run(c, rest, "", nil, os.Stdout, os.Stderr, cancel)
			if err != nil {
				errorf("%v", err)
			} else if code != 0 {
				fmt.Fprintf(os.Stderr, "（終了コード %d）\n", code)
			}
		})
	case "send", "get":
		args, err := splitArgs(rest)
		if err != nil {
			errorf("%v", err)
			return false
		}
		force := false
		if len(args) > 0 && args[0] == "-f" {
			force, args = true, args[1:]
		}
		if len(args) < 1 || len(args) > 2 {
			if name == "send" {
				fmt.Fprintln(os.Stderr, "使い方: send [-f] LOCAL [REMOTE]")
			} else {
				fmt.Fprintln(os.Stderr, "使い方: get [-f] REMOTE [LOCAL]")
			}
			return false
		}
		src, dst := args[0], ""
		if len(args) == 2 {
			dst = args[1]
		}
		doOp(s, in, func(c net.Conn, cancel <-chan struct{}) {
			if name == "send" {
				r, err := session.Send(c, src, dst, force, cancel)
				reportXfer("送りました", "相手側 ", r, err)
			} else {
				r, err := session.Get(c, src, dst, force, cancel)
				reportXfer("受け取りました", "", r, err)
			}
		})
	default:
		fmt.Fprintf(os.Stderr, "知らないコマンドです: %s（help で使い方を表示します）\n", name)
	}
	return false
}

func reportXfer(done, where string, r session.Result, err error) {
	if err != nil {
		errorf("%v", err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s → %s%s（%s）\n", done, where, r.Path, r.Stats)
}

// doOp は依頼を1つ実行する。実行中の Ctrl-C は依頼の取り消しにする。
func doOp(s *session.Session, in *input, fn func(c net.Conn, cancel <-chan struct{})) {
	a, b := net.Pipe()
	go s.ServeClient(b, nil)
	cancel := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(a, cancel)
	}()
	for {
		select {
		case <-done:
			a.Close()
			return
		case <-in.sig:
			fmt.Fprintln(os.Stderr, "\n（取り消しています…）")
			select {
			case cancel <- struct{}{}:
			default:
			}
		}
	}
}

// startGui は、この電話（番号 number）の naisen gui を別のプロセスで起動する（対話プロンプトはそのまま使える）。
func startGui(number int) {
	exe, err := os.Executable()
	if err != nil {
		errorf("%v", err)
		return
	}
	cmd := exec.Command(exe, "gui", "--line", strconv.Itoa(number))
	cmd.Stderr = os.Stderr
	detach(cmd)
	if err := cmd.Start(); err != nil {
		errorf("naisen gui を起動できません: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
