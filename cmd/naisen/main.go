// naisen は koya の中（コンテナ）と外（ホスト）の間で、ファイルとコマンドをやり取りするためのコマンド。
//
// ホストで naisen を起動して待ち受け、koya の中から naisen call で発信する。
// ホストで受諾すると通話が始まり、どちらの側からも相手側でのコマンドの実行とファイルの送受信ができる。
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = ""

const usage = `naisen - koya の中と外で、ファイルとコマンドをやり取りする内線電話（同じマシンの中だけでつながる）

使い方:
  naisen [--number N] [--bind ADDR] [--once]   内線を起動して着信を待つ（起動すると自分の番号が出る）
  naisen --auto [--number N] ...               自動で取る内線を起動する（koya の中で起動しておき、ホストからかける）
  naisen call [番号][@ホスト]                   その番号に電話をかける（番号を省くと、待っている内線を探す）
  naisen list [--local] [@ホスト]               ネットワークで待っている内線の一覧（電話帳）

通話中に、どちらの側でも別の端末から使えるコマンド（対話プロンプトでも同じ名前で使えます）:
  naisen run [--line N] [-C DIR] [-n] [--] COMMAND...   相手側でコマンドを実行する
  naisen send [--line N] [-f] LOCAL [REMOTE]            こちらのファイル・ディレクトリを相手側に送る
  naisen get [--line N] [-f] REMOTE [LOCAL]             相手側のファイル・ディレクトリを受け取る
  naisen gui [--line N] [--dir DIR] [--no-open]         ドラッグ&ドロップでファイルをやり取りするウィンドウを開く
  naisen status [--line N]                              このマシンの電話の状態を表示する
  naisen hangup [--line N]                              電話を切る

  naisen self-update [--check]          最新版に更新する（GitHub Releases から取って、このファイルを置き換える）
  naisen version                        バージョンを表示する
  naisen help [コマンド]                 ヘルプを表示する

流れ:
  1. ホストで naisen を起動する。「あなたの番号は 1 です」のように番号が出る
  2. koya の中で naisen call 1 を実行する（待っている電話が1つなら、naisen call だけでもよい）
  3. ホストの画面に着信が出るので、発信側と同じ確認コードか確かめて y で受ける
  4. 両方の画面に naisen(番号)> が出る。run・send・get・gui でやり取りし、bye で切る

電話はいくつでも起動できます（番号 1〜49。番号 N の電話は TCP の 7650+N で待ち受けます）。
1つの電話が同時に通話できる相手は1つです。通話中にかかってきた電話は「話し中」で断ります。
このマシンで通話中の電話が複数あるときは、単発コマンドや gui で --line 番号（自分の電話の番号）を付けて選びます。

相手側のパスとコマンドの作業ディレクトリは、相手の naisen を起動したディレクトリが基準です。

環境変数:
  NAISEN_HOST      naisen call・list でかける先のホスト（既定は、コンテナの中なら host.containers.internal、
                  host.docker.internal、127.0.0.1、ホストなら 127.0.0.1）
  NAISEN_BIND      naisen の待ち受けアドレス（既定 127.0.0.1）
  NAISEN_LINE      単発コマンドと gui で使う電話の番号（--line と同じ）
  NAISEN_SHELL     相手から頼まれたコマンドを実行するシェル（既定 sh -c、Windows は cmd /C）
  NAISEN_SOCK_DIR  制御ソケットを置くディレクトリ（電話と単発コマンドの間で使う）
`

const listenUsage = `使い方: naisen [--number N] [--bind ADDR] [--once] [--auto]

電話を起動して着信を待ちます。起動すると「あなたの番号は 1 です」のように自分の番号が出ます。
番号は、このマシンで空いている最小の番号（1〜49）です。番号 N の電話は TCP の 7650+N で待ち受けます。
着信があると、発信側の情報と確認コードを表示して、受けるかどうかをたずねます。
受けると対話プロンプト（naisen(番号)>）になります。電話が切れたら、また着信を待ちます。
着信を受けるかを答えるため、端末から起動してください。

オプション:
  --number N   番号を指定する（使われていればエラー）
  --bind ADDR  待ち受けるアドレス（既定 127.0.0.1。NAISEN_BIND でも指定できる）。
               koya のコンテナからホストの 127.0.0.1 に届かない環境（Linux で docker を直接使う場合など）では
               0.0.0.0 などを指定します。その場合はほかのマシンからも着信できるようになります
  --once       1回通話したら終わる
  --auto       自動で取る（着信をたずねずに受ける）。端末がなくても起動できる（バックグラウンドなど）。
               koya の中で起動しておけば、ホストからいつでもかけられる。コンテナの中では --bind の既定が 0.0.0.0 になる
               （ホストから届くように、コンテナを作るときにポート 7650+番号 を公開しておく。koya なら
               -p 127.0.0.1:7661:7661 のように。番号はホストの内線とぶつからないよう --number 11 などで決めておくとよい）。
               このポートに届く誰からの着信も確かめずに受けるので、届く範囲が限られているときだけ使う
`

