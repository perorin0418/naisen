package session

import (
	"encoding/json"
	"io"
	"net"
	"sync"

	"github.com/perorin0418/naisen/internal/proto"
)

// Line は1つの電話（naisen か naisen call の1プロセス）。制御ソケットで単発コマンドや gui からの依頼を受け、
// 通話中ならその通話に渡す。
type Line struct {
	Number  int
	Waiting bool // 着信を待つ電話か

	mu   sync.Mutex
	sess *Session
}

// SetSession は今の通話を設定する（nil で通話なし）。
func (ln *Line) SetSession(s *Session) {
	ln.mu.Lock()
	ln.sess = s
	ln.mu.Unlock()
}

// Session は今の通話を返す（なければ nil）。
func (ln *Line) Session() *Session {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	return ln.sess
}

// Info はこの電話の状態を返す。
func (ln *Line) Info() proto.LineInfo {
	info := proto.LineInfo{Number: ln.Number, Waiting: ln.Waiting}
	if s := ln.Session(); s != nil {
		select {
		case <-s.Done():
		default:
			info.InCall = true
			info.Peer = s.Remote.User + "@" + s.Remote.Host
			info.Status = s.Status()
		}
	}
	return info
}

// Serve は制御ソケット l に来た接続を受け付ける。l が閉じられると戻る。
func (ln *Line) Serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go ln.serveConn(c)
	}
}

func (ln *Line) serveConn(c net.Conn) {
	fc := proto.NewConn(c)
	f, err := fc.Read()
	if err != nil || f.Type != proto.TOpen {
		c.Close()
		return
	}
	reply := func(r *proto.Frame) {
		defer c.Close()
		go func() { _, _ = io.Copy(io.Discard, c) }()
		r.Type = proto.TResult
		_ = fc.Write(r)
	}
	if f.Op == proto.OpStatus {
		b, _ := json.Marshal(ln.Info())
		reply(&proto.Frame{Msg: string(b)})
		return
	}
	s := ln.Session()
	if s == nil {
		reply(&proto.Frame{Err: ErrNoCall.Error()})
		return
	}
	s.ServeClient(c, f)
}
