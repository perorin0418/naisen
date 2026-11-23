package session

import (
	"io"
	"sync"
	"unicode/utf8"
)

// codePage はコマンドの出力の文字コード（Windows のコンソールのコードページ）。
type codePage interface {
	// isLead は b が2バイト文字の1バイト目かを返す。
	isLead(b byte) bool
	// decode は文字の区切りで終わるバイト列を UTF-8 にする。
	decode(p []byte) []byte
}

// utf8Writer はコマンドの出力を UTF-8 にそろえて w に書く。
//
// 書かれたまとまりが UTF-8 として正しければそのまま渡し（git などは UTF-8 で出す）、
// そうでなければ cp の文字コードとみなして変換する。まとまりの終わりで文字が途中で切れていれば、
// 残りを次の書き込みまで持ち越す。
type utf8Writer struct {
	w       io.Writer
	cp      codePage
	mu      sync.Mutex
	pending []byte
}

func (u *utf8Writer) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	buf := append(u.pending, p...)
	u.pending = nil
	if n := utf8Prefix(buf); n >= 0 {
		u.pending = append([]byte(nil), buf[n:]...)
		if n > 0 {
			if _, err := u.w.Write(buf[:n]); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}
	// コードページの文字列として、最後の文字の区切りまでを変換する
	m := 0
	for i := 0; i < len(buf); {
		if u.cp.isLead(buf[i]) {
			if i+1 >= len(buf) {
				break // 2バイト目がまだ来ていない
			}
			i += 2
		} else {
			i++
		}
		m = i
	}
	u.pending = append([]byte(nil), buf[m:]...)
	if m > 0 {
		if _, err := u.w.Write(u.cp.decode(buf[:m])); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush は持ち越したバイトを書き出す（コマンドが終わったときに呼ぶ）。
func (u *utf8Writer) Flush() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pending) > 0 {
		_, _ = u.w.Write(u.cp.decode(u.pending))
		u.pending = nil
	}
}

// utf8Prefix は buf が「正しい UTF-8 + 途中で切れた最後の1文字」なら、正しい部分の長さを返す。
// UTF-8 でなければ -1。
func utf8Prefix(buf []byte) int {
	n := len(buf)
	// 最後の文字の始まりを探す（最大4バイト前まで）
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-3; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				n = i
			}
			break
		}
	}
	if !utf8.Valid(buf[:n]) {
		return -1
	}
	return n
}
