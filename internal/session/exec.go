package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/xfer"
)

var errCanceled = errors.New("取り消しました")

// handle は相手から来た依頼を1つ処理し、結果を返す。
func (s *Session) handle(f *proto.Frame, st *stream) {
	defer s.closeStream(s.execs, f.ID, st)
	var res *proto.Frame
	switch f.Op {
	case proto.OpRun:
		res = s.execRun(f, st)
	case proto.OpPut:
		res = s.execPut(f, st)
	case proto.OpGet:
		res = s.execGet(f, st)
	case proto.OpList:
		res = s.execList(f)
	default:
		res = &proto.Frame{Err: "知らない依頼です: " + f.Op}
	}
	res.Type, res.ID = proto.TResult, f.ID
	_ = s.peer.Write(res)
}

// execRun は相手から頼まれたコマンドを、この側のシェルで実行する。
func (s *Session) execRun(f *proto.Frame, st *stream) *proto.Frame {
	dir := s.resolve(f.Dir)
	s.logf("← 相手が実行: %s", f.Cmd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := shellCommand(ctx, f.Cmd)
	cmd.Dir = dir
	stdout, flushOut := outputWriter(&proto.DataWriter{C: s.peer, Type: proto.TStdout, ID: f.ID})
	stderr, flushErr := outputWriter(&proto.DataWriter{C: s.peer, Type: proto.TStderr, ID: f.ID})
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// コマンドが終わったあと、子プロセスが出力を握ったままでも待ち続けない
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &proto.Frame{Err: err.Error(), Code: 255}
	}
	if err := cmd.Start(); err != nil {
		return &proto.Frame{Err: "実行できません: " + err.Error(), Code: 127}
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	stdinCh := make(chan []byte, 64)
	go func() {
		for d := range stdinCh {
			if d == nil {
				stdin.Close()
				continue
			}
			_, _ = stdin.Write(d)
		}
	}()
	defer close(stdinCh)

	ch := st.ch
	canceled := false
	for {
		select {
		case err := <-waitCh:
			flushOut()
			flushErr()
			if canceled {
				return &proto.Frame{Code: 130, Msg: errCanceled.Error()}
			}
			return &proto.Frame{Code: exitCode(err)}
		case g, ok := <-ch:
			if !ok || g.Type == proto.TCancel {
				cancel()
				canceled = true
				ch = nil
				continue
			}
			switch g.Type {
			case proto.TStdin:
				stdinCh <- g.Data
			case proto.TStdinEOF:
				stdinCh <- nil
			}
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
		return 128 + 15 // シグナルで終わった
	}
	return 255
}

// execPut は相手から送られてきたファイルを、この側に置く。
func (s *Session) execPut(f *proto.Frame, st *stream) *proto.Frame {
	dest := s.resolve(f.Path)
	s.logf("← 相手がファイルを送ってきます → %s", dest)
	pr, pw := io.Pipe()
	type result struct {
		path string
		st   xfer.Stats
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		p, stats, err := xfer.Unpack(pr, dest, f.Force)
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.Close()
		}
		resCh <- result{p, stats, err}
	}()
	for {
		g, ok := <-st.ch
		if !ok || g.Type == proto.TCancel {
			pw.CloseWithError(errCanceled)
			<-resCh
			s.logf("← ファイルの受け取りを取り消しました（途中まで書いたファイルが残っていることがあります）")
			return &proto.Frame{Err: errCanceled.Error()}
		}
		switch g.Type {
		case proto.TTar:
			_, _ = pw.Write(g.Data) // 展開が失敗していれば捨てる
		case proto.TTarEOF:
			pw.Close()
			r := <-resCh
			if r.err != nil {
				s.logf("← ファイルを受け取れませんでした: %v", r.err)
				return &proto.Frame{Err: r.err.Error()}
			}
			s.logf("← 受け取りました: %s（%s）", r.path, r.st)
			return &proto.Frame{Path: r.path, Msg: r.st.String()}
		}
	}
}

// cancelWriter は取り消されたら書き込みを失敗させる。
type cancelWriter struct {
	w        io.Writer
	canceled atomic.Bool
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	if c.canceled.Load() {
		return 0, errCanceled
	}
	return c.w.Write(p)
}

// execGet は相手に頼まれたファイルを、tar にまとめて渡す。
func (s *Session) execGet(f *proto.Frame, st *stream) *proto.Frame {
	src := s.resolve(f.Path)
	s.logf("← 相手にファイルを渡します: %s", src)
	cw := &cancelWriter{w: &proto.DataWriter{C: s.peer, Type: proto.TTar, ID: f.ID}}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case g, ok := <-st.ch:
				if !ok || g.Type == proto.TCancel {
					cw.canceled.Store(true)
					return
				}
			case <-stop:
				return
			}
		}
	}()
	bw := bufio.NewWriterSize(cw, proto.ChunkSize)
	stats, err := xfer.Pack(bw, src)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		s.logf("← ファイルを渡せませんでした: %v", err)
		return &proto.Frame{Err: err.Error()}
	}
	if err := s.peer.Write(&proto.Frame{Type: proto.TTarEOF, ID: f.ID}); err != nil {
		return &proto.Frame{Err: err.Error()}
	}
	s.logf("← 渡しました: %s（%s）", src, stats)
	return &proto.Frame{Path: src, Msg: stats.String()}
}

// execList はこの側のディレクトリの一覧を返す（相手の gui の表示用。表示が多くなるのでログは出さない）。
func (s *Session) execList(f *proto.Frame) *proto.Frame {
	dir := s.resolve(f.Path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return &proto.Frame{Err: err.Error()}
	}
	l := proto.Listing{Path: dir, Parent: filepath.Dir(dir), Sep: string(filepath.Separator), Entries: []proto.Entry{}}
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		ent := proto.Entry{Name: e.Name(), Dir: info.IsDir(), Size: info.Size(), MTime: info.ModTime().Unix()}
		if info.Mode()&os.ModeSymlink != 0 {
			ent.Link = true
			if ti, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				ent.Dir = ti.IsDir()
			}
		}
		if ent.Dir {
			ent.Size = 0
		}
		l.Entries = append(l.Entries, ent)
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	b, err := json.Marshal(l)
	if err != nil {
		return &proto.Frame{Err: err.Error()}
	}
	return &proto.Frame{Path: dir, Msg: string(b)}
}
