package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/session"
)

type incoming struct {
	fc *proto.Conn
	h  *proto.Hello
}

func cmdListen(args []string) int {
	fs := flag.NewFlagSet("naisen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bind := fs.String("bind", os.Getenv("NAISEN_BIND"), "")
	number := fs.Int("number", 0, "")
	once := fs.Bool("once", false, "")
	auto := fs.Bool("auto", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if err == nil {
			err = fmt.Errorf("知らないコマンドです: %s", fs.Arg(0))
		}
		errorf("%v（naisen help で使い方を表示します）", err)
		return 2
	}
	if *number != 0 && (*number < proto.MinNumber || *number > proto.MaxNumber) {
		errorf("番号は %d〜%d です", proto.MinNumber, proto.MaxNumber)
		return 2
	}
	if !*auto && !isTerminal(os.Stdin) {
		errorf("着信を受けるかどうかを答えるため、端末から起動してください（自動で取るなら --auto）")
		return 2
	}
	if *bind == "" {
		*bind = "127.0.0.1"
		if *auto && inContainer() {
			// コンテナの中の 127.0.0.1 には、ポートを公開してもホストから届かない
			*bind = "0.0.0.0"
		}
	}

	// 番号を決める: 待ち受けのポート（BasePort+番号）と、制御ソケットの両方が空いている番号
	n, ln, ctl, err := claimWaitingLine(*bind, *number)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer ln.Close()
	defer session.ReleaseLine(n, ctl)
	line := &session.Line{Number: n, Waiting: true}
	go line.Serve(ctl)

	local := session.LocalHello()
	local.Number = n
	local.Auto = *auto

	// 着信は、ここで待っているときだけ受け取る。通話中（や返事を待っている間）に来た着信は話し中で断る。
	// 問い合わせ（電話帳）には、いつでも答える。
	var busy atomic.Bool
	// ネットワークの内線の一覧（naisen list）の問い合わせに、TCP と同じアドレス・ポートの UDP で答える
	if pc, err := net.ListenPacket("udp", ln.Addr().String()); err == nil {
		defer pc.Close()
		go session.ServeDiscovery(pc, func() proto.Hello {
			h := local
			h.Busy = busy.Load()
			return h
		})
	}
	calls := make(chan incoming)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				fc, h, err := session.ReadHello(c)
				if err != nil {
					return
				}
				if h.Probe {
					session.ReplyInfo(fc, local, busy.Load())
					return
				}
				select {
				case calls <- incoming{fc, h}:
				default:
					session.RejectBusy(fc)
				}
			}()
		}
	}()

	in := newInput(isTerminal(os.Stdin))
	logf("あなたの番号は %d です（%s で待っています。終わるには Ctrl-C）", n, net.JoinHostPort(*bind, strconv.Itoa(proto.BasePort+n)))
	if *auto {
		logf("自動で取ります（このポートに届く誰からの着信も、確かめずに受けます）")
		if inContainer() {
			logf("ホストからかけるには、このコンテナをポート %d を公開して作ってください（koya なら -p 127.0.0.1:%d:%d）",
				proto.BasePort+n, proto.BasePort+n, proto.BasePort+n)
		}
	} else {
		logf("koya の中から naisen call %d でかけられます", n)
	}
	for {
		select {
		case <-in.sig:
			fmt.Fprintln(os.Stderr)
			return 0
		case _, ok := <-in.lines:
			if !ok {
				return 0
			}
			logf("番号 %d で着信を待っています（終わるには Ctrl-C）", n)
		case inc := <-calls:
			busy.Store(true)
			fc, err := session.Answer(inc.fc, inc.h, local, func(h *proto.Hello, gone <-chan struct{}) (bool, string) {
				if *auto {
					return autoAccept(h)
				}
				return askAccept(in, h, gone)
			})
			if err != nil {
				logf("%v", err)
			} else {
				talk(fc, local, *inc.h, false, in, line)
			}
			busy.Store(false)
			if err == nil && *once {
				return 0
			}
			logf("番号 %d で着信を待っています（終わるには Ctrl-C）", n)
		}
	}
}

