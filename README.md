# Camp

AIとの作業履歴と知識ベースを同じ場所に置く、個人用セルフホストWebアプリ。

Claude Codeのセッションと会話履歴をCLI側の都合から独立して保持し、横断検索でき、セッションの起動・対話もでき、トークン消費と残量が見え、Obsidian Vaultも同じ場所で読み書きできる。最終的にObsidianを置き換える。

**これは公開用の写しです。** 作業メモとレビュー記録（`dev/`）は、個人の Vault の中身を
名指しで参照しているため含めていません。要件・仕様・意思決定の経緯も手元の Obsidian Vault
側にあります。ここに入っているのは**コードと技術仕様（`docs/*`）**です。

## ステータス

**Phase 3.6 完了（Codex のセッション駆動）＋ 本物の codex app-server での outer gate まで（2026-09-11）。**Phase 0〜3、Phase 2.5（保持設計、M21〜M25）、M25.5（権限境界）、Phase 3.5（SSH経由のリモート起動）は済み。2026-09-01にリポジトリ作成。

次は **Phase 4（ビュー機構の移行・チャート・グラフビュー）**。

2026-09-01に4方向の評価（取り込み層の実証・プロトコルの実証・Obsidian置き換えの実現可能性・codexによる独立レビュー）を経てフェーズを再構成した。

```
$ campd ingest
main 46 / resume-sidecar 14 / stub 9 / subagent 3 / empty 10
projects 21（cwd 35個から）  sessions 58  runs 33  session_runs 42  messages 32,412
usage 7,119行（出力 4,700,098トークン／キャッシュ読み 1,689,492,039トークン）
初回 5.4秒 / 追記なしの2回目 66ms

$ campd search 認証
2026-09-01T22:41  tool_result  （会話の題名）
  …Tailscale内に閉じたうえで**アプリ認証を最初から入れる**（D-011）…
382件が3〜12msで返る。既定のトークナイザだと同じ語で38件しか返らない（docs/20-data-model.md）

$ campd thread 9182f0fd
4639行 / 会話1本（繋ぎ直さなければ7本） / 根5 / 繋ぎ直し6 / 迷子0 / 時刻の逆行37

$ campd files Human/Projects/ExampleProject.md -summary -n 2
 52回 / 6 セッション  …/Obsidian-vault/Human/Projects/ExampleProject.md
      2026-08-02 11:59 .. 2026-08-06 12:55  [edit backup read]
 10回 / 2 セッション  …/Obsidian-Vault/Human/Projects/ExampleProject.md
      2026-08-11 05:29 .. 2026-08-12 06:46  [mention edit read backup]
370パス / 2,414件。大文字小文字は畳まない（改名の前後で別物として残る）

$ campd capture    # file-history の実体（編集前の中身）を退避する。取り込みでも自動で走る
走査 809個 / 新規 809個（15.5MiB）/ 保管 4.7MiB（同じ中身で済んだ 58個）

$ campd backup Obsidian-vault/Human/Projects/ExampleProject.md -n 2
   400  2026-08-07 01:30  v5   279.4KiB  …/Obsidian-vault/Human/Projects/ExampleProject.md
        session 7c876746  （会話の題名）
   399  2026-08-06 12:34  v4   277.5KiB  …/Obsidian-vault/Human/Projects/ExampleProject.md
$ campd backup -show 400 | wc -l
3627
このディレクトリ（小文字v）はもう存在しない。中身が残っているのは Camp だけ

$ campd secrets    # 認証情報らしい場所を記録する。何も書き換えない（D-010）
14箇所 / 10メッセージ。raw_json 33,621件のSHA-256は走査前後で一致
当たりは既定で伏せる。14箇所すべて、伏せたまま偽陽性と判別できた

$ make && ./campd passwd && ./campd serve
addr    http://127.0.0.1:8785
画面    埋め込みの web/dist
認証    必須（/healthz を除く全経路）
未認証は /api/* が 401、画面は /login へ 302。画面の殻もバンドルも配らない
セッション一覧・本文・横断検索・使用量。画面の状態は全部URLに乗る

$ ./campd login-url   # 構築中の入口。1回だけ使える（既定2分）
http://127.0.0.1:8785/login?t=…
発行できるのは DB に書ける者だけ。その者はもう全部読めるので権限は増えない（D-021）

$ campd backfill   # 派生テーブルを messages から作り直す（ディスクは読まない）
```

