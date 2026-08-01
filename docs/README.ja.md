# cepm(日本語版)

**C**hrome **E**xtension **P**ackage **M**anager — git で配布している unpacked
な Chrome 拡張を、自動で最新に保つツールです。

> この文書は [README.md](../README.md) の日本語版です。内容が食い違っていた
> 場合は英語版が正となります。

社内向けの Chrome 拡張は、Chrome Web Store が使えないため git リポジトリで
配布されることがよくあります。利用者は各自 clone して *chrome://extensions*
の「パッケージ化されていない拡張機能を読み込む」で読み込みますが、面倒なのは
その後です。更新のたびに `git pull` して、さらに拡張ごとに再読み込みボタンを
押す必要があります。cepm はこの両方を自動化します。

```console
$ cepm install git@github.example.com:team/internal-extensions.git
```

拡張ごとに一度だけ「パッケージ化されていない拡張機能を読み込む」を行えば、
あとは自動です。Chrome が起動している間、cepm は 1 時間ごとに pull し、
ファイルが変わった拡張だけを再読み込みします(Chrome 起動の 1〜2 分後にも
一度走るので、起動直後から最新の状態になります)。任意のタイミングで実行
したいときは `cepm update` です。

## 仕組み

```
Chrome ── cepm helper 拡張(cepm setup が生成、最初に一度だけ読み込む)
             │  Native Messaging(stdio。Chrome がプロセスを起動・管理)
             ▼
        cepm native host ── 定期的な git pull ── 変更された拡張を再読み込み
             ▲                                    (management.setEnabled の
             │  Unix socket                        off→on = ディスクから再読込)
        cepm CLI(install / update / list / doctor)
```

- 常駐デーモンの設定は不要です。native host は Chrome が起動・終了させます。
- Chrome が起動していなくても `cepm update` は pull できます。unpacked 拡張は
  次の Chrome 起動時にディスクから読み直されるためです。
- 1 つのリポジトリに拡張がいくつ入っていても構いません(`manifest.json` の
  あるディレクトリを自動的に検出します)。
- cepm 自身の更新も手間なく反映されます。Chrome は安定したパスの launcher
  スクリプト(`~/.cepm/bin/cepm-host`)経由で host を起動し、cepm を実行する
  たびにその参照先が現在のバイナリへ更新されます(Homebrew、`go install`、
  mise の shim、ファイルの差し替えのいずれでも動きます)。helper 拡張の
  ファイルも host の接続時に更新され、次回の Chrome 起動から有効になります。

## セットアップ

```console
$ go install github.com/be-hase/cepm/cmd/cepm@latest   # またはリリースバイナリを取得
$ cepm setup
$ # 一度だけ: chrome://extensions → デベロッパーモード →
$ #           「パッケージ化されていない拡張機能を読み込む」→ ~/.cepm/helper
$ cepm doctor        # すべて緑になっていることを確認
```

対応 OS は macOS と Linux です(Windows はレジストリによる host 登録と、
拡張 ID の算出方式が異なるため未対応です)。

## コマンド

```console
$ cepm install <git-url>          # clone して登録(このあと一度だけ読み込む)
$ cepm update                     # すべて pull し、変更された拡張を再読み込み
$ cepm list                       # 登録内容と、Chrome に読み込まれているか
$ cepm enable <repo>[/<dir>]      # リポジトリ内の拡張を使い始める
$ cepm disable <repo>[/<dir>]     # 使うのをやめる(登録は「利用可能」として残る)
$ cepm reload                     # pull せず再読み込みだけ(ローカルでの開発用)
$ cepm cleanup                    # 改名・削除で壊れた Chrome 側のエントリを削除
$ cepm uninstall <name>           # 登録解除して clone を削除
$ cepm doctor                     # セットアップと接続状況の診断
$ cepm reset                      # state が壊れたとき、state と clone を退避してやり直す
$ cepm id <path>                  # ディレクトリに対応する拡張 ID を表示
```

覚えておくと便利なフラグ: `cepm update --no-reload`(pull のみ)、
`cepm update --force`(ローカル変更を stash して pull)、
`cepm uninstall --keep-files`(登録解除するが clone は残す)、
`cepm list --json` / `cepm doctor --json`(スクリプト向け)、
任意のコマンドに `-v`(実行した git コマンドや host との通信を表示)。

### 複数の拡張を含むリポジトリ

リポジトリに複数の拡張が含まれる場合、`cepm install` はどれを使うか尋ねます
(Enter で全部。スクリプトからは `--only dir,dir` や `--all`)。選ばなかった
ものは「利用可能(available)」として登録されるだけで、更新や診断の対象には
なりません。あとから `cepm enable` で使い始められます。

手動での読み込みが必要な一箇所についても cepm が誘導します。ディレクトリの
パスがクリップボードに入り、chrome://extensions が開き、Chrome が実際に正しい
ディレクトリを読み込んだことを cepm が確認します。

