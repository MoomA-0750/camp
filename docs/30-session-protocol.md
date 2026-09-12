# セッション駆動プロトコル

`claude` CLI v2.1.252 を子プロセスとして双方向 stream-json で駆動する際のリファレンス。2026-09-01にサブエージェントが実測し、親セッションが独立に再検証した。

```bash
claude -p --input-format stream-json --output-format stream-json \
       --verbose --include-partial-messages \
       --permission-prompt-tool stdio \
       [--resume <sessionId>] [--fork-session]
```

---

## 1. `--permission-prompt-tool` は存在する（ヘルプに出ないだけ）

**当初「対話的な権限確認はできない」と結論したのは誤りだった。** このフラグは `.hideHelp()` で登録されているため `--help` に出ない。

★親セッションによる再検証:

```
$ claude --permission-prompt-tool
error: option '--permission-prompt-tool <tool>' argument missing   ← 受理されている

$ claude --this-flag-does-not-exist
error: unknown option '--this-flag-does-not-exist'                 ← 比較対象
```

バイナリ内に `can_use_tool` `control_request` `control_response` `rate_limit_event` `get_usage` `get_context_usage` `set_permission_mode` `set_model` `mcp_status` `interrupt` `permission_denied` `hook_callback` `file_suggestions` のシンボルがすべて存在し、`hideHelp` は7箇所で使われている。

リテラル `stdio` を渡すと全ての承認が stdin/stdout 経由でホストに回り、**ホストが答えるまでターンがブロックされる**。これは Agent SDK の `canUseTool` と同じ経路。

実測されたフレーム:

```json
{"type":"control_request","request_id":"b604ab96-…","request":{
  "subtype":"can_use_tool",
  "tool_name":"Write","display_name":"Write",
  "input":{"file_path":"/tmp/camp-test/hello.txt","content":"hi"},
  "description":"hello.txt",
  "permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],
  "tool_use_id":"toolu_01K6hD5seTGiH5Y41BVUjyRB"}}
```

ホストの返答は `{"behavior":"allow","updatedInput":{…}}` または `{"behavior":"deny","message":"…"}`。allow 後に実際にファイルが作られることを確認済み。

**→ Campは承認UIを作れる。** `--permission-mode` のプリセットに逃げる必要はない。

注意点:

- **この経路の承認に期限は無い**（2026-09-12 実測。`dev/scripts/park_probe.py` に相当する測り方で、Camp と
  同じ起こし方のまま `can_use_tool` を10分放置した——`claude` はフレームを1つも出さずに待ち続け、
  コマンドも走らなかった）。**Phase 3 で「5分で子が諦める」と読んだのは誤り**: 実体の説明によれば
  `CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`（既定5分・`never` で無効）は**遠くの相手へ回した**ダイアログと
  抱えたセッション間メッセージの期限で、「local-only の承認は影響を受けない」と書いてある。対話の CLI も
  承認に期限は無い（公式の説明では、放置で自動的に解けるのは `AskUserQuestion` の選択肢だけ）
- **フラグを付けない場合、CLIは単独で自動拒否する**（`{"type":"system","subtype":"permission_denied",…}` だけが出る）
- **読み取り専用コマンドは安全コマンドのfastpathを通り、承認なしで実行される**（`Bash(id -un)` が承認要求なしで走った）。**どの呼び出しが承認を出すかをCampが予測することはできない**ので、UIは「来たら出す」設計にする

---

## 2. プラン上限は制御プロトコルで取れる。statusLineは不要

**D-005を訂正する。** statusLineは「唯一の現実的な公式ルート」ではなかった。

- `rate_limit_event` が**要求なしにストリームへ届く**（`unifiedWindows.five_hour` / `seven_day` → `utilization` + リセット時刻）
- `get_usage` 制御リクエストは statusLine より**豊富**なデータを返す: `utilization`、ISO形式の `resets_at`、`severity` バンド、そして拘束中の窓を示す `is_active`
- `get_context_usage` はカテゴリ別のコンテキスト内訳を返す
- **これらの制御フレームはモデル呼び出しを一切発生させない**（トークン消費ゼロ）

