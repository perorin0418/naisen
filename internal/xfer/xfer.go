// Package xfer はファイルやディレクトリを tar 形式にまとめて送り、受け取った側で展開する。
package xfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Stats は送受信したファイルの数と大きさ。
type Stats struct {
	Files int
	Bytes int64
}

func (s Stats) String() string {
	return fmt.Sprintf("%d ファイル、%s", s.Files, HumanSize(s.Bytes))
}

// HumanSize はバイト数を読みやすい形にする。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Pack は src（ファイルかディレクトリ）を tar 形式で w に書く。
// tar の中の名前は src の最後の要素から始まる（/a/b/dir → dir/...）。
func Pack(w io.Writer, src string) (Stats, error) {
	var st Stats
	abs, err := filepath.Abs(src)
	if err != nil {
		return st, err
	}
	base := filepath.Base(abs)
	if base == string(filepath.Separator) || base == "." || strings.HasSuffix(base, ":") || strings.HasSuffix(base, `:\`) {
		return st, fmt.Errorf("%s は送れません（ディレクトリの中身を送るときはサブディレクトリを指定してください）", src)
	}
	if _, err := os.Lstat(abs); err != nil {
		return st, err
	}
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		name := base
		if rel != "." {
			name = base + "/" + filepath.ToSlash(rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			return nil // デバイスやソケットなどは送らない
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		// 所有者の情報は相手側では意味がないので入れない
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
			st.Files++
			st.Bytes += n
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	return st, tw.Close()
}

// ErrExists は置き先にすでにあり、上書きが許されていないときのエラー。
var ErrExists = errors.New("すでにあります（上書きするには -f を付けてください）")

// Unpack は r の tar を dest に展開し、置いた場所の絶対パスを返す。
//
//   - dest が既存のディレクトリなら、その中に元の名前で置く
//   - そうでなければ dest という名前で置く（親ディレクトリは必要）
//   - 置き先にすでにあれば、force のときだけ上書きする（ディレクトリは中身を重ねる）
//
// 展開は dest の親ディレクトリの外に出ないようにする（.. やシンボリックリンクで外を指す名前は拒否）。
func Unpack(r io.Reader, dest string, force bool) (string, Stats, error) {
	var st Stats
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", st, err
	}
	var parent, top string
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		parent = abs
	} else if err == nil || errors.Is(err, fs.ErrNotExist) {
		parent, top = filepath.Dir(abs), filepath.Base(abs)
		if pfi, err := os.Stat(parent); err != nil || !pfi.IsDir() {
			return "", st, fmt.Errorf("置き先のディレクトリ %s がありません", parent)
		}
	} else {
		return "", st, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", st, err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	srcTop := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", st, err
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", st, fmt.Errorf("不正な名前が含まれています: %q", hdr.Name)
		}
		first, rest, _ := strings.Cut(name, "/")
		if srcTop == "" {
			srcTop = first
			if top == "" {
				top = first
			}
			if _, err := root.Lstat(top); err == nil && !force {
				return "", st, fmt.Errorf("%s: %w", filepath.Join(parent, top), ErrExists)
			}
		} else if first != srcTop {
			return "", st, fmt.Errorf("不正な名前が含まれています: %q", hdr.Name)
		}
		rel := top
		if rest != "" {
			rel = top + "/" + rest
		}
		rel = filepath.FromSlash(rel)
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, mode|0o700); err != nil {
				return "", st, err
			}
		case tar.TypeReg:
			if err := mkParent(root, rel); err != nil {
				return "", st, err
			}
			if fi, err := root.Lstat(rel); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				// 既存のシンボリックリンクの先に書かない
				if err := root.Remove(rel); err != nil {
					return "", st, err
				}
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o600)
			if err != nil {
				return "", st, err
			}
			n, err := io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", st, err
			}
			_ = root.Chmod(rel, mode|0o600)
			st.Files++
			st.Bytes += n
		case tar.TypeSymlink:
			if err := mkParent(root, rel); err != nil {
				return "", st, err
			}
			if _, err := root.Lstat(rel); err == nil {
				if err := root.Remove(rel); err != nil {
					return "", st, err
				}
			}
			if err := root.Symlink(hdr.Linkname, rel); err != nil {
				return "", st, err
			}
		default:
			// それ以外の種類は無視する
		}
	}
	if srcTop == "" {
		return "", st, errors.New("受け取ったデータが空です")
	}
	return filepath.Join(parent, top), st, nil
}

func mkParent(root *os.Root, rel string) error {
	d := filepath.Dir(rel)
	if d == "." {
		return nil
	}
	return root.MkdirAll(d, 0o755)
}
