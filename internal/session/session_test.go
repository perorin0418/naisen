package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
)

// pair はローカルの TCP で発信・受諾し、両側の通話を返す。
func pair(t *testing.T, accept bool) (host, caller *Session, err error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type ans struct {
		fc *proto.Conn
		h  *proto.Hello
		e  error
	}
	ch := make(chan ans, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- ans{e: err}
			return
		}
		fc, h, err := ReadHello(c)
		if err != nil {
			ch <- ans{e: err}
			return
		}
		fc, err = Answer(fc, h, proto.Hello{Version: proto.Version, User: "host"}, func(*proto.Hello, <-chan struct{}) (bool, string) {
			return accept, "受けてもらえませんでした"
		})
		ch <- ans{fc, h, err}
	}()
	cfc, ch2, err := Call(context.Background(), ln.Addr().String(), proto.Hello{Version: proto.Version, User: "koya", Code: "1234"}, nil)
	a := <-ch
	if err != nil {
		return nil, nil, err
	}
	if a.e != nil {
		t.Fatal(a.e)
	}
	if a.fc == nil {
		t.Fatal("受諾したのに接続がない")
	}
	if a.h.Code != "1234" {
		t.Fatalf("番号が届かない: %+v", a.h)
	}
	host = New(a.fc, proto.Hello{}, *a.h, false, io.Discard)
	caller = New(cfc, proto.Hello{}, *ch2, true, io.Discard)
	go host.Run()
	go caller.Run()
	t.Cleanup(func() { caller.Hangup(); host.Hangup() })
	return host, caller, nil
}

// client は s に対する依頼用の接続（対話プロンプトと同じ net.Pipe）を作る。
func client(s *Session) net.Conn {
	a, b := net.Pipe()
	go s.ServeClient(b, nil)
	return a
}

func TestReject(t *testing.T) {
	_, _, err := pair(t, false)
	if err == nil || !strings.Contains(err.Error(), "受けてもらえません") {
		t.Fatalf("拒否が伝わらない: %v", err)
	}
}

