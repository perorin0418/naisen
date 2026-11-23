package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/session"
)

// parseTarget は「番号」「番号@ホスト」「@ホスト」を分ける。番号を省けば 0。
func parseTarget(s string) (int, string, error) {
	num, host, _ := strings.Cut(s, "@")
	n := 0
	if num != "" {
		v, err := strconv.Atoi(num)
		if err != nil || v < proto.MinNumber || v > proto.MaxNumber {
			return 0, "", fmt.Errorf("番号は %d〜%d の数字です: %s", proto.MinNumber, proto.MaxNumber, num)
		}
		n = v
	}
	return n, host, nil
}

// hostCandidates はかける先のホストの候補を返す。
// コンテナの中ならホスト（host.containers.internal・host.docker.internal）とこのコンテナ自身、
// ホストなら 127.0.0.1（このマシンの内線と、ポートを公開したコンテナの内線）。
func hostCandidates(host string) []string {
	if host != "" {
		return []string{host}
	}
	if h := os.Getenv("NAISEN_HOST"); h != "" {
		return []string{h}
	}
	if inContainer() {
		return []string{"host.containers.internal", "host.docker.internal", "127.0.0.1"}
	}
	return []string{"127.0.0.1"}
}

func addr(host string, n int) string {
	return net.JoinHostPort(host, strconv.Itoa(proto.BasePort+n))
}

// phonebook はホストの候補の番号 1〜49 に TCP で問い合わせ、見つかった内線を返す（Addr にホスト）。
// 同じ IP アドレスに解決される候補は1回だけ調べる。
func phonebook(ctx context.Context, hosts []string) []*proto.Hello {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []*proto.Hello
	seenIP := map[string]bool{}
	for _, h := range hosts {
		ips, err := net.DefaultResolver.LookupHost(ctx, h)
		if err != nil || len(ips) == 0 || seenIP[ips[0]] {
			continue
		}
		seenIP[ips[0]] = true
		for n := proto.MinNumber; n <= proto.MaxNumber; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				info, err := session.Probe(ctx, addr(h, n))
				if err != nil {
					return
				}
				info.Number, info.Addr = n, h
				mu.Lock()
				found = append(found, info)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sortPhones(found)
	return found
}

func sortPhones(list []*proto.Hello) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Addr != list[j].Addr {
			return list[i].Addr < list[j].Addr
		}
		return list[i].Number < list[j].Number
	})
}

func phoneState(h *proto.Hello) string {
	st := "待ち受け中"
	if h.Busy {
		st = "話し中"
	}
	if h.Version != proto.Version {
		st = fmt.Sprintf("版が違う（%d）", h.Version)
	}
	return st
}

func autoMark(h *proto.Hello) string {
	switch {
	case h.Auto:
		return "自動"
	}
	return "手で取る"
}

func printPhonebook(list []*proto.Hello) {
	rows := [][]string{{"かけ方", "状態", "取り方", "相手", "作業ディレクトリ"}}
	for _, h := range list {
		rows = append(rows, []string{fmt.Sprintf("%d@%s", h.Number, h.Addr), phoneState(h), autoMark(h), h.User + "@" + h.Host, h.Cwd})
	}
	printTable(os.Stderr, rows)
}

// printTable は表を、全角文字を2文字分の幅として列をそろえて表示する。
func printTable(w io.Writer, rows [][]string) {
	widths := map[int]int{}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], dispWidth(c))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-dispWidth(c)+2))
			}
		}
		fmt.Fprintln(w, b.String())
	}
}

// dispWidth は端末での表示幅（全角文字は2）を返す。
func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) || (r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe4f) || (r >= 0xff00 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1f64f) || (r >= 0x20000 && r <= 0x3fffd)):
			n += 2
		default:
			n++
		}
	}
	return n
}

