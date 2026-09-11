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

- **既定のパーク期限は5分**（`CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`）。UIが応答しないとターンが落ちる
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
  ログインシェルが何かを吐いても、それより前の行はフレームとして扱わない
