package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const sampleChecksums = `bdd3bab514303f819b74f0ae5745bb6dd35d0dbeba7a0a9f38c99800bc6c88f3  naisen_0.1.0_darwin_amd64.tar.gz
5f3677ec262838549ed012a7cdee929ff1504e22391476420b399990907af684  naisen_0.1.0_linux_amd64.tar.gz
ec70d400a32cc858c554d4db23ceb97ac6475447e530cd6057dbb1a93249cd1a  naisen_0.1.0_linux_arm64.tar.gz
f5709c7b26780290f2d43e0eba4a2fd751a042eedbad8e656cbdeee9e353a257  naisen_0.1.0_windows_amd64.zip
`

func TestPickAsset(t *testing.T) {
	for _, c := range []struct{ goos, goarch, asset string }{
		{"linux", "amd64", "naisen_0.1.0_linux_amd64.tar.gz"},
		{"linux", "arm64", "naisen_0.1.0_linux_arm64.tar.gz"},
		{"windows", "amd64", "naisen_0.1.0_windows_amd64.zip"},
	} {
		rel, err := pickAsset([]byte(sampleChecksums), c.goos, c.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.goarch, err)
		}
		if rel.asset != c.asset || rel.version != "v0.1.0" || len(rel.sum) != sha256.Size {
			t.Errorf("%s/%s: %+v", c.goos, c.goarch, rel)
		}
	}
	if _, err := pickAsset([]byte(sampleChecksums), "windows", "arm64"); err == nil {
		t.Error("無いファイルでエラーにならない")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		cmp   int
		known bool
	}{
		{"v0.1.0", "v0.1.0", 0, true},
		{"v0.1.0", "v0.2.0", -1, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.2.0-0.20261101000000-abcdef", "v0.2.0", 0, true},
		{"(devel)", "v0.1.0", 0, false},
		{"v0.1", "v0.1.0", 0, false},
	} {
		cmp, known := compareVersions(c.a, c.b)
		if cmp != c.cmp || known != c.known {
			t.Errorf("compareVersions(%q, %q) = %d, %v", c.a, c.b, cmp, known)
		}
	}
}

func makeTarGz(t *testing.T, dir, bin string, content []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{dir + "/README.md", []byte("readme")}, {dir + "/" + bin, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func makeZip(t *testing.T, dir, bin string, content []byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{dir + "/README.md": []byte("readme"), dir + "/" + bin: content} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	zw.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	want := []byte("new binary")
	got, err := extractBinary("naisen_0.2.0_linux_amd64.tar.gz", makeTarGz(t, "naisen_0.2.0_linux_amd64", "naisen", want))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("tar.gz: %q, %v", got, err)
	}
	got, err = extractBinary("naisen_0.2.0_windows_amd64.zip", makeZip(t, "naisen_0.2.0_windows_amd64", "naisen.exe", want))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("zip: %q, %v", got, err)
	}
}

// TestSelfUpdateFromServer は、手元の HTTP サーバーをリリースに見立てて、取得・照合・置き換えまでを通す。
func TestSelfUpdateFromServer(t *testing.T) {
	want := []byte("naisen v0.2.0 binary")
	dir := fmt.Sprintf("naisen_0.2.0_%s_%s", runtime.GOOS, runtime.GOARCH)
	asset, data := dir+".tar.gz", makeTarGz(t, dir, "naisen", want)
	if runtime.GOOS == "windows" {
		asset, data = dir+".zip", makeZip(t, dir, "naisen.exe", want)
	}
	sum := sha256.Sum256(data)
	checksums := fmt.Sprintf("%x  %s\n", sum, asset)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(checksums)) })
	mux.HandleFunc("/download/v0.2.0/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := &updater{ctx: context.Background(), base: srv.URL, client: srv.Client()}
	rel, err := u.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if rel.version != "v0.2.0" || rel.asset != asset {
		t.Fatalf("resolve: %+v", rel)
	}
	bin, err := u.fetchBinary(rel)
	if err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(t.TempDir(), "naisen")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(exe, bin); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, want) {
		t.Errorf("置き換え後: %q", got)
	}
	if st, _ := os.Stat(exe); runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		t.Errorf("実行権限がない: %v", st.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Errorf("一時ファイルが残っている: %v", entries)
	}

	// 中身が改ざんされていたら置き換えない
	rel.sum = make([]byte, sha256.Size)
	if _, err := u.fetchBinary(rel); err == nil {
		t.Error("SHA-256 が合わないのにエラーにならない")
	}
}