あとからリポジトリに追加された拡張も「利用可能」として登録されるので、
勝手に読み込まれたり、催促されたりすることはありません。拡張のディレクトリが
改名・削除された場合は cepm がそれを報告し、有効にしていたという選択は新しい
パスへ引き継がれます。壊れた Chrome 側のエントリは `cepm cleanup` が
(Chrome 自身の確認ダイアログを通して)削除します。

### ブランチではなくバージョンタグを追う

リポジトリのデフォルトブランチに開発中のコードが入る場合は、タグを追う設定に
できます。この場合 cepm は常に最新のリリースバージョンだけを checkout します。

```console
$ cepm install <git-url> --track tag                  # 最新の安定版
$ cepm install <git-url> --track tag --prerelease     # v2.0.0-rc1 なども含める
$ cepm install <git-url> --track tag --tag-pattern "v1.*"   # 1.x 系に留まる
```

`--tag-pattern` は「どのタグを候補にするか」を絞るものです。候補に残ったタグは
バージョン番号(`v1.2.3`)である必要があります。cepm はそれを比較するためです。

比較は semver で行うので `v1.10.0` は `v1.9.0` より新しく扱われます。
プレリリース(`v2.0.0-rc1`)は `--prerelease` を付けない限り無視され、
バージョン番号でないタグ(`nightly` など)も無視されます。一致する
バージョンタグが 1 つもない場合は、最後に打たれたタグを勝手に追うのではなく
エラーとして報告します。

**GitHub Releases との関係。** cepm は Releases API ではなくタグを見ます。
そのため利用者の端末に GitHub のトークンは不要で、`api.github.com` に
アクセスできない環境でも更新は動きます。実運用ではほぼ一致します(リリースを
公開するとタグが push され、下書きのリリースはタグを作らないので見えません)が、
次の 2 点だけ違います。

- リリースを作らずにタグだけ push した場合も、通常のタグとして追われます。
  配布する意図のあるものにだけタグを打ってください。
- リリースの「プレリリース」チェックボックスは cepm からは見えません。判断は
  タグ名で行うので、プレリリースは `v2.0.0` ではなく `v2.0.0-rc1` のように
  命名してください。

### リポジトリ側の設定(`cepm.toml`、任意)

拡張の作者はリポジトリのルートに `cepm.toml` を置けます。利用者が単に
`cepm install <url>` するだけで適切な設定になります。

```toml
# 拡張ディレクトリの明示(自動検出をやめる):
extensions = ["dist/sidebar", "dist/search"]

# 利用者にタグ追従を推奨する:
track = "tag"
tag_pattern = "v*"
# prerelease = true   # 利用者にリリース候補も配る場合
```

作者向けの注意: 拡張ディレクトリの改名は、利用者全員にとって破壊的な変更です。
Chrome は拡張 ID をパスから導出するため、利用者は新しいディレクトリを一度
読み込み直す必要があります(cepm が手順を案内します)。ディレクトリ名は安定
させるか、`manifest.json` に `key` を入れて ID を固定してください。cepm は
`key` を尊重するので、移動しても ID が変わりません。

### 利用者側の設定(`~/.cepm/config.toml`、任意)

```toml
[update]
interval = "1h"    # Chrome 起動中の自動更新の間隔(最小 "1m")
auto     = true    # false にすると "cepm update" 実行時のみ更新

[git]
stash_dirty = false  # clone にローカル変更があるとき stash/pop して pull する
```

既定では、ローカルに変更のあるリポジトリは警告つきで skip されます。作業中の
内容が失われることはありません。`cepm update --force` を使うと、pull の前後で
stash と復元を行います。

## 困ったときは

**まず `cepm doctor` を実行してください。** git、helper のファイル、
Native Messaging の登録、Chrome が現在接続しているか、登録された各拡張が実際に
読み込まれているか、という一連の流れをすべて確認します。失敗として報告される
項目には、必ず対処コマンドが添えられます。以下はその「なぜ」を知りたいときの
説明です。

**拡張に変更が反映されない。** `cepm list` で読み込み済みと表示され、
`cepm update` も再読み込みしたと言っているのに挙動が古いままなら、変更箇所は
おそらく `manifest.json` です。Chrome は拡張を読み込んだ時点の manifest を
キャッシュし続けるため、これだけは Chrome の再起動が必要です。該当する変更が
あったときは cepm がその旨を表示し、再起動するまで doctor が指摘し続けます。

**「Chrome is not reachable」と出る。** 異常ではありません。helper は Chrome が
起動している間しか接続しないため、Chrome を終了していれば当然この表示になります。
pull 自体は `cepm update` で動き、新しいコードは次の Chrome 起動時に反映されます。
Chrome が起動しているのにこの表示が出る場合は、chrome://extensions で helper が
読み込まれ有効になっているかを確認し、もう一度 `cepm doctor` を実行してください。

**Chrome から拡張が消えた、またはエラー表示になっている。** リポジトリ側で拡張の
ディレクトリが改名・削除されたときに起こります。ID はパスから導出されるため、
古いエントリが宙に浮いた状態です。`cepm cleanup` がそれらを削除し(Chrome が
1 つずつ確認を求めます)、改名の場合は代わりに読み込むべきディレクトリを
cepm が教えます。