★再検証: バイナリ内に `unifiedWindows` `utilization` `five_hour` `seven_day` を確認済み。

**→ 仕様が求めていた4つの可視化すべてが、TUIなしで取れる。** statusLineフックは不要になった（補助として残すのは自由）。

なお Codex は同じデータを会話記録に直接埋め込んでいる（`docs/20-data-model.md` B5参照）ので、**どちらのエージェントも statusLine を必要としない。**

---

## 3. 双方向入力

1行1JSONで stdin に書く:

```json
{"type":"user","message":{"role":"user","content":"…"}}
```

確認された挙動:

- `result` の後もプロセスは**アイドルで生存し続ける**。stdin を閉じると exit 0
- 1プロセスで2ターン通り、コンテキストは保持される
- **`system/init` はプロセスごとではなく*ターン*ごとに出る。** これを起点にセッションを作ると二重計上する

---

## 4. 中断

- `{"subtype":"interrupt"}` → `{"still_queued":[]}` の受領応答 → `terminal_reason:"aborted_streaming"`
- **SIGTERM は exit 143**。ターンは失われ result は出ない
- アイドル時の SIGINT は exit 0（ターン中の経路は未検証）

**正しい停止順序: interrupt → result を待つ → stdin を閉じる → シグナル。**

---

## 5. セッションの再開と分岐

- **ディレクトリを跨いだ ID 指定の resume は動く。** 同じIDで、**元のプロジェクトディレクトリのファイルに追記**される
- **`--fork-session` は新しいIDで、親のディレクトリに新しいファイルを作り、履歴を全部複製する**（63KB vs 55KB）。親は無傷
- → **fork は必ずメッセージの `uuid` で重複排除すること**

**2026-09-13 に測り直して分かったこと**（M48。実装したのは resume だけ。詳しくは 14 節）:
複製された行は `uuid` が元と同じだが、**全行に `isSidechain` が付く**。Camp の取り込みは
`messages` をファイル＋位置で一意にしているので複製は全行入り、しかも `isSidechain` を見て
**丸ごとサブエージェント扱い**にする。記録に「fork された」と分かる印は無く、本物のサブ
エージェントと見分けられない。**そのため Camp は fork を入れていない。**

---

## 6. 認証

Proサブスクで動くことの確認が3つ取れた:

- `ANTHROPIC_API_KEY` は未設定
- `system/init` の `"apiKeySource":"none"`
- `initialize` の `"subscriptionType":"Claude Pro"`, `"apiProvider":"firstParty"`

---

## 7. 並行実行

3プロセス同時実行で、PIDは別々、全部 exit 0。**`~/.claude` 配下にロックファイルは存在しない。** プロセスごとのソケットが `/run/user/1000/cc-socks/` に作られる。

ただし**同時「ターン」がレート枠をどう消費するかは未検証**（プローブは意図的にトークンを使っていない）。

---

## 8. その他

対称的な制御プロトコルが約20サブタイプある。実行中に `set_permission_mode` / `set_model` を変更でき、`mcp_status`・`file_suggestions`・`get_settings` も取れる。

## 未検証の項目

- 実際にレート制限に達した時の挙動（測定中アカウントは56%だった）
- 同時ターンがレート枠を消費する量
- ターン中の SIGINT
- `hook_callback` の往復
- `can_use_tool` の任意フィールド、`request_user_dialog`、`initialize` のオプション一覧は**バイナリに埋め込まれた Zod スキーマから読んだもので、実行して確かめてはいない**

---

## 9. 2.1.260 での再確認（2026-09-04）

上は 2.1.252 の実測。Phase 3 の着手前に、いま入っている版で同じことが言えるかを
測り直した（`dev/scripts/probe_session.py`、記録は `dev/active/phase3-baseline.md`）。

**変わっていなかった。** `--permission-prompt-tool stdio` も `get_usage` も
`get_context_usage` も `rate_limit_event` も、`system/init` がターンごとに出ることも同じ。

新しく分かったことが2つある。

**(a) `rate_limit_info` は上に書いたより広い。** 実測で
`status` / `overageStatus` / `overageDisabledReason` / `isUsingOverage` が付いていた。
超過枠を使っているかどうかが分かるので、残量表示に使える。

