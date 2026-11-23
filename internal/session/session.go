// Package session は通話（ホストと koya の間の接続）を管理する。
//
// 通話が始まると、どちらの側も同じ役割を持つ:
//   - 依頼する側: 自分の単発コマンド（naisen run など）や対話プロンプトからの依頼を相手に送る
//   - 受ける側: 相手からの依頼（コマンドの実行、ファイルの受け取り・受け渡し）を処理する
//
// 1本の接続の上で複数の依頼を同時に流せるように、依頼ごとに番号（ID）を付ける。
// 発信側は奇数、受信側は偶数の番号を使うので、番号を見ればどちらの依頼か分かる。
package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
)

// stream は1つの依頼に届いたフレームを渡す口。
type stream struct {
	ch   chan *proto.Frame
	done chan struct{} // 受け取り側が使い終わったら閉じる
	once sync.Once
}

func newStream() *stream {
	return &stream{ch: make(chan *proto.Frame, 64), done: make(chan struct{})}
}

func (st *stream) finish() { st.once.Do(func() { close(st.done) }) }

// Session は1回の通話。
type Session struct {
	Local  proto.Hello
	Remote proto.Hello
	Log    io.Writer // 相手からの依頼などを表示する先

	peer    *proto.Conn
	caller  bool
	cwd     string
	started time.Time

	mu      sync.Mutex
	nextID  uint32
	clients map[uint32]*stream // 自分が出した依頼
	execs   map[uint32]*stream // 相手から来た依頼
	closed  bool
	reason  string
	done    chan struct{}
}

// New は通話を作る。caller は発信側かどうか。Run を呼ぶと動き出す。
func New(peer *proto.Conn, local, remote proto.Hello, caller bool, log io.Writer) *Session {
	cwd, _ := os.Getwd()
	s := &Session{
		Local: local, Remote: remote, Log: log,
		peer: peer, caller: caller, cwd: cwd, started: time.Now(),
		clients: map[uint32]*stream{}, execs: map[uint32]*stream{},
		done: make(chan struct{}),
	}
	if caller {
		s.nextID = 1
	} else {
		s.nextID = 2
	}
	return s
}

// Done は通話が終わると閉じる。
func (s *Session) Done() <-chan struct{} { return s.done }

// Reason は通話が終わった理由。
func (s *Session) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Session) setReason(r string) {
	s.mu.Lock()
	if s.reason == "" {
		s.reason = r
	}
	s.mu.Unlock()
}

func (s *Session) mine(id uint32) bool { return (id%2 == 1) == s.caller }

func (s *Session) logf(format string, a ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "\r"+format+"\n", a...)
	}
}

// Hangup は電話を切る。
func (s *Session) Hangup() {
	s.setReason("電話を切りました")
	_ = s.peer.Write(&proto.Frame{Type: proto.TBye})
	_ = s.peer.Close()
}

// Run は相手からのフレームを読み続ける。通話が終わると戻る。
func (s *Session) Run() {
	defer s.shutdown()
	for {
		f, err := s.peer.Read()
		if err != nil {
			s.setReason("通話が切れました")
			return
		}
		switch f.Type {
		case proto.TBye:
			s.setReason("相手が電話を切りました")
			return
		case proto.TOpen:
			if s.mine(f.ID) {
				continue
			}
			st := newStream()
			s.mu.Lock()
			s.execs[f.ID] = st
			s.mu.Unlock()
			go s.handle(f, st)
		default:
			s.mu.Lock()
			var st *stream
			if s.mine(f.ID) {
				st = s.clients[f.ID]
			} else {
				st = s.execs[f.ID]
			}
			s.mu.Unlock()
			if st == nil {
				continue // 終わった依頼の残り
			}
			select {
			case st.ch <- f:
			case <-st.done:
			}
		}
	}
}

func (s *Session) shutdown() {
	_ = s.peer.Close()
	s.mu.Lock()
	s.closed = true
	for _, st := range s.clients {
		close(st.ch)
	}
	for _, st := range s.execs {
		close(st.ch)
	}
	s.clients, s.execs = map[uint32]*stream{}, map[uint32]*stream{}
	s.mu.Unlock()
	close(s.done)
}

