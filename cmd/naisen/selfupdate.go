package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// self-update: GitHub Releases から、この OS・CPU 用の最新版を取ってきて、実行中のバイナリを置き換える。
//
// 配布ファイルは naisen_<版>_<OS>_<CPU>.tar.gz（Windows は .zip）で名前に版が入るため、
// releases/latest/download/checksums.txt から最新版とファイル名を決める（GitHub API は使わない）。

const defaultReleaseURL = "https://github.com/perorin0418/naisen/releases"

const selfUpdateUsage = `使い方: naisen self-update [--check] [--version vX.Y.Z] [-f]

GitHub Releases から、この OS・CPU 用の naisen を取ってきて、実行中の naisen（このファイル）を置き換えます。
取ってきたファイルは checksums.txt の SHA-256 で確かめてから置き換えます。

オプション:
  --check          新しい版があるかを表示するだけで、置き換えない（新しい版があれば終了コード 1）
  --version vX.Y.Z  最新版ではなく、その版にする（古い版に戻すときにも使う）
  -f               今の版と同じでも置き換える

置き換えても、起動中の naisen（待ち受け中・通話中の電話）は古い版のまま動きます。起き直すと新しい版になります。
ホストと koya の中で同じ版を使うため、両方で実行してください。
置き場所に書き込めないとき（/usr/local/bin など）は、書き込める権限で実行してください。

環境変数:
  NAISEN_RELEASE_URL  リリースの取得元（既定 ` + defaultReleaseURL + `）。
                     <URL>/latest/download/checksums.txt と <URL>/download/<版>/<ファイル> を取ります
  HTTPS_PROXY など    プロキシ（Go の標準の扱い）
`

func cmdSelfUpdate(args []string) int {
	var check, force bool
	var want string
	fs, ok := parseFlags("self-update", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&check, "check", false, "")
		fs.StringVar(&want, "version", "", "")
		fs.BoolVar(&force, "f", false, "")
	})
	if !ok {
		return 2
	}
	if fs.NArg() > 0 {
		errorf("余分な引数があります: %s（naisen help self-update）", strings.Join(fs.Args(), " "))
		return 2
	}
	if want != "" && !strings.HasPrefix(want, "v") {
		want = "v" + want
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-interrupts()
		cancel()
	}()

	base := strings.TrimRight(os.Getenv("NAISEN_RELEASE_URL"), "/")
	if base == "" {
		base = defaultReleaseURL
	}
	u := &updater{ctx: ctx, base: base, client: &http.Client{Timeout: 10 * time.Minute}}

	rel, err := u.resolve(want)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	cur := versionString()
	cmp, known := compareVersions(cur, rel.version)
	switch {
	case check:
		if known && cmp >= 0 {
			fmt.Printf("naisen %s は最新です（リリースの最新は %s）\n", cur, rel.version)
			return 0
		}
		fmt.Printf("naisen %s → %s に更新できます（naisen self-update）\n", cur, rel.version)
		return 1
	case !force && known && cmp == 0:
		fmt.Printf("naisen %s は最新です（同じ版で置き換えるときは -f）\n", cur)
		return 0
	case !force && known && cmp > 0 && want == "":
		fmt.Printf("naisen %s はリリースの最新（%s）より新しいので、置き換えません（戻すときは --version %s）\n", cur, rel.version, rel.version)
		return 0
	}

	exe, err := executablePath()
	if err != nil {
		errorf("実行中の naisen の場所が分かりません: %v", err)
		return 1
	}
	logf("%s を取っています…", rel.asset)
	bin, err := u.fetchBinary(rel)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	if err := replaceExecutable(exe, bin); err != nil {
		errorf("%s を置き換えられません: %v", exe, err)
		if errors.Is(err, os.ErrPermission) {
			errorf("書き込める権限で実行してください（sudo naisen self-update など）")
		}
		return 1
	}
	fmt.Printf("naisen を %s → %s に更新しました（%s）\n", cur, rel.version, exe)
	if lines := lineInfos(); len(lines) > 0 {
		fmt.Printf("起動中の電話が %d つあります。古い版のまま動いているので、切ってから起動し直してください。\n", len(lines))
	}
	return 0
}

type updater struct {
	ctx    context.Context
	base   string
	client *http.Client
}

type release struct {
	version string // v0.2.0
	asset   string // naisen_0.2.0_linux_amd64.tar.gz
	sum     []byte // SHA-256
}