**(b) 承認は1ターンに何度でも来る。**

これは実際に踏んで気づいた。M26 の e2e テストを「承認が1つ来たら答えて、あとは
result を待つ」形で書いたら、`can_use_tool` が2つ来て、2つ目に誰も答えないまま
子が待ち続け、2分の待ちが空振りした。

> **UI は待ち行列として作る。** 「いま出ている承認」を1つだけ持つ形にすると、
> 同じ止まり方をする。M28 の受け入れ条件に入れた。

頻度そのものは低い。3回の工具呼び出しのうち承認が要ったのは1回で、
`Bash(id -un)` と `Read` は読み取り専用 fastpath を通った。**どれが来るかは
予測できない**ので、設計は変わらず「来たら出す」。

---

## 10. SSH 越しに駆動する（2026-09-11）

上のプロトコルは SSH の上でもそのまま通る（stdin/stdout を ssh が運ぶだけ）。
変わるのはプロセスの見え方で、`ssh localhost` と OpenSSH 10.2 で次を測った
（設計は `dev/active/phase3.5-plan.md`、決定は D-028）。

| 測ったこと | 結果 |
|---|---|
| 遠隔コマンドの pid / pgid / sid | 同じ。**sshd は遠隔コマンドごとに setsid する** |
| 向こうの子がシグナルで死んだときの ssh の出口 | **255**。接続の失敗と同じ番号 |
| 向こうの子が `exit 7` / `exit 255` | 7 / 255（そのまま運ばれる） |
| 手元の ssh を SIGKILL | stdin を読んでいない向こうの子は**生き残る**。読んでいる子は終わる |
| 手元から stdin を閉じる | 子は終わる。裏で起こした**孫は生き残る** |
| `systemd-run --user --scope` を ssh の中で | pid を変えずに成り代わる。**引数の `$$` を `$` に展開する**（systemd 259） |

したがって:

- **255 だけで「接続が切れた」と決めない。** stderr（`closed by remote host` など）と、
  Camp 自身が止めに入ったかどうかで見分ける
- **手元の ssh が終わっても、向こうが終わったとは限らない。** 工具を走らせている
  最中の claude は stdin を読んでいないので残る。向こうへ別の ssh で入り、
  起動時刻を照らしてから、同じセッション番号のもの（と親を辿れるもの）を止める
- **向こうの sh は `claude` に成り代わらず、親として残る。** 子が終わったら同じセッションの
  残りを止めてから抜ける。残りが stdout を握っていると sshd はチャネルを閉じず、
  手元の ssh も終わらない
- 1行目に向こうの sh が身元（pid・起動時刻・boot_id・scope・実パス）を名乗る。
  ログインシェルが何かを吐いても、それより前の行はフレームとして扱わない（64 行まで。超えても名乗らなければ
  諦めて手元の ssh を止める。このとき向こうの身元を知らないので reapScript は入れず、向こうの sh が子の終わりで
  残りを止めるのに任せる）
- **（M42、2026-09-12）向こうの sh はエージェントの名前を知らない。** 探す名前・置き場の環境変数名と
  既定の相対パス・引数（駆動器の Argv）を実行面が渡す。名乗りは版 3 で、9欄のあとに `key=value`
  （いまは `home`・`homereal`、置き場の直す前と実パス）を足せる。Codex の置き場はこれと文字の上で照らす
- **（M42）しるし**: エージェントを `CAMP_SESSION=<セッション id>` の環境で起こす。Codex 自身が SIGKILL で
  死ぬと、Codex が別のセッションで起こしたコマンドは ppid 1 で残り、親を辿れない（`rp` で実測。
  `dev/scripts/probe_codex_remote.py execkill`）。**モデルのターンで起こしたコマンドにもしるしは届く**
  （`rp` で実測、2026-09-12。`probe_codex_remote.py tree`。`sleep` は自分のセッションで codex.bin の子として
  走り、app-server に渡した環境変数を持っていた）。sh の終わりと reapScript は、しるしを持つプロセスも止める:
  `/proc/<pid>/environ` を読めるものは**同じ uid かどうかも見て**（root で入ったときに別のユーザーまで
  拾わないため）、読めなかった同じユーザーのプロセスは数だけ報告に出す。TERM のあとにもう一度しるしを
  探し（止めている間に生まれた子を拾う）、KILL のあとに数え直して、残っていればその数も報告に出す。
  **残る穴**: 走査から撃つまでの間にしるし持ちが終わり、同じ pid が別人に渡ると、その別人へ TERM が飛ぶ
  （窓は数秒。pid_max の小さいホストでのみ現実的）。しるしは台帳のセッション id なので、campd の
  再起動後の孤児の始末でも渡る