func (s *Session) openClient() (uint32, *stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, nil, errors.New("通話は終わっています")
	}
	id := s.nextID
	s.nextID += 2
	st := newStream()
	s.clients[id] = st
	return id, st, nil
}

func (s *Session) closeStream(m map[uint32]*stream, id uint32, st *stream) {
	st.finish()
	s.mu.Lock()
	if m[id] == st {
		delete(m, id)
	}
	s.mu.Unlock()
}

// Status は通話の状態を文で返す。
func (s *Session) Status() string {
	r := s.Remote
	var b strings.Builder
	fmt.Fprintf(&b, "番号 %d: 通話中（%s から、%s 経過）\n", s.Local.Number, s.started.Format("15:04:05"), time.Since(s.started).Round(time.Second))
	fmt.Fprintf(&b, "  相手: %s@%s（番号 %d、%s）", r.User, r.Host, r.Number, r.OS)
	if r.Profile != "" {
		fmt.Fprintf(&b, " koya プロファイル %s", r.Profile)
	}
	fmt.Fprintf(&b, "\n  相手の作業ディレクトリ: %s\n", r.Cwd)
	fmt.Fprintf(&b, "  自分の作業ディレクトリ: %s", s.cwd)
	return b.String()
}

// resolve は相手から来たパスを、この側の作業ディレクトリを基準にした絶対パスにする。
func (s *Session) resolve(p string) string {
	if p == "" {
		return s.cwd
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.cwd, p)
	}
	return filepath.Clean(p)
}

// ServeClient は単発コマンド（または対話プロンプト）からの依頼を1つ受け、相手との間を中継する。
// f は読み終えた最初のフレーム（nil なら c から読む）。
func (s *Session) ServeClient(c net.Conn, f *proto.Frame) {
	fc := proto.NewConn(c)
	defer fc.Close()
	if f == nil {
		var err error
		if f, err = fc.Read(); err != nil {
			return
		}
	}
	if f.Type != proto.TOpen {
		return
	}
	// すぐに結果を返すとき。依頼した側が続けて書いてきても詰まらないように、読み捨てながら返す
	// （net.Pipe はバッファーを持たないので、互いに書き込むと止まる）。
	reply := func(r *proto.Frame) {
		go func() { _, _ = io.Copy(io.Discard, c) }()
		r.Type = proto.TResult
		_ = fc.Write(r)
	}
	switch f.Op {
	case proto.OpStatus:
		reply(&proto.Frame{Msg: s.Status()})
		return
	case proto.OpHangup:
		s.Hangup()
		reply(&proto.Frame{})
		return
	case proto.OpRun, proto.OpPut, proto.OpGet, proto.OpList:
	default:
		reply(&proto.Frame{Err: "知らない依頼です: " + f.Op})
		return
	}
	id, st, err := s.openClient()
	if err != nil {
		reply(&proto.Frame{Err: err.Error()})
		return
	}
	defer s.closeStream(s.clients, id, st)
	f.ID = id
	if err := s.peer.Write(f); err != nil {
		reply(&proto.Frame{Err: "相手に送れません: " + err.Error()})
		return
	}

	finished := make(chan struct{})
	// 依頼した側 → 相手
	go func() {
		for {
			g, err := fc.Read()
			if err != nil {
				select {
				case <-finished:
				default:
					// 依頼した側が途中でいなくなったので取り消す
					_ = s.peer.Write(&proto.Frame{Type: proto.TCancel, ID: id})
				}
				return
			}
			switch g.Type {
			case proto.TStdin, proto.TStdinEOF, proto.TTar, proto.TTarEOF, proto.TCancel:
				_ = s.peer.Write(&proto.Frame{Type: g.Type, ID: id, Data: g.Data})
			}
		}
	}()
	// 相手 → 依頼した側
	for {
		g, ok := <-st.ch
		if !ok {
			_ = fc.Write(&proto.Frame{Type: proto.TResult, Err: s.Reason()})
			break
		}
		g.ID = 0
		if err := fc.Write(g); err != nil {
			_ = s.peer.Write(&proto.Frame{Type: proto.TCancel, ID: id})
			break
		}
		if g.Type == proto.TResult {
			break
		}
	}
	close(finished)
}
