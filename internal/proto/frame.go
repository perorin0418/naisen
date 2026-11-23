// Package proto は naisen の通信で使うフレーム（メッセージの単位）を定める。
//
// 1つのフレームは「ヘッダーの長さ(4バイト) + データの長さ(4バイト) + ヘッダー(JSON) + データ」。
// ホストと koya の間（TCP）と、通話中のプロセスと単発コマンドの間（制御ソケット）の両方で同じ形を使う。
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Version は通信の版。発信側と受信側で違えば通話しない。
const Version = 2

// 番号とポート。番号 n の電話は TCP の BasePort+n で待ち受ける。
const (
	MinNumber = 1
	MaxNumber = 49
	BasePort  = 7650
)

// フレームの種類。
const (
	THello   = "hello"   // 発信側 → 受信側: 発信の情報（Probe なら問い合わせだけ）
	TInfo    = "info"    // 受信側 → 問い合わせ側: 自分の情報（電話帳の表示用）
	TWelcome = "welcome" // 受信側 → 発信側: 受諾
	TReject  = "reject"  // 受信側 → 発信側: 拒否・話し中など（Msg に理由）
	TBye     = "bye"     // どちらからでも: 電話を切る

	TOpen     = "open"      // 依頼の開始（Op で種類を示す）
	TStdin    = "stdin"     // run: 標準入力のデータ
	TStdinEOF = "stdin-eof" // run: 標準入力の終わり
	TStdout   = "stdout"    // run: 標準出力のデータ
	TStderr   = "stderr"    // run: 標準エラー出力のデータ
	TTar      = "tar"       // send/get: tar 形式のデータ
	TTarEOF   = "tar-eof"   // send/get: tar の終わり
	TCancel   = "cancel"    // 依頼の取り消し（Ctrl-C）
	TResult   = "result"    // 依頼の結果（これで依頼が終わる）
)

// 依頼の種類（Op）。
const (
	OpRun    = "run"    // 相手側でコマンドを実行する
	OpPut    = "put"    // 相手側にファイルを置く（send）
	OpGet    = "get"    // 相手側のファイルを受け取る（get）
	OpList   = "ls"     // 相手側のディレクトリの一覧（gui で使う。結果は Msg に JSON）
	OpStatus = "status" // 制御ソケットだけ: 通話の状態
	OpHangup = "hangup" // 制御ソケットだけ: 電話を切る
)

// Hello は発信側・受信側の情報。
type Hello struct {
	Version int    `json:"version"`
	Number  int    `json:"number,omitempty"` // 電話の番号
	Probe   bool   `json:"probe,omitempty"`  // 問い合わせだけ（電話帳）。着信にしない
	Busy    bool   `json:"busy,omitempty"`   // 問い合わせの答え: 通話中（または呼び出し中）
	Auto    bool   `json:"auto,omitempty"`   // 問い合わせの答え: 自動で取る内線
	Addr    string `json:"addr,omitempty"`   // 電話帳の表示用: 見つけたアドレス（通信では使わない）
	Host    string `json:"host"`
	User    string `json:"user"`
	Cwd     string `json:"cwd"`
	Profile string `json:"profile,omitempty"` // koya のプロファイル（KOYA_PROFILE）
	Code    string `json:"code,omitempty"`    // 確認コード（発信側が作り、両方の画面に出す）
	OS      string `json:"os"`
}

// LineInfo はこのマシンの1つの電話の状態（制御ソケットの status の結果。Msg に JSON で入れる）。
type LineInfo struct {
	Number  int    `json:"number"`
	Waiting bool   `json:"waiting"`          // 着信を待つ電話（naisen）か。false なら発信した電話（naisen call）
	InCall  bool   `json:"inCall"`           // 通話中か
	Peer    string `json:"peer,omitempty"`   // 通話の相手（user@host）
	Status  string `json:"status,omitempty"` // 通話の状態の説明
}

// Listing はディレクトリの一覧（OpList の結果）。
type Listing struct {
	Path    string  `json:"path"`   // 一覧したディレクトリの絶対パス
	Parent  string  `json:"parent"` // 親ディレクトリ（最上位なら Path と同じ）
	Sep     string  `json:"sep"`    // パスの区切り文字（/ か \）
	Entries []Entry `json:"entries"`
}

// Entry は一覧の1項目。
type Entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Link  bool   `json:"link,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // UNIX 時刻（秒）
}

// Frame は1つのメッセージ。
type Frame struct {
	Type  string `json:"t"`
	ID    uint32 `json:"id,omitempty"`
	Op    string `json:"op,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Path  string `json:"path,omitempty"`
	Force bool   `json:"force,omitempty"`
	Code  int    `json:"code,omitempty"`
	Err   string `json:"err,omitempty"`
	Msg   string `json:"msg,omitempty"`
	Hello *Hello `json:"hello,omitempty"`

	Data []byte `json:"-"`
}

const (
	maxHeader = 1 << 20
	maxData   = 1 << 20
	// ChunkSize はデータを分けて送るときの1フレームの大きさ。
	ChunkSize = 32 << 10
)

// Conn はフレームを読み書きする接続。書き込みは複数のゴルーチンから同時に呼べる。
type Conn struct {
	c  net.Conn
	mu sync.Mutex
}

// NewConn は c をフレームの接続として包む。
func NewConn(c net.Conn) *Conn { return &Conn{c: c} }

// Raw は元の接続を返す。
func (c *Conn) Raw() net.Conn { return c.c }

// Close は接続を閉じる。
func (c *Conn) Close() error { return c.c.Close() }

// Write はフレームを1つ書く。
func (c *Conn) Write(f *Frame) error {
	h, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(f.Data) > maxData {
		return fmt.Errorf("データが大きすぎます（%d バイト）", len(f.Data))
	}
	buf := make([]byte, 8, 8+len(h)+len(f.Data))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(h)))
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(f.Data)))
	buf = append(buf, h...)
	buf = append(buf, f.Data...)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.c.Write(buf)
	return err
}

// Read はフレームを1つ読む。同時に呼べるのは1つのゴルーチンだけ。
func (c *Conn) Read() (*Frame, error) {
	var lens [8]byte
	if _, err := io.ReadFull(c.c, lens[:]); err != nil {
		return nil, err
	}
	hl := binary.BigEndian.Uint32(lens[0:4])
	dl := binary.BigEndian.Uint32(lens[4:8])
	if hl > maxHeader || dl > maxData {
		return nil, errors.New("フレームが大きすぎます（naisen 以外とつながっているかもしれません）")
	}
	body := make([]byte, int(hl)+int(dl))
	if _, err := io.ReadFull(c.c, body); err != nil {
		return nil, err
	}
	f := &Frame{}
	if err := json.Unmarshal(body[:hl], f); err != nil {
		return nil, fmt.Errorf("フレームを読めません: %w", err)
	}
	if dl > 0 {
		f.Data = body[hl:]
	}
	return f, nil
}

// DataWriter は書かれたデータを、種類 typ のフレームに分けて送る io.Writer。
type DataWriter struct {
	C    *Conn
	Type string
	ID   uint32
}

func (w *DataWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), ChunkSize)
		d := make([]byte, k)
		copy(d, p[:k])
		if err := w.C.Write(&Frame{Type: w.Type, ID: w.ID, Data: d}); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}