---

## 11. Codex を駆動する（2026-09-11、codex-cli 0.154.0）

口は `codex app-server`（stdio の JSON-RPC、1行1メッセージ）。`codex exec --json` は承認を
返せない（非対話）ので使わない。測り方は `dev/scripts/probe_codex.py`・`probe_codex_config.py`、
設計は `dev/active/phase3.6-plan.md`・`phase3.7-plan.md`、決定は D-029（D-030 で一部覆した）。

```
→ initialize {clientInfo, capabilities:{optOutNotificationMethods:[…]}}   ← {userAgent, codexHome, …}
→ initialized
→ thread/start {cwd, …}（「CLI と同じ」では方針を渡さない。ほかの度合いでは approvalPolicy 等を渡す（§12）。Phase 3.6 では毎回渡していた）
                                                   ← {thread:{id}, approvalPolicy, approvalsReviewer, sandbox, cwd, …}
→ turn/start {threadId, input:[{type:"text", text}]}   ← {turn:{id}}、以後 turn/started … turn/completed
← item/commandExecution/requestApproval {id:0, command, cwd, …}   → {id:0, result:{decision:"accept"|"decline"}}
← item/fileChange/requestApproval {id:1, itemId}                   （差分は直前の item/started の changes）
→ turn/interrupt {threadId, turnId}                ← turn/completed {status:"interrupted"}
→ account/rateLimits/read                          ← {rateLimits:{primary, secondary, planType}}（モデルを呼ばない）
```

| 測ったこと | 結果 |
|---|---|
| 承認 | サーバーからの**要求**として来る。1ターンに2つ来た。id は整数の 0 から |
| 断る（`decline`） | そのコマンドだけ `declined` になり、ターンは続く。理由を渡す欄は無い |
| ファイル変更の承認 | 要求そのものに差分が無い。直前の `item/started`（fileChange）の `changes` にある |
| `untrusted` + `workspace-write` | `touch`・`rm`・作業場所の中のファイル作成も訊いてきた |
| 中断 | ターンは `interrupted` で終わる。**走っていたコマンドは残る**（15 秒後も生きていた） |
| stdin を閉じる／SIGTERM | 0.06 秒で終わる。走っていたコマンドも 1 秒後には居ない |
| 子 | app-server の子（MainThread・node_repl・codex-code-mode）は**別々のプロセスグループ**。コマンドは `codex-linux-sandbox` の下で**別のセッション** |
| 途中経過 | `optOutNotificationMethods` で止まる（`mcpServer/startupStatus/updated` 10 → 0） |
| `thread/start` の応答 | 効いた方針が返る。本人の `config.toml` より引数が勝った |

**本人の Codex の設定を引き継ぐと、承認を通らずに走る経路がある**（Fable の設計レビューで指摘され、測った）:

| 測ったこと | 結果 |
|---|---|
| 本人の `rules/default.rules` に allow のあるコマンド | 承認要求なしで走った。`curl` は `networkAccess:false` なのに外へ出た（sandbox の外で走る） |
| MCP のツール | 承認要求なしで走った。**Codex には MCP の承認の要求そのものが無い** |
| `thread/start`（`workspace-write`） | 本人の `config.toml` に `[projects."<cwd>"] trust_level="trusted"` を書き足した（`read-only` では書かない） |