const callUsage = `使い方: naisen call [番号][@ホスト]

その番号の電話にかけます。相手が受けると通話が始まり、対話プロンプト（naisen(番号)>）になります。
番号を省くと、ホストで待っている電話を探して、空いている電話が1つならそこにかけます。
複数あれば一覧（電話帳）を表示するので、番号を指定してかけ直してください。

ホストを省くと、NAISEN_HOST、なければ host.containers.internal（podman）、host.docker.internal（docker）、
127.0.0.1 の順に探します。

例:
  naisen call                  待っている内線が1つならそこにかける
  naisen call 2                番号 2 にかける
  naisen call 2@myhost         myhost の番号 2 にかける

かける側の電話にも、このマシンで空いている番号が付きます（単発コマンドや gui の --line で使います）。
標準入力が端末でないとき（バックグラウンドで動かすときなど）は、対話プロンプトを出さずに通話だけを続けます。
その間は別の端末から naisen run・send・get・gui が使えます。naisen hangup か Ctrl-C で切ります。
`

const listUsage = `使い方: naisen list [--local] [@ホスト]

ネットワークで待っている内線の一覧（かけ方・状態・取り方・相手・作業ディレクトリ）を表示します。着信にはなりません。
次の2つを合わせて表示します。
  - ホストの候補（naisen call と同じ。ホストでは 127.0.0.1）の番号 1〜49 に TCP で問い合わせて見つけた内線
    （このマシンの内線と、ポートを公開したコンテナの内線）
  - LAN にブロードキャスト（UDP）で問い合わせて、返事のあった内線（--bind 0.0.0.0 などで待っている、ほかのマシンの内線）

オプション:
  --local  ブロードキャストをせず、ホストの候補だけを調べる
  @ホスト  そのホストだけを調べる
`

const lineHelp = `
--line N を付けると、このマシンの番号 N の電話を使います（環境変数 NAISEN_LINE でも指定できる）。
省略すると、通話中の電話が1つだけのときにそれを使います。
`

const runUsage = `使い方: naisen run [--line N] [-C DIR] [-n] [--] COMMAND...

相手側のシェル（sh -c、Windows は cmd /C）でコマンドを実行し、出力を表示します。終了コードは相手側のものです。
引数が複数あるときは空白でつないで1つのコマンドラインにします（ssh と同じ）。
パイプなどを相手側で解釈させたいときは、まとめて引用符で囲みます: naisen run 'ls | wc -l'

オプション:
  -C DIR  相手側の作業ディレクトリ（既定は相手が naisen を起動したディレクトリ）
  -n      標準入力を渡さない

標準入力が端末でない（パイプやファイルの）ときは、その内容を相手側のコマンドに渡します。
端末を必要とするコマンド（vim、対話シェルなど）は使えません。Ctrl-C で相手側のコマンドを止めます。
` + lineHelp

const sendUsage = `使い方: naisen send [--line N] [-f] LOCAL [REMOTE]

こちらのファイルかディレクトリを相手側に送ります。
REMOTE を省略すると相手の作業ディレクトリに同じ名前で置きます。REMOTE が既存のディレクトリなら
その中に、そうでなければ REMOTE という名前で置きます。相対パスは相手の作業ディレクトリが基準です。

オプション:
  -f  置き先にすでにあれば上書きする（ディレクトリは中身を重ねる）
` + lineHelp

const getUsage = `使い方: naisen get [--line N] [-f] REMOTE [LOCAL]

相手側のファイルかディレクトリを受け取ります。REMOTE の相対パスは相手の作業ディレクトリが基準です。
LOCAL を省略するとカレントディレクトリに同じ名前で置きます。LOCAL が既存のディレクトリなら
その中に、そうでなければ LOCAL という名前で置きます。

オプション:
  -f  置き先にすでにあれば上書きする（ディレクトリは中身を重ねる）
` + lineHelp

const statusUsage = `使い方: naisen status [--line N]

このマシンで動いている電話（naisen・naisen call）の番号と状態を表示します。--line で1つに絞れます。
`

const hangupUsage = `使い方: naisen hangup [--line N]

電話を切ります。
` + lineHelp

var helps = map[string]string{
	"listen": listenUsage, "call": callUsage, "list": listUsage, "run": runUsage, "send": sendUsage,
	"get": getUsage, "gui": guiUsage, "status": statusUsage, "hangup": hangupUsage,
	"self-update": selfUpdateUsage,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	removeOldExecutable()
	if len(args) > 0 {
		switch args[0] {
		case "call":
			return cmdCall(args[1:])
		case "run":
			return cmdRun(args[1:])
		case "send":
			return cmdSend(args[1:])
		case "get":
			return cmdGet(args[1:])
		case "gui":
			return cmdGui(args[1:])
		case "list":
			return cmdList(args[1:])
		case "status":
			return cmdStatus(args[1:])
		case "hangup":
			return cmdHangup(args[1:])
		case "self-update":
			return cmdSelfUpdate(args[1:])
		case "listen":
			return cmdListen(args[1:])
		case "version", "--version":
			fmt.Println("naisen", versionString())
			return 0
		case "help", "-h", "--help":
			if len(args) > 1 {
				if h, ok := helps[args[1]]; ok {
					fmt.Print(h)
					return 0
				}
			}
			fmt.Print(usage)
			return 0
		}
	}
	return cmdListen(args)
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

func errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "naisen: "+format+"\n", a...)
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\rnaisen: "+format+"\n", a...)
}