| Phase | 内容 | 状態 |
|---|---|---|
| 0 | 取り込み・SQLite・**日本語対応**全文検索・最小UI | **完了**（M0〜M12） |
| 1 | Vault読み取り＋ノート↔セッション相互リンク | **完了**（M13〜M16） |
| 2 | ビュー機構の読み取り側＋LLM文脈コンパイラ＋**MCPサーバー公開** | **完了**（M17〜M20） |
| 2.5 | 保持設計（伏字化・保持ポリシー・暗号化バックアップ・監査ログ）＋権限境界 | **完了**（M21〜M25、M25.5） |
| 3 | セッション駆動（`claude -p` 双方向stream-json＋承認UI）・ローカルまで | **完了**（M26〜M31） |
| 3.5 | SSH経由のリモート起動（向こうに常駐させない） | **完了**（2026-09-11） |
| 3.6 | Codex のセッション駆動（このマシン、Camp 専用の置き場で） | **完了**（2026-09-11） |
| 4 | ビュー機構の移行（`.base` から独自定義へ）・チャート・グラフビュー | 次 |
| 5 | 編集面（CodeMirror 6）→ 書き手の移行 | 未着手 |
| 6 | モバイル（まずPWA） | 未着手 |
| 将来 | 向こうのホストで Codex・会話記録の取り込み（向こう・Codex）・再開・セッションごとの確認の度合いと範囲 | 後で必ずやる |

**当分CampはVaultに書かない。** Vaultは従来どおりObsidian Gitで書く。正本の移行は編集面（Phase 5）ができてから。

## 動かす場所

`general-console`（mooma-lpve上のVM 200、Fedora 44、4コア/16GB/128GB）に常設し、Tailscale内に閉じる。**実質「認証付き任意コード実行エンドポイント」なので公開しない。**

## Vault の索引

```
$ campd vault scan  ~/Documents/git-cloned/Obsidian-Vault   # 内訳だけ見る
$ campd vault index ~/Documents/git-cloned/Obsidian-Vault   # 索引する
$ campd vault ghosts                                        # 実体の無い触り跡
```

**Camp は Vault に一切書かない。** 索引は 4,170ファイル（markdown 4,121）・15.4MiB、走査18ms・索引2.4秒（2回目645ms）。ドット始まりのディレクトリは降りる前に切る（`.claude/worktrees/` にこの Vault の11倍のファイルがある）。

ノートの中身は `blobs` に寄せてあるので、**Vault から消えたノートも Camp からは読める**。

## ビューと MCP

```
$ campd views                 # .base の30ビューを一覧
$ campd views -n 5 'Health/テーブル'
$ campd mcp                   # MCPサーバー（stdio・読み取り専用）
```

Vault の `.base` をそのまま読む（書き戻さない）。ただし **`order:` は許可リストではなく「前に出す指定」として読み、列は既定で全部出す。**

| ビュー | Bases の解釈 | Camp の解釈 |
|---|---:|---:|
| Health/テーブル | 11列 | **107列**（定義10 + 自動97） |
| Note-Taking/All | 3列 | 13列 |

実在の7ファイルで136列中100列（73%）がどのビューからも見えなくなっていた。列は増え続けるのに、許可リストは手で書いたときのまま止まるため。

MCPに登録する（`~/.claude.json` 等）:

```json
{ "mcpServers": { "camp": {
    "command": "/home/mooma-0750/Documents/git-cloned/camp/campd",
    "args": ["mcp"],
    "env": { "CAMP_DB": "/var/lib/camp/camp.sqlite" }
} } }
```

道具は7つ、全部読み取り専用: `search_sessions` `get_session` `search_notes` `get_note` `views_list` `view` `usage`。

## リポジトリの外に置いた設定

**statusLine のフック。** プラン残量はここでしか手に入らないので、記録は `~/.claude/statusline.sh` に1ブロック足して行う（D-022）。新しいマシンで動かすときは同じものを入れる。既存の statusline がある場合は `input="$(cat)"` の直後に置く。

```bash
CAMPD="${CAMP_BIN:-/usr/local/bin/campd}"
[ -x "$CAMPD" ] || CAMPD="$HOME/Documents/git-cloned/camp/campd"
if [ -x "$CAMPD" ]; then
  printf '%s' "$input" | "$CAMPD" limits record >/dev/null 2>&1 &
  disown 2>/dev/null || true
fi
```

**`CAMP_DB` は指定しない。** M25.5 以降、DB は `camp` ユーザーのもので、
statusLine のフック（人間のユーザーで動く）からは書けない。`campd limits record` は
DB を開けないと分かると**黙って報告口へ回す**。残量が手に入るのはプロンプトの
描画時だけで、それを観測できるのは人間側だけなので、**境界を越えられるのは
この1種類だけ**にしてある。

