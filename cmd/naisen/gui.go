package main

import (
	"archive/zip"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/perorin0418/naisen/internal/proto"
	"github.com/perorin0418/naisen/internal/session"
)

//go:embed gui.html
var guiHTML []byte

const guiUsage = `使い方: naisen gui [--line 番号] [--dir DIR] [--no-open]

ドラッグ&ドロップでファイルをやり取りするウィンドウを開きます（通話中に使います。通話を始める前に開いてもかまいません）。

  - ウィンドウには相手側のディレクトリの一覧が出ます。フォルダーはダブルクリックで開きます
  - エクスプローラーなどからファイル・フォルダーをウィンドウにドロップすると、表示中の相手側のディレクトリに送ります
  - 一覧の項目をウィンドウの外（エクスプローラーなど）へドラッグすると、そこに受け取ります
    （Edge・Chrome のときだけ。フォルダーは zip になります）
  - 「受け取る」ボタンでは、--dir のディレクトリ（既定はカレントディレクトリ）に受け取ります

ウィンドウは Edge か Chrome のアプリモード（アドレスバーのないウィンドウ）で開きます。
見つからなければ既定のブラウザーで開きます。画面を表示できない環境（koya の中など）では、表示した URL を
同じマシンのブラウザーで開いてください。ウィンドウを閉じると、しばらくして naisen gui も終わります。

オプション:
  --line 番号  使う電話（このマシンの naisen・naisen call の番号）。電話が1つだけか、通話中の電話が1つだけなら省略できる
               （環境変数 NAISEN_LINE でも指定できる）。対話プロンプトの gui はその電話の番号で開く
  --dir DIR   「受け取る」ボタンで受け取る先（既定はカレントディレクトリ）
  --no-open   ウィンドウを開かず、URL を表示するだけにする
`

type guiServer struct {
	line     int // 使う電話の番号
	token    string
	localDir string
	host     string // 受け付ける Host ヘッダー

	mu       sync.Mutex
	lastSeen time.Time
	closed   bool
}

func cmdGui(args []string) int {
	var dir string
	var noOpen bool
	var line *int
	_, ok := parseFlags("gui", args, func(fs *flag.FlagSet) {
		fs.StringVar(&dir, "dir", ".", "")
		fs.BoolVar(&noOpen, "no-open", false, "")
		line = lineFlag(fs)
	})
	if !ok {
		return 2
	}
	number, err := guiLine(*line)
	if err != nil {
		errorf("%v", err)
		return 2
	}
	localDir, err := filepath.Abs(dir)
	if err == nil {
		if fi, serr := os.Stat(localDir); serr != nil || !fi.IsDir() {
			err = fmt.Errorf("%s はディレクトリではありません", localDir)
		}
	}
	if err != nil {
		errorf("%v", err)
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		errorf("待ち受けられません: %v", err)
		return 1
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	g := &guiServer{line: number, token: hex.EncodeToString(tok), localDir: localDir, host: ln.Addr().String(), lastSeen: time.Now()}
	u := fmt.Sprintf("http://%s/?t=%s", g.host, g.token)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", g.page)
	mux.HandleFunc("GET /api/status", g.api(g.status))
	mux.HandleFunc("GET /api/ls", g.api(g.list))
	mux.HandleFunc("POST /api/upload", g.api(g.upload))
	mux.HandleFunc("POST /api/get", g.api(g.get))
	mux.HandleFunc("GET /api/download/{name}", g.downloadHandler)
	mux.HandleFunc("POST /api/bye", g.api(g.bye))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	logf("番号 %d のウィンドウを開きます: %s", number, u)
	if noOpen || !openWindow(u) {
		logf("ブラウザーで上の URL を開いてください（終わるには Ctrl-C）")
	}

	in := newInput(false)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-in.sig:
			fmt.Fprintln(os.Stderr)
			return 0
		case <-tick.C:
			g.mu.Lock()
			idle := time.Since(g.lastSeen)
			closed := g.closed
			g.mu.Unlock()
			// ウィンドウが閉じられた（bye が届いて、その後に開き直されていない）か、長く音沙汰がなければ終わる。
			// 最小化したウィンドウではタイマーが1分に1回ほどに間引かれるので、長めに待つ。
			if (closed && idle > 5*time.Second) || idle > 3*time.Minute {
				logf("ウィンドウが閉じられたので終わります")
				return 0
			}
		}
	}
}