func cmdList(args []string) int {
	var local bool
	fs, ok := parseFlags("list", args, func(fs *flag.FlagSet) { fs.BoolVar(&local, "local", false, "") })
	if !ok {
		return 2
	}
	host := ""
	if fs.NArg() > 0 {
		host = strings.TrimPrefix(fs.Arg(0), "@")
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	var tcp, udp []*proto.Hello
	wg.Add(1)
	go func() { defer wg.Done(); tcp = phonebook(ctx, hostCandidates(host)) }()
	if !local && host == "" {
		wg.Add(1)
		go func() { defer wg.Done(); udp = session.Discover(ctx, 800*time.Millisecond) }()
	}
	wg.Wait()
	list := mergePhones(tcp, udp)
	if len(list) == 0 {
		errorf("内線が見つかりません")
		return 1
	}
	printPhonebook(list)
	fmt.Fprintln(os.Stderr, "\nかけるには naisen call <かけ方>（例: naisen call "+fmt.Sprintf("%d@%s", list[0].Number, list[0].Addr)+"）")
	return 0
}

// mergePhones は TCP と UDP で見つけた内線をまとめる。同じ内線（同じユーザー・ホスト・番号・作業ディレクトリ）は
// TCP で見つけたほうを残す。
func mergePhones(tcp, udp []*proto.Hello) []*proto.Hello {
	key := func(h *proto.Hello) string {
		return fmt.Sprintf("%s|%s|%d|%s", h.User, h.Host, h.Number, h.Cwd)
	}
	seen := map[string]bool{}
	var out []*proto.Hello
	for _, h := range tcp {
		seen[key(h)] = true
		out = append(out, h)
	}
	var rest []*proto.Hello
	for _, h := range udp {
		if !seen[key(h)] {
			seen[key(h)] = true
			rest = append(rest, h)
		}
	}
	sortPhones(rest)
	return append(out, rest...)
}

func cmdCall(args []string) int {
	fs, ok := parseFlags("call", args, func(fs *flag.FlagSet) {})
	if !ok {
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		errorf("引数が多すぎます（naisen help call）")
		return 2
	}
	target := ""
	if len(rest) == 1 {
		target = rest[0]
	}
	num, host, err := parseTarget(target)
	if err != nil {
		errorf("%v", err)
		return 2
	}
	hosts := hostCandidates(host)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := newInput(isTerminal(os.Stdin))

	// 番号を省いたら、待っている内線を探す
	if num == 0 {
		list := phonebook(ctx, hosts)
		var free []*proto.Hello
		for _, x := range list {
			if !x.Busy && x.Version == proto.Version {
				free = append(free, x)
			}
		}
		switch {
		case len(list) == 0:
			errorf("待っている内線が見つかりません（ホストで naisen を起動しているか確かめてください）")
			return 1
		case len(free) == 1:
			num, hosts = free[0].Number, []string{free[0].Addr}
		default:
			printPhonebook(list)
			if len(free) == 0 {
				errorf("空いている内線がありません")
			} else {
				errorf("かける先を指定してください（例: naisen call %d@%s）", free[0].Number, free[0].Addr)
			}
			return 1
		}
	}

	// 自分の番号（このマシンで単発コマンドや gui から選ぶためのもの）
	me, ctl, err := claimCallerLine()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	defer session.ReleaseLine(me, ctl)
	line := &session.Line{Number: me}
	go line.Serve(ctl)

	local := session.LocalHello()
	local.Number = me
	local.Code = randomCode()

	type result struct {
		fc     *proto.Conn
		remote *proto.Hello
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		var errs []string
		for _, h := range hosts {
			a := addr(h, num)
			fc, remote, err := session.Call(ctx, a, local, func() {
				logf("番号 %d（%s）にかけています… 確認コード %s（相手の画面に同じコードが出ます。やめるには Ctrl-C）", num, a, local.Code)
			})
			if err == nil {
				resCh <- result{fc, remote, nil}
				return
			}
			var oe *net.OpError
			var de *net.DNSError
			if (errors.As(err, &oe) && oe.Op == "dial") || errors.As(err, &de) {
				errs = append(errs, fmt.Sprintf("%s: %v", a, err))
				continue // つながらなければ次の候補へ
			}
			resCh <- result{err: err}
			return
		}
		resCh <- result{err: fmt.Errorf("番号 %d につながりません（その番号の naisen が起動しているか、naisen list で確かめてください）\n  %s", num, strings.Join(errs, "\n  "))}
	}()

	var r result
	select {
	case r = <-resCh:
	case <-in.sig:
		cancel()
		<-resCh
		fmt.Fprintln(os.Stderr)
		errorf("発信をやめました")
		return 1
	}
	if r.err != nil {
		errorf("%v", r.err)
		return 1
	}
	r.remote.Number = num
	talk(r.fc, local, *r.remote, true, in, line)
	return 0
}

// claimCallerLine は発信する電話の番号を決める（このマシンで空いている最小の番号）。
func claimCallerLine() (int, net.Listener, error) {
	for n := proto.MinNumber; n <= proto.MaxNumber; n++ {
		if ctl, err := session.ClaimLine(n); err == nil {
			return n, ctl, nil
		}
	}
	return 0, nil, fmt.Errorf("空いている番号がありません（%d〜%d）", proto.MinNumber, proto.MaxNumber)
}

func randomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "0000"
	}
	return fmt.Sprintf("%04d", n.Int64())
}