// resolve は checksums.txt から、この OS・CPU 用の配布ファイルと版を決める。want が空なら最新版。
func (u *updater) resolve(want string) (*release, error) {
	url := u.base + "/latest/download/checksums.txt"
	if want != "" {
		url = u.base + "/download/" + want + "/checksums.txt"
	}
	body, err := u.get(url, 1<<20)
	if err != nil {
		if want != "" {
			return nil, fmt.Errorf("版 %s のリリースを取れません: %w", want, err)
		}
		return nil, fmt.Errorf("最新版のリリースを取れません: %w", err)
	}
	return pickAsset(body, runtime.GOOS, runtime.GOARCH)
}

// pickAsset は checksums.txt の中身から、goos・goarch 用の配布ファイルの行を選ぶ。
func pickAsset(checksums []byte, goos, goarch string) (*release, error) {
	ext := `\.tar\.gz`
	if goos == "windows" {
		ext = `\.zip`
	}
	re := regexp.MustCompile(`^naisen_([^_]+)_` + regexp.QuoteMeta(goos) + `_` + regexp.QuoteMeta(goarch) + ext + `$`)
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		sum, err := hex.DecodeString(f[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("checksums.txt の %s の行が読めません", name)
		}
		return &release{version: "v" + m[1], asset: name, sum: sum}, nil
	}
	return nil, fmt.Errorf("リリースに %s/%s 用のファイルがありません", goos, goarch)
}

// fetchBinary は配布ファイルを取り、SHA-256 を確かめて、中の naisen の中身を返す。
func (u *updater) fetchBinary(rel *release) ([]byte, error) {
	data, err := u.get(u.base+"/download/"+rel.version+"/"+rel.asset, 200<<20)
	if err != nil {
		return nil, fmt.Errorf("%s を取れません: %w", rel.asset, err)
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], rel.sum) {
		return nil, fmt.Errorf("%s の SHA-256 が checksums.txt と合いません（%x）", rel.asset, got)
	}
	bin, err := extractBinary(rel.asset, data)
	if err != nil {
		return nil, fmt.Errorf("%s から naisen を取り出せません: %w", rel.asset, err)
	}
	return bin, nil
}

func (u *updater) get(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(u.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "naisen/"+versionString())
	resp, err := u.client.Do(req)
	if err != nil {
		if u.ctx.Err() != nil {
			return nil, errors.New("取り消しました")
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: 大きすぎます", url)
	}
	return data, nil
}

// extractBinary は配布ファイル（tar.gz か zip）から naisen（naisen.exe）を取り出す。
// 配布ファイルの中身は naisen_<版>_<OS>_<CPU>/naisen と README.md。
func extractBinary(asset string, data []byte) ([]byte, error) {
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == "naisen.exe" && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, errors.New("naisen.exe がありません")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("naisen がありません")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "naisen" {
			return io.ReadAll(tr)
		}
	}
}

// executablePath は実行中の naisen のファイルの場所（シンボリックリンクはたどった先）を返す。
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// replaceExecutable は exe を bin の中身で置き換える。
// 同じディレクトリに一時ファイルを作ってから rename するので、途中で失敗しても exe は壊れない。
// Windows は実行中のファイルを上書きできないので、先に exe を exe.old に退けてから置き、
// exe.old は次に naisen を起動したときに消す（removeOldExecutable）。
func replaceExecutable(exe string, bin []byte) error {
	mode := os.FileMode(0o755)
	if st, err := os.Stat(exe); err == nil {
		mode = st.Mode().Perm() | 0o111
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".naisen-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename したあとは何もしない
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(tmpName, exe); err != nil {
			os.Rename(old, exe) // 元に戻す
			return err
		}
		return nil
	}
	return os.Rename(tmpName, exe)
}

// removeOldExecutable は Windows で self-update が退けた naisen.exe.old を消す（使用中なら何もしない）。
func removeOldExecutable() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := executablePath(); err == nil {
		os.Remove(exe + ".old")
	}
}

// compareVersions は vX.Y.Z の形の版を比べる（a<b なら負、a==b なら 0、a>b なら正）。
// どちらかが版の形でなければ（(devel) など）known=false を返す。
// v0.2.0-0.20261101-abcdef のような go install の仮の版は、- より前の数字だけで比べる。
func compareVersions(a, b string) (cmp int, known bool) {
	pa, ok1 := parseVersion(a)
	pb, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