// guiLine は gui で使う電話の番号を決める。want が 0 なら、このマシンの電話が1つだけか、
// 通話中の電話が1つだけのときにそれを選ぶ（gui は通話の前から開いておけるため）。
func guiLine(want int) (int, error) {
	if want != 0 {
		if _, err := session.DialLine(want); err != nil {
			return 0, err
		}
		return want, nil
	}
	infos := lineInfos()
	if len(infos) == 1 {
		return infos[0].Number, nil
	}
	if len(infos) == 0 {
		return 0, errors.New("このマシンで動いている naisen がありません（先に naisen か naisen call を起動してください）")
	}
	if n, err := pickLine(0); err == nil {
		return n, nil
	}
	var names []string
	for _, i := range infos {
		names = append(names, "  "+describe(i))
	}
	return 0, fmt.Errorf("このマシンの電話が複数あります。--line 番号 で選んでください:\n%s", strings.Join(names, "\n"))
}

func (g *guiServer) dial() (net.Conn, error) { return session.DialLine(g.line) }

func (g *guiServer) touch(closed bool) {
	g.mu.Lock()
	g.lastSeen = time.Now()
	g.closed = closed
	g.mu.Unlock()
}

func (g *guiServer) page(w http.ResponseWriter, r *http.Request) {
	if !g.sameHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(guiHTML)
}

// sameHost は Host ヘッダーが自分のアドレスかを確かめる（DNS の付け替えでほかのサイトから呼ばれないように）。
func (g *guiServer) sameHost(r *http.Request) bool { return r.Host == g.host }

