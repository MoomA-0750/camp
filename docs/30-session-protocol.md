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
