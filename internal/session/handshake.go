package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
)

// LocalHello はこの側の情報を作る。
func LocalHello() proto.Hello {
	h := proto.Hello{Version: proto.Version, OS: runtime.GOOS, Profile: os.Getenv("KOYA_PROFILE")}
	h.Host, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		h.User = u.Username
	}
	h.Cwd, _ = os.Getwd()
	return h
}

// RingTimeout は相手が出るまで待つ時間。
const RingTimeout = 15 * time.Minute

// Call は addr の naisen に発信し、受諾されるまで待つ。ctx が終わると発信をやめる。
// ringing は呼び出しを始めたとき（つながって、相手の返事を待ち始めたとき）に呼ばれる。
func Call(ctx context.Context, addr string, local proto.Hello, ringing func()) (*proto.Conn, *proto.Hello, error) {
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.THello, Hello: &local}); err != nil {
		c.Close()
		return nil, nil, err
	}
	if ringing != nil {
		ringing()
	}
	_ = c.SetReadDeadline(time.Now().Add(RingTimeout + 15*time.Second))
	f, err := fc.Read()
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("相手の返事を受け取れません: %w", err)
	}
	switch {
	case f.Type == proto.TWelcome && f.Hello != nil:
		return fc, f.Hello, nil
	case f.Type == proto.TReject:
		c.Close()
		return nil, nil, errors.New(f.Msg)
	default:
		c.Close()
		return nil, nil, errors.New("相手が naisen ではないようです")
	}
}

// Decide は着信を受けるかどうかを決める関数。gone は発信側が切ったら閉じる。
// 受けないときは、その理由を返す（発信側に表示される）。
type Decide func(h *proto.Hello, gone <-chan struct{}) (ok bool, why string)

// ReadHello は着信 c の最初のフレーム（発信か問い合わせ）を読む。
func ReadHello(c net.Conn) (*proto.Conn, *proto.Hello, error) {
	fc := proto.NewConn(c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := fc.Read()
	_ = c.SetReadDeadline(time.Time{})
	if err != nil || f.Type != proto.THello || f.Hello == nil {
		c.Close()
		return nil, nil, fmt.Errorf("%s からの接続は naisen の発信ではありませんでした", c.RemoteAddr())
	}
	return fc, f.Hello, nil
}

// ReplyInfo は問い合わせに、この電話の情報を返して切る。
func ReplyInfo(fc *proto.Conn, local proto.Hello, busy bool) {
	defer fc.Close()
	local.Busy = busy
	_ = fc.Raw().SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = fc.Write(&proto.Frame{Type: proto.TInfo, Hello: &local})
}

// Answer は着信 fc（最初のフレーム h は ReadHello で読んだもの）に出て、decide で受けるかを決める。
// 受けたときだけ接続を返す。
func Answer(fc *proto.Conn, h *proto.Hello, local proto.Hello, decide Decide) (*proto.Conn, error) {
	c := fc.Raw()
	if h.Version != proto.Version {
		msg := fmt.Sprintf("naisen の通信の版が違います（相手 %d、発信側 %d）。同じ版の naisen を使ってください", proto.Version, h.Version)
		_ = fc.Write(&proto.Frame{Type: proto.TReject, Msg: msg})
		c.Close()
		return nil, errors.New(msg)
	}

	// 返事を待つ間に発信側が切ったら気づけるように、読み込みを待っておく。
	// 発信側は返事が来るまで何も送らないので、読めたら（またはエラーなら）切られたとみなす。
	gone := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		var b [1]byte
		_, err := c.Read(b[:])
		var ne net.Error
		if !(errors.As(err, &ne) && ne.Timeout()) {
			close(gone)
		}
		close(readDone)
	}()
	ok, why := decide(h, gone)
	_ = c.SetReadDeadline(time.Now()) // 待っている読み込みを止める
	<-readDone
	_ = c.SetReadDeadline(time.Time{})
	select {
	case <-gone:
		c.Close()
		return nil, errors.New("相手が切りました")
	default:
	}
	if !ok {
		_ = fc.Write(&proto.Frame{Type: proto.TReject, Msg: why})
		c.Close()
		return nil, errors.New(why)
	}
	if err := fc.Write(&proto.Frame{Type: proto.TWelcome, Hello: &local}); err != nil {
		c.Close()
		return nil, err
	}
	return fc, nil
}

// RejectBusy は通話中に来た着信を断る。
func RejectBusy(fc *proto.Conn) {
	defer fc.Close()
	_ = fc.Raw().SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = fc.Write(&proto.Frame{Type: proto.TReject, Msg: "話し中です"})
}

// Probe は addr の電話に問い合わせて、その情報を返す（着信にはならない）。
func Probe(ctx context.Context, addr string) (*proto.Hello, error) {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.THello, Hello: &proto.Hello{Version: proto.Version, Probe: true}}); err != nil {
		return nil, err
	}
	f, err := fc.Read()
	if err != nil {
		return nil, err
	}
	if f.Type != proto.TInfo || f.Hello == nil {
		return nil, errors.New("naisen ではありません")
	}
	return f.Hello, nil
}