// api はトークン（URL に含まれる文字列）を確かめてから h を呼ぶ。結果は JSON で返す。
func (g *guiServer) api(h func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := r.Header.Get("X-Naisen-Token")
		if t == "" {
			t = r.URL.Query().Get("t")
		}
		if !g.sameHost(r) || subtle.ConstantTimeCompare([]byte(t), []byte(g.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		g.touch(false)
		v, err := h(r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (g *guiServer) status(r *http.Request) (any, error) {
	res := map[string]any{"local": g.localDir, "line": g.line, "inCall": false}
	c, err := g.dial()
	if err != nil {
		res["message"] = fmt.Sprintf("番号 %d の naisen が終わっています", g.line)
		return res, nil
	}
	defer c.Close()
	msg, err := session.Simple(c, proto.OpStatus)
	if err != nil {
		return nil, err
	}
	var info proto.LineInfo
	if err := json.Unmarshal([]byte(msg), &info); err != nil {
		return nil, err
	}
	res["inCall"] = info.InCall
	if info.InCall {
		res["message"] = info.Status
	} else {
		res["message"] = fmt.Sprintf("番号 %d: 通話していません。通話が始まると、ここに相手側のファイルが出ます", g.line)
	}
	return res, nil
}

func (g *guiServer) list(r *http.Request) (any, error) {
	c, err := g.dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return session.List(c, r.URL.Query().Get("path"))
}

type itemResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Msg  string `json:"msg"`
}

// upload はドロップされたファイルを一時ディレクトリに受け、相手側の dir に送る。
// フォームの各ファイルの名前は "f:" + encodeURIComponent(ドロップした位置からの相対パス)。
func (g *guiServer) upload(r *http.Request) (any, error) {
	q := r.URL.Query()
	dest, force := q.Get("dir"), q.Get("force") == "1"
	if dest == "" {
		return nil, errors.New("送り先がありません")
	}
	tmp, err := os.MkdirTemp("", "naisen-gui-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	var tops []string
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name, ok := strings.CutPrefix(part.FormName(), "f:")
		if !ok {
			part.Close()
			continue
		}
		rel, err := url.PathUnescape(name)
		if err != nil {
			return nil, err
		}
		rel = path.Clean(strings.ReplaceAll(rel, `\`, "/"))
		if rel == "." || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, ":") {
			return nil, fmt.Errorf("不正な名前です: %q", name)
		}
		top, _, _ := strings.Cut(rel, "/")
		if !seen[top] {
			seen[top] = true
			tops = append(tops, top)
		}
		local := filepath.FromSlash(rel)
		if d := filepath.Dir(local); d != "." {
			if err := root.MkdirAll(d, 0o755); err != nil {
				return nil, err
			}
		}
		f, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(f, part)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		part.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(tops) == 0 {
		return nil, errors.New("送るファイルがありません（空のフォルダーは送れません）")
	}
	var res []itemResult
	for _, top := range tops {
		c, err := g.dial()
		if err != nil {
			return nil, err
		}
		rr, err := session.Send(c, filepath.Join(tmp, top), dest, force, nil)
		c.Close()
		if err != nil {
			res = append(res, itemResult{Name: top, Msg: err.Error()})
		} else {
			res = append(res, itemResult{Name: top, OK: true, Msg: fmt.Sprintf("送りました → 相手側 %s（%s）", rr.Path, rr.Stats)})
		}
	}
	return res, nil
}

// get は相手側の path を、この側の localDir に受け取る。
func (g *guiServer) get(r *http.Request) (any, error) {
	q := r.URL.Query()
	c, err := g.dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rr, err := session.Get(c, q.Get("path"), g.localDir, q.Get("force") == "1", nil)
	if err != nil {
		return nil, err
	}
	return itemResult{Name: filepath.Base(rr.Path), OK: true, Msg: fmt.Sprintf("受け取りました → %s（%s）", rr.Path, rr.Stats)}, nil
}

func (g *guiServer) bye(r *http.Request) (any, error) {
	g.touch(true)
	return map[string]bool{"ok": true}, nil
}

// downloadHandler は相手側の path を受け取って、ブラウザーにそのまま渡す（ウィンドウの外へのドラッグで使う）。
// フォルダーは zip にする。ドラッグ先からは URL だけで取りに来るので、トークンは URL の t で受け取る。
func (g *guiServer) downloadHandler(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("t")
	if !g.sameHost(r) || subtle.ConstantTimeCompare([]byte(t), []byte(g.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g.touch(false)
	tmp, err := os.MkdirTemp("", "naisen-gui-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	c, err := g.dial()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rr, err := session.Get(c, r.URL.Query().Get("path"), tmp, false, nil)
	c.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fi, err := os.Stat(rr.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := filepath.Base(rr.Path)
	if !fi.IsDir() {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		http.ServeFile(w, r, rr.Path)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	zw := zip.NewWriter(w)
	_ = filepath.WalkDir(rr.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() && !d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(rr.Path), p)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = rel
		if d.IsDir() {
			h.Name += "/"
			_, err = zw.CreateHeader(h)
			return err
		}
		h.Method = zip.Deflate
		zf, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(zf, f)
		return err
	})
	_ = zw.Close()
}

// openWindow は url を Edge か Chrome のアプリモードのウィンドウで開く。なければ既定のブラウザーで開く。
func openWindow(u string) bool {
	app := []string{"--app=" + u, "--window-size=960,680"}
	var browsers [][]string
	switch runtime.GOOS {
	case "windows":
		var cands []string
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			cands = append(cands,
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
		}
		for _, p := range cands {
			if _, err := os.Stat(p); err == nil {
				browsers = append(browsers, append([]string{p}, app...))
			}
		}
		browsers = append(browsers, []string{"rundll32", "url.dll,FileProtocolHandler", u})
	case "darwin":
		for _, name := range []string{"Microsoft Edge", "Google Chrome", "Chromium"} {
			if _, err := os.Stat("/Applications/" + name + ".app"); err == nil {
				browsers = append(browsers, append([]string{"open", "-na", name, "--args"}, app...))
			}
		}
		browsers = append(browsers, []string{"open", u})
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		for _, name := range []string{"microsoft-edge", "google-chrome", "chromium", "chromium-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				browsers = append(browsers, append([]string{p}, app...))
			}
		}
		browsers = append(browsers, []string{"xdg-open", u})
	}
	for _, b := range browsers {
		cmd := exec.Command(b[0], b[1:]...)
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }()
			return true
		}
	}
	return false
}
