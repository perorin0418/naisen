package xfer

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRoundTripDir(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "proj", "a.txt"), "A")
	write(t, filepath.Join(src, "proj", "sub", "b.txt"), "BB")
	var buf bytes.Buffer
	st, err := Pack(&buf, filepath.Join(src, "proj"))
	if err != nil || st.Files != 2 || st.Bytes != 3 {
		t.Fatalf("Pack = %+v, %v", st, err)
	}
	// 既存のディレクトリへ → その中に同じ名前で
	p, _, err := Unpack(bytes.NewReader(buf.Bytes()), dst, false)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dst, "proj") || read(t, filepath.Join(p, "sub", "b.txt")) != "BB" {
		t.Fatalf("置き場所が違う: %s", p)
	}
	// もう一度 → すでにある
	if _, _, err := Unpack(bytes.NewReader(buf.Bytes()), dst, false); !errors.Is(err, ErrExists) {
		t.Fatalf("上書きを拒否しない: %v", err)
	}
	if _, _, err := Unpack(bytes.NewReader(buf.Bytes()), dst, true); err != nil {
		t.Fatalf("-f で上書きできない: %v", err)
	}
	// 存在しない名前へ → その名前で
	p, _, err = Unpack(bytes.NewReader(buf.Bytes()), filepath.Join(dst, "renamed"), false)
	if err != nil || read(t, filepath.Join(p, "a.txt")) != "A" || filepath.Base(p) != "renamed" {
		t.Fatalf("名前を変えて置けない: %s %v", p, err)
	}
}

func TestRoundTripFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "x.txt"), "hello")
	var buf bytes.Buffer
	if _, err := Pack(&buf, filepath.Join(src, "x.txt")); err != nil {
		t.Fatal(err)
	}
	p, _, err := Unpack(bytes.NewReader(buf.Bytes()), filepath.Join(dst, "y.txt"), false)
	if err != nil || read(t, p) != "hello" {
		t.Fatalf("%s %v", p, err)
	}
	if _, _, err := Unpack(bytes.NewReader(buf.Bytes()), filepath.Join(dst, "nodir", "y.txt"), false); err == nil {
		t.Fatal("親ディレクトリがなくてもエラーにならない")
	}
}

func evilTar(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestRejectsEscape(t *testing.T) {
	dst := t.TempDir()
	cases := map[string][]byte{
		"dotdot": evilTar(t, &tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}),
		"two tops": evilTar(t,
			&tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
			&tar.Header{Name: "b", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}),
		"symlink out": evilTar(t,
			&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
			&tar.Header{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
			&tar.Header{Name: "d/link/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Unpack(bytes.NewReader(data), filepath.Join(dst, name), false); err == nil {
				t.Fatal("外に出る名前を拒否しない")
			}
		})
	}
	if _, err := os.Stat("/tmp/evil"); err == nil {
		t.Fatal("外に書かれた")
	}
}