Phase 3.6 ではこれを受けて **専用の `CODEX_HOME`** で起こしていた（設定を本人の `config.toml` から
作り直し、rules と信頼済みの場所は持ち込まない）。**Phase 3.7 で覆した**（D-030、2026-09-12）:
Camp はこのマシンの AI 作業環境の忠実なリモコンなので、Codex は**本人の `~/.codex` のまま**起こし、
上の「承認を通らずに走る経路」も CLI と同じ振る舞いとして受け入れる（専用の置き場ではプラグイン・
スキル・computer use が使えなかった）。話し始める前に照らすのは、本人の置き場で起きたか
（initialize の `codexHome` を実パスで）・作業場所・スレッド id だけ。承認のコマンドが起こした場所の
外なら、断らずに印を付けて見せる。設定が途中で変わっても（`thread/settings/updated`）止めずに記録する。

**承認に期限は無い**（2026-09-12 実測、`dev/scripts/park_probe.py codex`）: `untrusted` で
`item/commandExecution/requestApproval` が来たあと、答えないまま10分放置しても Codex は何も出さず、
コマンドも走らなかった。**Claude Code も同じ**（同じ probe の claude。§1 も見よ）。それを受けて
**Camp も期限切れにしない**（本人の決定。以前は 4分30秒で自分から断っていた。`Supervisor.ParkAfter`
が 0 なら見ない＝既定）。

**確かめていない**: 権限の拡張・質問・MCP の問い合わせに断りを返したときの振る舞い（偽物でしか
試せていない）。画面から答えられるようにするのは**まだ作っていない**——Phase 3.8 で作る予定だったが、
3.8 は記録の対等（M44〜M47）で終えた。どのフェーズで作るかは未定。

## 12. 駆動器（Phase 3.7、2026-09-12）

エージェントの違いは駆動器（`internal/session/driver.go` の `Driver` と、子1本との会話の
`Conversation`）に閉じ込めた（D-031）。実行面の本流・campd・API・画面は、ここに書いた形しか知らない。
設計は `dev/active/phase3.7-design.md`。

**実行面 ↔ campd**:

| 口 | 欄 |
|---|---|
| hello | `agents`（起こせる名前の配列。古い campd のために残す）と `drivers`（駆動器の説明: `name`・`label`・`perms`・`notes`・`interrupt_leaves_tools`・`remote`）。**名乗らない古い実行面**の分は campd が手元の駆動器から補い、確認の度合いは `cli` だけとみなす |
| hello | `build`（実行ファイルの指紋。Go が埋める VCS の版から作る）。campd は自分のものと照らし、違えば画面に出す。**止めはしない。** 版の文字列（`version`）では足りない——campd と実行面は同じバイナリで、どちらも同じ文字列を名乗る。**名乗らない実行面は古いとみなす**（この仕組みより前のバイナリだから）。自分の指紋が取れないとき（VCS の外・試験）は何も言わない |
| start | `agent`・`perm`（空は `cli`）。実行面は名乗っていない度合いなら起こさない |
| start | `resume`（エージェント自身のセッション id。空なら新しく起こす。M48、2026-09-13）。**名乗らない実行面へは頼まない**——古い実行面はこの欄を読まないので、頼んでも黙って落ち、続きのつもりで新しい会話が始まる。駆動器の説明の `resume` で見る |
| started | 実行面が起こした `agent`・`perm` を名乗る。campd が頼んだものと照らし、違えば止める |
| frame | 駆動器が畳んだ意味（`turn_end`・`ask`・`interrupted`・`note`・`withdrawn`）。campd は種類の文字列を読み分けない |

中断でターンが終わっても工具が残るか（Codex）・向こうのホストでも起こせるか（Claude）は、
エージェントの名前でなく駆動器の説明で決める。残量の問い合わせは全エージェントで同じ
`get_usage`・`get_context_usage` だけ（`mcp_status` は外した）。

**API（画面はこれだけを見る）**:

| 口 | 欄 |
|---|---|
| `GET /api/runtime` | `agents`（いま繋がっている実行面が起こせるものの説明） |
| 行 | `agent_label`（表示名）・`agent_session_id`（エージェント自身のセッション id。`claude_id` は同じ値で当面残す） |
| `GET /api/runtime/{id}` | `agent_notes`（固有の振る舞いの説明） |
| `/log`・`/stream` の行 | `summary`（駆動器が畳んだ一言）・`own`（Camp 自身の問い合わせのやりとり。画面が畳む） |
| `/usage` | `view`（プラン・枠・コンテキスト・内訳の表・答えの中の失敗）。生の答えも並べる |
| 承認の `detail` | `view`（`what`: command / file / tool、コマンド・場所・`outside`、変更は `{path, kind, patch}`、理由）。元の要求も並べる |
| `POST /api/runtime/{id}/resume` | 終わったセッションの続きから起こす（M48）。**中身は渡さない**——場所・エージェント・度合いは元の行から引き継ぐ。返るのは新しく起きた行 |
| 行 | `resumed_from`（どの行の続きか）・`resumed_by`（この行の続きとして起きた行）。画面は「元のものが生き返った」ように見せる |

エージェントを足すのは、`drivers` の1行とその駆動器のファイル（`Info`・`Launch`・`Argv`・`Open`・
`Usage`・`Summary` と `Conversation`）だけ。テストの中の3つ目のエージェント（`fake3_test.go`）が手本。
**まだ残っているエージェントの名前**（codex exec のレビュー、2026-09-12）: 実行面の設定の口（`campd agent` の
`-claude`・`-codex`・`-codex-home` と `Agent` の欄、systemd の `CAMP_CLAUDE_BIN`）、古い実行面との互換
（名乗らなければ Claude）、移行 0025・0026 の SQL、会話記録の取り込みと「残量」の画面（Claude だけ）。

**確認の度合い（M41）**: start の `perm`（`cli`・`ask`・`edits`・`auto`・`full`）を、駆動器が渡し方に直す
（Claude は `--permission-mode`、Codex は thread/start の欄。対応は README の「確認の度合い」）。
**照らすのは渡したものだけ、起こしたときに1回だけ**: Codex は thread/start の応答（違えば話し始めない）、
Claude は最初の system/init の `permissionMode`（最初の入力のあとにしか来ない。名乗らないのも違うと読む。
**違っていても最初のターンは始まっていて、止めるまでの間に道具が動きうる**。system/init そのものが来なければ照らせない）。
Claude で違えば、実行面が frame の `halt` で伝え、campd が孫まで止めて `start_failed` と書く。2回目以降の
変化は `note`（止めない）。台帳は `runtime_sessions.perm`（移行 0025）。Phase 3.6 の Codex の行は `legacy`
（表示だけ、頼めない）。向こうのホストでも同じ5つを渡す（M42 から。引数・thread/start の欄は駆動器が向こうへ渡す）。

## 13. 向こうのホストの記録を読む（Phase 3.8 の M47、2026-09-12）

campd は ssh しない（D-025）。**記録を読むのも実行面を通す。** 向こうには常駐させず、小さな sh が
1回走って終わる（D-028）。**読むだけで、向こうへは書かない。**

| 口 | 頼むこと | 返るもの |
|---|---|---|
| `rec_list` | 接続先（`remote`。固定つき）・置き場の決め方（`rec_home_env`・`rec_home_default`・`rec_sub`）・前回位置（`rec_at`）・窓の大きさ（`rec_win`） | 向こうが解決した置き場（`rec_root`）・一覧（`rec_files`: パス・大きさ・更新時刻）・前回位置の直前の窓（`rec_window`） |
| `rec_read` | 範囲の束（`rec_want`: パス・位置・最大バイト数）・1回の合計の蓋（`rec_cap`） | 中身（`rec_data`）・蓋で切ったか（`rec_more`） |

- **パスを運ばない。** 置き場は向こうの sh が `$HOME` と環境変数から解決して名乗る（駆動器の
  `RemoteLaunch` の `HomeEnv`／`HomeDefault` と、取り込み器の `RecordSub`）。campd は向こうの `$HOME` を
  知らないし、自由なパスを打ち込む口も作らない（本人の決定 2026-09-12）
- 向こうの sh は `wrapperScript` と同じ約束——1行目に `CAMP-REC` か `CAMP-ERR`、`cd -- && pwd -P` で
  実パスを照らす、改行やタブのあるパスを弾く、**`*.jsonl` だけ・置き場の下だけ・通常ファイルだけ・
  symlink は辿らない**。大きさと更新時刻は `find -printf`（GNU）→ `stat -c`（GNU）→ `stat -f`（BSD・macOS）
  の順に試し、**どれも無ければ名乗る前に断る**