// claimWaitingLine は待ち受ける番号を決め、TCP と制御ソケットで待ち受ける。want が 0 なら空いている最小の番号。
func claimWaitingLine(bind string, want int) (int, net.Listener, net.Listener, error) {
	lo, hi := proto.MinNumber, proto.MaxNumber
	if want != 0 {
		lo, hi = want, want
	}
	var lastErr error
	for n := lo; n <= hi; n++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(proto.BasePort+n)))
		if err != nil {
			lastErr = fmt.Errorf("番号 %d（ポート %d）で待ち受けられません: %v", n, proto.BasePort+n, err)
			continue
		}
		ctl, err := session.ClaimLine(n)
		if err != nil {
			ln.Close()
			lastErr = err
			continue
		}
		return n, ln, ctl, nil
	}
	if want == 0 {
		return 0, nil, nil, fmt.Errorf("空いている番号がありません（%d〜%d）: %v", proto.MinNumber, proto.MaxNumber, lastErr)
	}
	return 0, nil, nil, lastErr
}

// autoAccept は自動で取る内線の着信を受ける。
func autoAccept(h *proto.Hello) (bool, string) {
	logf("自動で着信を受けました: %s@%s（番号 %d、作業ディレクトリ %s）", h.User, h.Host, h.Number, h.Cwd)
	return true, ""
}

// inContainer はコンテナの中で動いているかを返す。
func inContainer() bool {
	if os.Getenv("container") != "" {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// askAccept は着信を受けるかを画面でたずねる。
func askAccept(in *input, h *proto.Hello, gone <-chan struct{}) (bool, string) {
	fmt.Fprintf(os.Stderr, "\n着信: %s@%s（番号 %d、%s", h.User, h.Host, h.Number, h.OS)
	if h.Profile != "" {
		fmt.Fprintf(os.Stderr, "、koya プロファイル %s", h.Profile)
	}
	fmt.Fprintf(os.Stderr, "）\n  作業ディレクトリ: %s\n  確認コード: %s（発信側の画面と同じか確かめてください）\n", h.Cwd, h.Code)
	fmt.Fprintf(os.Stderr, "受けると、相手はこのマシンで %s の権限でコマンドを実行し、ファイルを読み書きできます。\n", session.LocalHello().User)
	fmt.Fprint(os.Stderr, "受けますか？ [y/N]: ")
	timer := time.NewTimer(session.RingTimeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-in.lines:
			if !ok {
				return false, "受けてもらえませんでした"
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return true, ""
			case "n", "no", "":
				return false, "受けてもらえませんでした"
			default:
				fmt.Fprint(os.Stderr, "y か n で答えてください [y/N]: ")
			}
		case <-timer.C:
			fmt.Fprintln(os.Stderr)
			return false, "応答がありませんでした"
		case <-gone:
			fmt.Fprintln(os.Stderr)
			return false, ""
		case <-in.sig:
			fmt.Fprintln(os.Stderr)
			return false, "受けてもらえませんでした"
		}
	}
}

// talk は通話を始め、終わるまで対話プロンプト（または待機）を続ける。
func talk(fc *proto.Conn, local, remote proto.Hello, caller bool, in *input, line *session.Line) {
	s := session.New(fc, local, remote, caller, os.Stderr)
	line.SetSession(s)
	defer line.SetSession(nil)
	go s.Run()
	logf("番号 %d と通話を始めました（相手: %s@%s、作業ディレクトリ %s）", remote.Number, remote.User, remote.Host, remote.Cwd)
	if in.lines != nil {
		logf("help で使い方を表示します。別の端末からも naisen run・send・get・gui が使えます")
		repl(s, in, line.Number)
	} else {
		logf("別の端末から naisen run・send・get・gui が使えます。切るには naisen hangup か Ctrl-C")
		select {
		case <-s.Done():
		case <-in.sig:
			s.Hangup()
		}
	}
	<-s.Done()
	logf("%s", s.Reason())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