statusLine を使っていない場合は `campd limits record` に同じ形の JSON を stdin で渡せばよい。確認は `campd limits show`。

## 権限の境界（M25.5）

Camp が世話をするエージェントは、人間のユーザーで動き、環境を自由に横断する。
**そこは縛らない。**縛るのは Camp のほう。

| | 誰のもの | エージェントから |
|---|---|---|
| `/var/lib/camp/camp.sqlite` | `camp` (0600、置き場 0700) | 読めない・書けない |
| `/usr/local/bin/campd` | `root` (0755) | 差し替えられない |
| `~/.claude/projects`・Vault | 人間のもの | **今までどおり**。camp には既定ACLで読み取りだけ |
| `/run/camp/report.sock` | `camp:campreport` (0660) | **追記だけできる** |

入れ方は `deploy/install-boundary.sh`（root で1回）。unit は `deploy/` の3本。
`campd doctor` の「DB の見え方」が、境界が入っているかを毎回言う。

**何が守られるか**: すでに書かれた監査ログを書き換えられない。DB そのものを読めない。

**何は守られないか**: エージェントは報告をやめられる（launcher は同じユーザーで動く）。
守れるのは過去の記録であって、これからの網羅性ではない。
人間は `sudo` で全部触れる——それは意図どおり。
**`campd` に NOPASSWD の sudoers 規則は置かない。** 置いた瞬間、エージェントも同じ力を得る。

### 境界の外から記録を残す

```
$ campd report -action session.start -target /home/mooma-0750/proj
記録した  id=42
```

`actor` を名乗る欄は無い。campd が `SO_PEERCRED` で呼び出し元を確かめて書く。
読み出しも更新も削除も、この口には**命令そのものが無い**。

## セッションを起こす（Phase 3）

campd は `claude` を起こせない（`camp` ユーザー・`ProtectHome=read-only`）。
起こすのは本人のユーザーで動く実行面 `campd agent` で、campd とは
`/run/camp/agent.sock` で話す（D-025）。入れ方は `deploy/install-agent.sh`、
確かめ方は `deploy/verify-agent.sh`。

```
$ campd allow add ~/Documents/git-cloned/camp   # 起こせる場所。既定は全部拒否
$ campd runtime                                  # 走っているセッション
```

- 画面の「駆動」から起こし、話し、止める。承認は待ち行列で出て、答えないと期限切れで拒否
- 許可リストの変更にはパスワードの再入力が要る（D-026）
- campd を入れ替えても、走っているセッションは引き取り直す。実行面が落ちると子も終わる（仕様）
- 終わったものは「終わったもの」に全部残る。どう終わったか（本人が止めた・放置で閉じた・
  実行面が落ちた…）、そのとき動いている途中だったか、承認を待たせたまま終わったかで絞れる（D-027）
- **守れないこと**: 同じユーザーのプロセスは実行面になりすませる。受け入れている（D-025）

外から使うときは `deploy/expose-tailscale.sh`。tailnet の中だけに出し、funnel は使わない。

## 向こうのホストで起こす（Phase 3.5）

`~/.ssh/config` の接続先に、SSH 越しでセッションを起こせる（D-028）。
向こうに常駐するものは置かない。実行面が `ssh` を起こし、向こうの小さな sh が
場所を確かめてから `claude` を子として起こす。

1. 「接続先の台帳」で `~/.ssh/config` を読み直し、接続先を**許す**（パスワードが要る）。
   許したときの行き先（`ssh -G`）と、**known_hosts が信じるホスト鍵**が固定される。
   あとで config や known_hosts が別の先・別の鍵を指すようになったら起こさない。
   known_hosts に鍵の無い先は許せない
2. 「許可した場所」で、そのホストの場所を足す（CLI なら `campd allow add -host <alias> <dir>`）
3. 向こうの `claude` が非対話の PATH に無ければ、台帳で場所を書く
   （空なら `PATH`・`~/.local/bin`・Homebrew を探す）
4. 「走っているもの」で起こす先を選んで起こす

- **ホスト鍵は Camp では受け入れない。** 初めての先は端末で一度 `ssh <alias>` して確かめる
- 接続が切れても向こうの子は終わるとは限らない（工具を走らせている最中は残る）。
  実行面が向こうへ入り直して孫まで止める。繋がらなければ孤児のまま残し、繋がり次第止める
- 向こうには sh と互換のシェルと `/proc` が要る（Linux・Termux）。**Windows・macOS では起こさない**
  ——切れたあとに向こうを確かめる手段が無いので