- **名乗り（`CAMP-REC`）より先に道具を見極め、最後に `END` を出す。** 名乗ってから失敗すると、
  受け手は1行目だけ見て「読めた」と判断し、中身が来ないのを「0件」と読む（2026-09-12、macOS で
  実際にそうなった）。campd は `END` が無ければ「途中で切れた」として失敗にする
- **再開点のハッシュは向こうで計算させない。** `sha256sum` があるとは限らず、あっても手元と同じ
  取り方だと保証できない。窓（256 バイト）の中身そのものを運び、手元で同じ関数にかける
- **繋ぐ前に行き先を照らす**（`ssh -G` と固定）。起こすときと同じ約束で、掃除（`reap`）も同じにした
- 読んでよい組み合わせは台帳 `ssh_record_roots`（`host`・`agent`・`enabled`。移行 0028）。
  **行が無ければ読まない。** パスは持たず、最後に読めた時刻・最後の失敗・続けて失敗した回数だけ持つ
- 読む契機は3つ: 押したとき（`POST /api/ssh/{alias}/record/read`）、そのホストのセッションが終わった
  直後（**回線が切れて終わったときは行かない**）、定期（`campd serve -record-every`。既定1時間、`0` でやめる）

## 14. 終わったセッションの続きから起こす（M48、2026-09-13）

**先に本物で測った**（`dev/scripts/probe_claude_resume.py`・`probe_codex_resume.py`）。

| | 頼み方 | 文脈 | エージェント側の id | 記録 |
|---|---|---|---|---|
| Claude Code | `-p --resume <session-id>`（Camp と同じ引数のまま足す） | **続いた** | **変わらない** | 同じ JSONL へ追記 |
| Codex | `thread/resume {threadId}` | **続いた** | **変わらない** | 同じ rollout へ追記 |

id も記録ファイルも変わらないので、**取り込みは何も変えなくてよい**（差分読みがそのまま効き、
会話は1本のまま繋がる）。

**非対称は駆動器に閉じた**（D-031）: Claude は**引数**で続ける（`Argv(perm, resume)`）、Codex は
**呼び出し**で続ける（`Open`/`Begin` が `thread/resume` を投げる）。campd と画面はどちらも知らない。

**台帳は新しい行を作る**（本人の決定 2026-09-13。移行 0029 の `resumed_from`）。終わった行を
生き返らせて使い回すと、どう終わったか・いつ・待たせたまま終わった承認が上書きされ、過去が消える。
プロセスとの 1 対 1（pid・起動時刻・boot_id・scope で所有権を見る）も崩れる。
**画面では「元のものが生き返った」ように見せる**——終わった一覧に「続きから」、詳細に
「前のセッションの続き」「続きが起きている」を出す。

**続けないもの**: まだ走っている行（同じ会話を2つ開くと、Claude は「別の端末で走っている」として
拒み、Codex は読み込み済みのスレッドへの上書きを無視する）、エージェント側の id を名乗らないまま
終わった行、Phase 3.6 の Codex の行（`legacy`。専用の置き場で起きていた）。

**確認の度合いも続きで頼む。** 元が「毎回訊く」だったのに続きで本人の設定へ戻ると、安全側でない
驚きになる。効いたかは起こしたときに1回照らす（既存の仕組みのまま）。

**続けたスレッドが頼んだものかを照らす**（Codex）。違う id が返ったら話し始めない——別の会話の
続きを本人に見せることになる。

**`--fork-session`（複製へ分岐）は入れていない。** 実測（2026-09-13）: fork は過去のやり取りごと
複製した別ファイルを作り、**その全行に `isSidechain` が付く**。取り込みはそれを見てサブエージェント
として入れるので、fork した会話が丸ごとサブエージェント扱いになり、同じ会話が2本になる
（`messages` の一意制約はファイル＋位置なので、複製は別物として全行入る）。記録には「fork された」と
分かる印が無く、**本物のサブエージェントと見分けられない**。端末で本人が `--fork-session` した分は
Camp からは永久に見分けられないので、直し切れない。Codex にも対称な `thread/fork` があるので、
やるなら後から両方同時に足せる。
