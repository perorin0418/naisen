package session

import (
	"bytes"
	"testing"
)

// fakeCP は Shift_JIS に似せたテスト用のコードページ。0x81〜0x9f が2バイト文字の1バイト目で、
// 2バイト文字は「<XXYY>」に変換する。
type fakeCP struct{}

func (fakeCP) isLead(b byte) bool { return b >= 0x81 && b <= 0x9f }
func (fakeCP) decode(p []byte) []byte {
	var out []byte
	for i := 0; i < len(p); i++ {
		if (fakeCP{}).isLead(p[i]) && i+1 < len(p) {
			out = append(out, []byte("<"+string("0123456789abcdef"[p[i]>>4])+string("0123456789abcdef"[p[i]&15])+string("0123456789abcdef"[p[i+1]>>4])+string("0123456789abcdef"[p[i+1]&15])+">")...)
			i++
		} else {
			out = append(out, p[i])
		}
	}
	return out
}

func feed(chunks ...[]byte) string {
	var b bytes.Buffer
	w := &utf8Writer{w: &b, cp: fakeCP{}}
	for _, c := range chunks {
		w.Write(c)
	}
	w.Flush()
	return b.String()
}

func TestUTF8WriterPassesUTF8(t *testing.T) {
	s := []byte("日本語 ok\n")
	// 文字の途中で分けても、そのまま届く
	if got := feed(s[:2], s[2:4], s[4:]); got != string(s) {
		t.Fatalf("got %q", got)
	}
}

func TestUTF8WriterConvertsCodePage(t *testing.T) {
	sjis := []byte{0x93, 0xfa, 0x96, 0x7b, '\n'} // 「日本」
	if got := feed(sjis); got != "<93fa><967b>\n" {
		t.Fatalf("got %q", got)
	}
	// 2バイト文字の途中で分かれても、つなげて変換する
	if got := feed(sjis[:1], sjis[1:3], sjis[3:]); got != "<93fa><967b>\n" {
		t.Fatalf("分割: got %q", got)
	}
}