func TestRunBothWays(t *testing.T) {
	host, caller, err := pair(t, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{caller, host} {
		var out, errb bytes.Buffer
		code, err := Run(client(s), "cat; echo err >&2; exit 3", "", strings.NewReader("hello\n"), &out, &errb, nil)
		if err != nil || code != 3 || out.String() != "hello\n" || errb.String() != "err\n" {
			t.Fatalf("code=%d err=%v out=%q stderr=%q", code, err, out.String(), errb.String())
		}
	}
}

func TestRunCancel(t *testing.T) {
	_, caller, err := pair(t, true)
	if err != nil {
		t.Fatal(err)
	}
	cancel := make(chan struct{}, 1)
	go func() { time.Sleep(200 * time.Millisecond); cancel <- struct{}{} }()
	start := time.Now()
	code, _ := Run(client(caller), "sleep 30", "", nil, io.Discard, io.Discard, cancel)
	if code != 130 || time.Since(start) > 5*time.Second {
		t.Fatalf("取り消せない: code=%d %s", code, time.Since(start))
	}
}

func TestSendGet(t *testing.T) {
	host, caller, err := pair(t, true)
	if err != nil {
		t.Fatal(err)
	}
	src, hostDir := t.TempDir(), t.TempDir()
	host.cwd = hostDir // ホスト側の作業ディレクトリ
	big := bytes.Repeat([]byte("0123456789"), 100000)
	os.MkdirAll(filepath.Join(src, "d", "e"), 0o755)
	os.WriteFile(filepath.Join(src, "d", "e", "big.bin"), big, 0o644)

	r, err := Send(client(caller), filepath.Join(src, "d"), "", false, nil)
	if err != nil || r.Path != filepath.Join(hostDir, "d") {
		t.Fatalf("send: %+v %v", r, err)
	}
	got, _ := os.ReadFile(filepath.Join(hostDir, "d", "e", "big.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("中身が違う")
	}
	os.WriteFile(filepath.Join(src, "d", "huge.bin"), bytes.Repeat(big, 20), 0o644) // 送り終わる前に断られる大きさ
	if _, err := Send(client(caller), filepath.Join(src, "d"), "", false, nil); err == nil || !strings.Contains(err.Error(), "すでに") {
		t.Fatalf("上書きを拒否しない: %v", err)
	}
	if _, err := Send(client(caller), filepath.Join(src, "nothing"), "", false, nil); err == nil {
		t.Fatal("ないファイルでエラーにならない")
	}

	back := t.TempDir()
	r, err = Get(client(caller), "d", filepath.Join(back, "copy"), false, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(back, "copy", "e", "big.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("受け取った中身が違う")
	}
	if _, err := Get(client(caller), "nothing", back, false, nil); err == nil {
		t.Fatal("相手側にないファイルでエラーにならない")
	}
	// 既存のディレクトリへは、その中に同じ名前で置く。2回目はすでにある
	if r, err := Get(client(caller), "d", back, false, nil); err != nil || r.Path != filepath.Join(back, "d") {
		t.Fatalf("ディレクトリの中に置けない: %+v %v", r, err)
	}
	if _, err := Get(client(caller), "d", back, false, nil); err == nil || !strings.Contains(err.Error(), "すでに") {
		t.Fatalf("受け取り側の上書きを拒否しない: %v", err)
	}
	// ホストからも受け取れる（逆向き）
	os.WriteFile(filepath.Join(src, "c.txt"), []byte("C"), 0o644)
	caller.cwd = src
	if _, err := Get(client(host), "c.txt", filepath.Join(back, "c2.txt"), false, nil); err != nil {
		t.Fatal(err)
	}
	// 通話はまだ使える
	var out bytes.Buffer
	if code, err := Run(client(caller), "echo ok", "", nil, &out, io.Discard, nil); err != nil || code != 0 || out.String() != "ok\n" {
		t.Fatalf("その後の run が通らない: %d %v %q", code, err, out.String())
	}
}

func TestHangup(t *testing.T) {
	host, caller, err := pair(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Simple(client(caller), proto.OpHangup); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("相手側が切れない")
	}
	if !strings.Contains(host.Reason(), "相手が電話を切りました") {
		t.Fatalf("理由: %s", host.Reason())
	}
	if _, err := Run(client(host), "true", "", nil, io.Discard, io.Discard, nil); err == nil {
		t.Fatal("切れたあとも依頼できる")
	}
}

func TestList(t *testing.T) {
	host, caller, err := pair(t, true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	host.cwd = dir
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("12345"), 0o644)
	os.Mkdir(filepath.Join(dir, "a"), 0o755)
	l, err := List(client(caller), "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != dir || len(l.Entries) != 2 || !l.Entries[0].Dir || l.Entries[1].Name != "b.txt" || l.Entries[1].Size != 5 {
		t.Fatalf("%+v", l)
	}
	if _, err := List(client(caller), "nothing"); err == nil {
		t.Fatal("ないディレクトリでエラーにならない")
	}
}

func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		fc, h, err := ReadHello(c)
		if err != nil || !h.Probe {
			return
		}
		ReplyInfo(fc, proto.Hello{Version: proto.Version, Number: 3, User: "host"}, true)
	}()
	h, err := Probe(context.Background(), ln.Addr().String())
	if err != nil || h.Number != 3 || !h.Busy {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestDiscover(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:7699")
	if err != nil {
		t.Skip("UDP 7699 が使えない:", err)
	}
	defer pc.Close()
	go ServeDiscovery(pc, func() proto.Hello { return proto.Hello{Version: proto.Version, Number: 49, User: "u", Auto: true} })
	got := discover(context.Background(), 300*time.Millisecond, []net.IP{net.IPv4(127, 0, 0, 1)}, 49, 49)
	if len(got) != 1 || got[0].Number != 49 || got[0].Addr != "127.0.0.1" || !got[0].Auto {
		t.Fatalf("%+v", got)
	}
}
