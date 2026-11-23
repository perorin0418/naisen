package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/session"
)

// 単発コマンド: 通話中のプロセスに制御ソケットでつなぎ、依頼を1つ出す。

func parseFlags(name string, args []string, def func(fs *flag.FlagSet)) (*flag.FlagSet, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	def(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Print(helps[name])
		} else {
			errorf("%v（naisen help %s）", err, name)
		}
		return nil, false
	}
	return fs, true
}

// interrupts は Ctrl-C を取り消しの合図に変える。2回目の Ctrl-C で強制終了する。
func interrupts() <-chan struct{} {
	in := newInput(false)
	cancel := make(chan struct{}, 1)
	go func() {
		n := 0
		for range in.sig {
			n++
			if n >= 2 {
				os.Exit(130)
			}
			fmt.Fprintln(os.Stderr, "\nnaisen: 取り消しています…（もう一度 Ctrl-C で強制終了）")
			cancel <- struct{}{}
		}
	}()
	return cancel
}

// lineFlag は --line（どの電話を使うか）を fs に足す。既定は環境変数 NAISEN_LINE。
func lineFlag(fs *flag.FlagSet) *int {
	def, _ := strconv.Atoi(os.Getenv("NAISEN_LINE"))
	return fs.Int("line", def, "")
}

// lineInfos はこのマシンの電話の状態を集める。
func lineInfos() []proto.LineInfo {
	var infos []proto.LineInfo
	for _, n := range session.Lines() {
		c, err := session.DialLine(n)
		if err != nil {
			continue
		}
		msg, err := session.Simple(c, proto.OpStatus)
		c.Close()
		var info proto.LineInfo
		if err != nil || json.Unmarshal([]byte(msg), &info) != nil {
			continue
		}
		infos = append(infos, info)
	}
	return infos
}

func describe(i proto.LineInfo) string {
	kind := "待ち受け"
	if !i.Waiting {
		kind = "発信"
	}
	if i.InCall {
		return fmt.Sprintf("番号 %d（%s、%s と通話中）", i.Number, kind, i.Peer)
	}
	return fmt.Sprintf("番号 %d（%s、通話していない）", i.Number, kind)
}

// pickLine は使う電話の番号を決める。want が 0 なら、通話中の電話が1つだけのときにそれを選ぶ。
func pickLine(want int) (int, error) {
	if want != 0 {
		return want, nil
	}
	infos := lineInfos()
	var talking []proto.LineInfo
	for _, i := range infos {
		if i.InCall {
			talking = append(talking, i)
		}
	}
	switch {
	case len(talking) == 1:
		return talking[0].Number, nil
	case len(talking) > 1:
		var names []string
		for _, i := range talking {
			names = append(names, "  "+describe(i))
		}
		return 0, fmt.Errorf("通話中の電話が複数あります。--line 番号 で選んでください:\n%s", strings.Join(names, "\n"))
	case len(infos) > 0:
		return 0, fmt.Errorf("%v\n  このマシンの電話: %s", session.ErrNoCall, describe(infos[0]))
	default:
		return 0, session.ErrNoCall
	}
}

func dialOrFail(want int) (net.Conn, bool) {
	n, err := pickLine(want)
	if err != nil {
		errorf("%v", err)
		return nil, false
	}
	c, err := session.DialLine(n)
	if err != nil {
		errorf("%v", err)
		return nil, false
	}
	return c, true
}

func cmdRun(args []string) int {
	var dir string
	var noStdin bool
	var line *int
	fs, ok := parseFlags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&dir, "C", "", "")
		fs.BoolVar(&noStdin, "n", false, "")
		line = lineFlag(fs)
	})
	if !ok {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(os.Stderr, runUsage)
		return 2
	}
	c, ok := dialOrFail(*line)
	if !ok {
		return 255
	}
	defer c.Close()
	var stdin io.Reader
	if !noStdin && !isTerminal(os.Stdin) {
		stdin = os.Stdin
	}
	code, err := session.Run(c, strings.Join(fs.Args(), " "), dir, stdin, os.Stdout, os.Stderr, interrupts())
	if err != nil {
		errorf("%v", err)
		if code == 0 {
			code = 255
		}
	}
	return code
}

func cmdSend(args []string) int { return xferCmd("send", args) }
func cmdGet(args []string) int  { return xferCmd("get", args) }

func xferCmd(name string, args []string) int {
	var force bool
	var line *int
	fs, ok := parseFlags(name, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&force, "f", false, "")
		line = lineFlag(fs)
	})
	if !ok {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprint(os.Stderr, helps[name])
		return 2
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	c, ok := dialOrFail(*line)
	if !ok {
		return 255
	}
	defer c.Close()
	var r session.Result
	var err error
	if name == "send" {
		r, err = session.Send(c, src, dst, force, interrupts())
		reportXfer("送りました", "相手側 ", r, err)
	} else {
		r, err = session.Get(c, src, dst, force, interrupts())
		reportXfer("受け取りました", "", r, err)
	}
	if err != nil {
		return 1
	}
	return 0
}

func cmdStatus(args []string) int {
	var line *int
	if _, ok := parseFlags("status", args, func(fs *flag.FlagSet) { line = lineFlag(fs) }); !ok {
		return 2
	}
	infos := lineInfos()
	if len(infos) == 0 {
		fmt.Println("このマシンで動いている naisen はありません")
		return 1
	}
	found := false
	for _, i := range infos {
		if *line != 0 && i.Number != *line {
			continue
		}
		found = true
		if i.InCall {
			fmt.Println(i.Status)
		} else {
			fmt.Println(describe(i))
		}
	}
	if !found {
		errorf("このマシンに番号 %d の naisen はありません", *line)
		return 1
	}
	return 0
}

func cmdHangup(args []string) int {
	var line *int
	if _, ok := parseFlags("hangup", args, func(fs *flag.FlagSet) { line = lineFlag(fs) }); !ok {
		return 2
	}
	c, ok := dialOrFail(*line)
	if !ok {
		return 255
	}
	defer c.Close()
	if _, err := session.Simple(c, proto.OpHangup); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}