**「cepm cannot use this state file」と出る。** `~/.cepm/state.json` が cepm 以外に
書き換えられたか、書き込みが中断された状態です。解釈できない state を推測で
扱うことはせず、Chrome にもディスクにも一切触れずに停止します。`cepm reset` を
実行すると、state ファイルと clone を `~/.cepm/` 配下のタイムスタンプ付き
バックアップへ**移動**します(削除はしません)。そのうえで、使っている
リポジトリを `cepm install` し直してください。元の URL はバックアップ内の
`state.json` から、あるいは各 clone から
`git -C <backup>/repos/<name> remote get-url origin` で確認できます。

**「exists but no repository is registered for it」と出る。** clone の配置と登録の
あいだで install が中断された状態です。そのディレクトリを削除するか
`cepm reset` を実行してから、もう一度 install してください。

**特に問題はないのに自動更新されない。** 自動更新は native host の中で動き、
host は Chrome が起動している間だけ存在します。また pull を行うのは 1 プロセス
だけです。`~/.cepm/config.toml` の `[update] auto` を確認してください。間隔は
既定で 1 時間です(Chrome 起動直後に一度走ります)。詳細は
`~/.cepm/logs/host.log` に記録されています。

## 注意事項・制限

- 拡張を最初に読み込む操作だけは必ず手動です。Chrome に「unpacked を読み込む」
  API がないためです。cepm は読み込むべきディレクトリを正確に表示します。
  表示されるのは新規の拡張のときだけです。
- **`manifest.json` の変更には Chrome の再起動が必要です。** unpacked 拡張の
  再読み込みはディスクからコードを読み直しますが、manifest は読み込み時に
  キャッシュしたものが使われ続けます。したがってコードの更新は即座に反映され、
  バージョンの変更・新しい権限・新規に宣言したファイルは Chrome の再起動後に
  反映されます。cepm は pull した変更が `manifest.json` に及んだときにそれを
  伝え、`cepm doctor` は反映されるまで指摘し続けます。
- Chrome の終了中に更新を pull した場合、コードはディスクに置かれますが Chrome は
  キャッシュしたものを実行している可能性があります。そのため cepm は、Chrome が
  接続した直後に、有効な拡張をすべて一度再読み込みします。
- cepm を更新すると helper 拡張のファイルも自動的に書き換わりますが、動作中の
  helper は次の Chrome 起動まで現在のバージョンのままです(自分自身を再読み込み
  すると cepm との接続が切れてしまうためです)。
- **helper を読み込むのは 1 つの Chrome プロファイルだけにしてください。**
  自動再読み込みは、最初に helper が接続したプロファイルに対して行われます。
  2 つ目のプロファイルはファイルこそ更新されますが、再起動するまで古いコードを
  実行し続けます。Chrome の**種別**(Stable、Beta、Canary、Chromium)については
  cepm が管理します。`cepm setup --chrome-variant <x>` は指定した Chrome だけを
  登録し、他からは登録を削除するので、切り替えはコマンド 1 つで済みます
  (加えて、以前の Chrome を終了してください。終了するまでは既存の接続が
  残ります。setup はそれを検出すると警告します)。同一 Chrome 内のプロファイルは
  この登録を共有するため、2 つ目のプロファイルに helper を入れないことは利用者側の
  責任になります。
- helper 拡張には `management` 権限(他の拡張の有効・無効を切り替えるため)、
  `nativeMessaging`、`alarms` が必要です。ページのデータには一切触れません。
- すべて `~/.cepm/` 配下に置かれます(clone、state、ログは
  `~/.cepm/logs/host.log`)。パーミッションは所有者のみです。clone は社内拡張の
  ソースそのものなので、同じマシンの他ユーザーからは読めません。
- cepm は指示されていないものを削除しません。`uninstall` は自身が作成した clone を
  削除し(`--keep-files` で残せます)、`reset` は移動のみ、Chrome からの削除は
  必ず Chrome 自身の確認ダイアログを通します。

## 開発

```console
$ make build   # bin/cepm
$ make test    # ユニットテストと結合テスト
$ make e2e     # 実際の Chrome を操作(初回は Chrome for Testing を取得)
$ make lint    # gofmt + go vet
```

`make e2e` は本物です。使い捨てプロファイルで Chrome を起動し、helper と
テスト用拡張を読み込み、`git push` が Chrome の実行しているコードを実際に
変えることを検証します(自動更新の経路、Chrome 停止中に適用された更新、
helper の更新も含みます)。`CEPM_E2E_HEADED=1` を付けると動作を目視できます。

`docs/e2e-checklist.md` には、人間が確認する必要のある項目(対話プロンプト、
クリップボード、Chrome 自身の確認ダイアログ)がまとまっています。

コードに手を入れる場合は [AGENTS.md](../AGENTS.md) も参照してください。設計上の
不変条件(ロックの取り方、state 保存とファイル操作の順序など)が書かれています。

## ライセンス

MIT