- 向こうの会話記録は取り込まない。画面に出るのは実行面が受け取ったフレーム
- **campd は向こうのプロセスを確かめられない**（鍵を持つのは実行面だけ）。向こうの身元は実行面の報告として残る

## Codex で起こす（Phase 3.6）

「走っているもの」で起こすときに **Claude Code / Codex** を選べる（D-029）。
Codex は `codex app-server`（stdio の JSON-RPC）で駆動し、対話・承認・中断・停止・残量は
Claude Code と同じ画面・同じ許可リスト・同じ監査ログで扱う。いまはこのマシンだけ。

- **Camp は Codex を専用の置き場（`~/.local/share/camp/codex`）で起こす。** 起こすたびに
  `~/.codex/config.toml` から設定を作り直し、MCP・プラグイン・モデルは同じにする。
  **対話で「今後訊かない」にしたルール（`~/.codex/rules`）と信頼済みの場所は持ち込まない**
  ——持ち込むと、それらは承認を訊かずに、sandbox の外で走る（実測）。ログインは
  `auth.json` の symlink で借りる。`~/.codex` には書かない
- 話し始める前に、Codex で効いた方針（承認の方針・承認を見る者・sandbox・作業場所）を
  照らす。違えば起こさない
- 承認は「許す／断る」だけ。**MCP のツールは Codex の作りとして承認を訊いてこない**
- **Codex の「中断」は、走っていたコマンドを止めない。** 確実に止めるのは「止める」
- 実行面は `codex` が見つかり、scope で包めるときだけ Codex を起こせると名乗る
  （`campd agent -codex <path> -codex-home <dir>`）
- Codex の会話記録は取り込まない。画面に出るのは実行面が受け取ったフレーム

## 退避先（バックアップ）

**稼働中のDBは平文のまま。** SQLCipher は systemd 常駐と相性が悪く、再起動のたびに
手で解錠することになる。守れるのは持ち出す先だけなので、そこは確実に守る。

```bash
# 取る。鍵は 1Password から渡す。campd は鍵を持たない
op read "op://Private/camp backup/password" |   ./campd snapshot -out ~/backups/camp-$(date +%Y%m%d).snapshot

# 戻す。戻したあと doctor まで通って初めて成功
op read "op://Private/camp backup/password" |   ./campd snapshot -restore ~/backups/camp-20260903.snapshot -out /tmp/restored.sqlite
```

- `VACUUM INTO` で一貫したスナップショットを取る。稼働中でも WAL ごと辻褄が合う
  （ファイルをコピーする方式は `-wal` と `-shm` を取りこぼす）
- AES-256-GCM。鍵は PBKDF2-HMAC-SHA256 を 600,000 回。4 MiB ずつに区切って
  暗号化するので、274 MB でもメモリに載せない
- **後ろを切り落としたファイルは復元を拒否する。** 静かに欠けたバックアップは、
  取れていないバックアップより悪い
- 既にあるファイルは上書きしない。復元に失敗したら中途半端なファイルを残さない
- 取ったときに出る `sha256` を控えておく。**戻したときに同じ値が出れば中身は同じ**

実測（2026-09-03、274 MB）: 取るのに 4.4 秒、戻すのに 4.8 秒、暗号文の増分は 1,089 バイト。
実際に稼働中のDBを消して戻し、`doctor` が全項目通ることを確かめてある。

## 制約

- **Claude Proサブスクの範囲内に収める。** APIクレジットは使わない。これがClaude Agent SDKを採れない理由（Agent SDKはAPIキー前提）
- セッション駆動は `claude` CLI のヘッドレス双方向JSONモードで行う
- **バックエンドはGo、フロントはVite + React SPA**（`docs/40-layout.md`）。Next.jsは使わない
- 履歴は生のまま保存する。**パターン検出によるリダクトはしない**（偽陽性100%・偽陰性100%だった。`docs/10-decisions.md` D-010）。ただし2026-09-03から、**索引に入っていない部分**（誰も復号できない `thinking` 署名、二重に入っている画像）だけは `campd retain` で落とせる。消したぶんは `tombstones` に残る
- Tailscale内に閉じたうえで**アプリ認証を最初から入れる**（D-011）
- プラン上限はいずれ `claude -p` の制御プロトコルから取る（`rate_limit_event` / `get_usage`。トークン消費ゼロ）。ただし**記録だけは statusLine のフックで先に始めている**——この値は捨てられたら遡れない唯一の素材（D-022）
- 権限確認UIは `--permission-prompt-tool stdio` で作れる（`--help` に出ない隠しフラグだが実在する）

詳細は `docs/` を参照。
