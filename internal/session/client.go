package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/xfer"
)

// ここの関数は、依頼する側（単発コマンドと対話プロンプト）が使う。
// c は通話中のプロセス（制御ソケット、または net.Pipe）への接続。
// cancel に値が届くと、相手に取り消しを送る（Ctrl-C）。

func watchCancel(fc *proto.Conn, cancel <-chan struct{}, onCancel func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-cancel:
				if onCancel != nil {
					onCancel()
				}
				_ = fc.Write(&proto.Frame{Type: proto.TCancel})
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func lost(err error) error {
	return errors.New("通話中のプロセスとの接続が切れました: " + err.Error())
}

// Run は相手側でコマンドを実行し、終了コードを返す。stdin が nil なら標準入力は空にする。
func Run(c net.Conn, cmdline, dir string, stdin io.Reader, stdout, stderr io.Writer, cancel <-chan struct{}) (int, error) {
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.TOpen, Op: proto.OpRun, Cmd: cmdline, Dir: dir}); err != nil {
		return 255, lost(err)
	}
	stop := watchCancel(fc, cancel, nil)
	defer stop()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(&proto.DataWriter{C: fc, Type: proto.TStdin}, stdin)
			_ = fc.Write(&proto.Frame{Type: proto.TStdinEOF})
		}()
	} else {
		_ = fc.Write(&proto.Frame{Type: proto.TStdinEOF})
	}
	for {
		f, err := fc.Read()
		if err != nil {
			return 255, lost(err)
		}
		switch f.Type {
		case proto.TStdout:
			_, _ = stdout.Write(f.Data)
		case proto.TStderr:
			_, _ = stderr.Write(f.Data)
		case proto.TResult:
			if f.Err != "" {
				return f.Code, errors.New(f.Err)
			}
			return f.Code, nil
		}
	}
}

// Result は send・get の結果。
type Result struct {
	Path  string // 置いた場所（send なら相手側、get ならこの側）
	Stats string
}

// Send はこの側の local を相手側の remote（空なら相手の作業ディレクトリ）に送る。
func Send(c net.Conn, local, remote string, force bool, cancel <-chan struct{}) (Result, error) {
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.TOpen, Op: proto.OpPut, Path: remote, Force: force}); err != nil {
		return Result{}, lost(err)
	}
	cw := &cancelWriter{w: &proto.DataWriter{C: fc, Type: proto.TTar}}
	stop := watchCancel(fc, cancel, func() { cw.canceled.Store(true) })
	defer stop()
	packErr := make(chan error, 1)
	go func() {
		bw := bufio.NewWriterSize(cw, proto.ChunkSize)
		_, err := xfer.Pack(bw, local)
		if err == nil {
			err = bw.Flush()
		}
		if err != nil {
			_ = fc.Write(&proto.Frame{Type: proto.TCancel})
			packErr <- err
			return
		}
		_ = fc.Write(&proto.Frame{Type: proto.TTarEOF})
		packErr <- nil
	}()
	for {
		f, err := fc.Read()
		if err != nil {
			return Result{}, lost(err)
		}
		if f.Type != proto.TResult {
			continue
		}
		// 相手側が先に失敗したとき（すでにあるなど）は、まとめるのをやめる
		cw.canceled.Store(true)
		perr := <-packErr
		if perr != nil && !errors.Is(perr, errCanceled) && (f.Err == "" || f.Err == errCanceled.Error()) {
			return Result{}, perr // この側で読めなかった
		}
		if f.Err != "" {
			return Result{}, errors.New(f.Err)
		}
		return Result{Path: f.Path, Stats: f.Msg}, nil
	}
}

// Get は相手側の remote を、この側の local（空ならカレントディレクトリ）に受け取る。
func Get(c net.Conn, remote, local string, force bool, cancel <-chan struct{}) (Result, error) {
	if local == "" {
		local = "."
	}
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.TOpen, Op: proto.OpGet, Path: remote}); err != nil {
		return Result{}, lost(err)
	}
	stop := watchCancel(fc, cancel, nil)
	defer stop()
	pr, pw := io.Pipe()
	type result struct {
		path  string
		stats xfer.Stats
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		p, st, err := xfer.Unpack(pr, local, force)
		if err != nil {
			pr.CloseWithError(err)
			// 受け取れないので、相手に送るのをやめてもらう
			_ = fc.Write(&proto.Frame{Type: proto.TCancel})
		} else {
			pr.Close()
		}
		resCh <- result{p, st, err}
	}()
	for {
		f, err := fc.Read()
		if err != nil {
			pw.CloseWithError(err)
			<-resCh
			return Result{}, lost(err)
		}
		switch f.Type {
		case proto.TTar:
			_, _ = pw.Write(f.Data)
		case proto.TTarEOF:
			pw.Close()
		case proto.TResult:
			if f.Err != "" {
				pw.CloseWithError(errors.New(f.Err))
			} else {
				pw.Close()
			}
			// 失敗の理由は、この側の展開の失敗か、相手側の失敗（パイプ経由で展開に伝わる）
			r := <-resCh
			if r.err != nil {
				return Result{}, r.err
			}
			return Result{Path: r.path, Stats: r.stats.String()}, nil
		}
	}
}

// List は相手側のディレクトリ dir（空なら相手の作業ディレクトリ）の一覧を返す。
func List(c net.Conn, dir string) (*proto.Listing, error) {
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.TOpen, Op: proto.OpList, Path: dir}); err != nil {
		return nil, lost(err)
	}
	for {
		f, err := fc.Read()
		if err != nil {
			return nil, lost(err)
		}
		if f.Type != proto.TResult {
			continue
		}
		if f.Err != "" {
			return nil, errors.New(f.Err)
		}
		var l proto.Listing
		if err := json.Unmarshal([]byte(f.Msg), &l); err != nil {
			return nil, err
		}
		return &l, nil
	}
}

// Simple は status・hangup のように、結果だけを受け取る依頼を出す。
func Simple(c net.Conn, op string) (string, error) {
	fc := proto.NewConn(c)
	if err := fc.Write(&proto.Frame{Type: proto.TOpen, Op: op}); err != nil {
		return "", lost(err)
	}
	f, err := fc.Read()
	if err != nil {
		return "", lost(err)
	}
	if f.Err != "" {
		return "", errors.New(f.Err)
	}
	return f.Msg, nil
}
