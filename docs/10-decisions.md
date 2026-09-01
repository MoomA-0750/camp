# 意思決定ログ

新しい決定は上に追記する。覆した決定は消さず「覆した」と書き残す。

---

## D-015: 索引に入れるのは `message.content` のブロックだけ。`toolUseResult` は入れない（2026-09-02）

M6 でブロックを切り出すときに、同じ中身が2箇所にあることが分かった。`message.content` の `tool_result` ブロックと、行の最上位の `toolUseResult` である。実測で content 68MB 対 toolUseResult 57MB、大きい行では**同じ文字列がほぼそのまま両方に入っている**（例: 27,054文字 対 25,893文字）。

両方索引すると索引が倍になり、同じヒットが2回出る。`message.content` 側だけ入れる。構造化された差分（`structuredPatch` 等）が要るようになったら `raw_json` から取れる。

### 索引して分かったコーパスの実像

| | |
|---|---|
| 159MB のうち **93MB が base64 の画像** | スクリーンショット。索引しても引けないので飛ばす |
| `thinking` ブロック2,977個のうち、本文があるのは **14個だけ** | 残りは `"thinking":""` と暗号化された `signature` のみ。**思考は検索できない**（CLIが平文で残していない） |
| 索引対象になった実テキストは **12MB / 16,535ブロック** | 内訳は tool_use 7,222・tool_result 7,095・text 2,021・attachment 183・thinking 14 |

「ブロックが作られなかったメッセージ」が3,148件あって最初はバグを疑ったが、内訳は**画像だけの tool_result が105件、本文が空の thinking が2,992件**で、いずれも索引すべきものが無い行だった。大きな `tool_result` を1件選んで、抽出した文字数と `json_extract` の文字数が**27,054 対 27,054 で一致**することを確かめてある。

---

## D-014: 派生テーブルの作り直しはディスクではなく `messages` から行う（2026-09-02）

M5 で `usage` を足したとき、すでに取り込み済みのファイルは `ingested_offset` が終端にあるので新しい派生テーブルが埋まらない、という当たり前の問題にぶつかった。素直な解決は「DBを消して `~/.claude/projects` から取り込み直す」だが、**これは Camp の存在理由そのものを壊す**。Camp は CLI 側から消えた記録を残すために作っている。ディスクを正として作り直す運用にすると、派生テーブルを足すたびに「消えたぶんを失う」事故が起きる。

`raw_json` を無加工で持っている（D-010）ので、派生はいつでもDBの中だけで作り直せる。`campd backfill` をその入り口にした。

**検証:** backfill で作った `usage` と、空のDBに丸ごと取り込み直して作った `usage` を突き合わせた。共通する7,119行が `output_tokens` / `cache_read_input_tokens` / `input_tokens` / `iterations` / `day` / `model` / `run_id` / `project_id` のすべてで一致。差は、あとから走らせた側が1メッセージぶん新しい記録を拾ったことだけだった。

---

## D-013: `usage` は「最初の1行を採る」ではなく列ごとの `max` を採る。`day` はローカル日付（2026-09-02）

**受け入れ条件に書いた `ON CONFLICT DO NOTHING` は間違いだった。**

同じ `api_message_id` が複数行に現れる理由は2つあって、扱いが違う。

| 理由 | 現れ方 | 正しい扱い |
|---|---|---|
| ストリーミングの途中経過 | 同じファイルの中に、出力が伸びていく途中の行が並ぶ | 最後（＝最大）が確定値 |
| fork / resume の複製 | 別ファイルに、まったく同じ数値でもう一度現れる | どれを採っても同じ |

実測（2026-09-02）: `api_message_id` 7,101個のうち **48個で `output_tokens` が行ごとに違う**。`input_tokens` / `cache_read_input_tokens` / `cache_creation_input_tokens` が食い違う id は**ゼロ**。つまり伸びるのは出力側だけ。時刻順に並べたとき出力が減る箇所も**ゼロ**（単調増加）。

最初の1行を採ると出力が **4,630,878**、`max` を採ると **4,667,284**。36,406トークンの過少計上になる。

**採用:** 列ごとに `max` を取る upsert。`max` は順序に依らず冪等なので、途中経過と確定値が別々の取り込みパスに分かれても結果が変わらない（差分追尾では実際に分かれる）。

**`<synthetic>` は `usage` に入れない。** CLIがローカルで作る擬似アシスタント行で、`usage` は常にゼロ、課金も発生していない。入れておいて集計のたびに除外するより、最初から入れないほうが「除外を忘れる」経路が消える。`messages` には残す（実測16行）。

**`day` はUTCではなくホストのローカル日付にする。** JSTだと15:00Z以降は翌日で、UTCで切ると「今日どれだけ使ったか」が毎日9時間ぶんずれる。プラン上限の話をしたいのだから、区切りは人間の1日に揃える。

**`usage.iterations` は整数ではなかった。** `usage` と同じ形のオブジェクトの配列で、実コーパスでは長さが必ず1、その唯一の要素は上位の `usage` と完全に一致する（11,468行すべてで `output_tokens` も `cache_read_input_tokens` も一致）。上位がロールアップで配列がその内訳。長さ2以上の実例をまだ見ていないので、**上位が内訳の合計なのか最終イテレーションなのかは未確認**。いまは本数だけ持つ。

**同じ id が複数セッションに現れたときは最初に取り込んだほうに帰属させる。** トークンは1回しか払っていないので、fork した側の複製ぶんが0になるのはむしろ正しい。

---

## D-012: run とセッションは多対多。`runs.session_id` を捨てて `session_runs` を作る（2026-09-02）

**D-001 の時点で組んだ「1セッションが複数の run を持つ」という片方向の関係は間違いだった。**

M2 の実装中、`runs` の挿入試行42回に対して行が33しかできないことに気づいて追ったところ、**1つの run が複数のセッションを生む**経路が見つかった。

実測（`unibridge`、2026-09-02確認）:

| run | 生んだセッション | 期間 |
|---|---|---|
| `bf4ff50f-…` | **8** | 2026-08-02T11:59 → 08-08T10:09 |
| `23758181-…` | 2 | 08-12T02:27 → 08-12T19:10 |
| `a46a0092-…` | 2 | 08-10T08:01 → 08-10T11:35 |

`bf4ff50f` の8ファイルは**隣り合う末尾と先頭のタイムスタンプが25ms以内で連なり、8本すべてがちょうど1回の `/clear` を含む**。つまり `/clear` は会話ファイルを新しくするが CLI の実行IDは変えない。しかも `bf4ff50f` という名前の `.jsonl` は存在しない。逆に session `9182f0fd` は4つの run を持つがそのどれも自分のIDではない（`/clear` で生まれた側だから）。

- 1セッション : 多run … `--resume`
- 1run : 多セッション … `/clear`

**片方向だと思うと、`runs` に「持ち主のセッション」を1つ選ばせることになる。** 実装は最初そうなっていて、8セッションのうちアルファベット順で最初のものを恣意的に選んでいた。さらに `messages.run_id` をセッション単位で照合していたため、**19,577件あるべき run 紐付きメッセージが12,404件に落ちていた**（7,173件の取りこぼし）。

**採用:** `runs` は実行そのものだけを持ち、`session_runs(session_id, run_id, seq)` で対応を張る。`seq` はそのセッション内での実行順（0 = 初回、以降 `--resume`）。run 側から見ると「この一続きの実行が生んだ会話たち」が辿れる。

`0001_init.sql` を書き換えて反映した（マイグレーションの追記ではなく）。**まだどこにもデプロイしておらず、コーパスは6秒で作り直せる**ため、`ALTER TABLE RENAME` で `messages.run_id` の参照先が書き換わる罠を踏むより初期スキーマを正すほうが安全だと判断した。

---

## D-011: アプリ認証を最初のHTTPエンドポイントから入れる（2026-09-01）

Tailscaleだけでは境界として不十分。tailnetには21デバイスあり、`tagged-devices`（k8s Operator）、引き出しのiPhone X、119日オフラインのQuest 2、Apple TVが含まれる。Phase 0の時点で全トランスクリプトがそこに露出する。

単一ユーザーなのでセッションCookie1つで足りる。**後から既存のAPI面に被せるより最初から入れるほうが安い**ので、最初のエンドポイントと同時に入れる。オリジン検証とパス制限も同時に。

---

## D-010: raw_json は生のまま保存する。リダクトしない（2026-09-01）

**実データで検証した結果、リダクトは有害と判明した。**

`.githooks/pre-push` の高信頼パターンで現在の165MBを走査したところ、`ghp_` 1件・`BEGIN … PRIVATE KEY` 9件がヒットしたが、**10件すべて偽陽性**だった。全部が `tool_use`（Bashコマンド）内のパターン文字列そのもので、鍵の本体が続くものはゼロ。おそらくpre-pushフック自体を書いていた時のコマンド。

**素朴なパターンリダクトを入れていたら、10件の正当なレコードを壊して得るものはゼロだった。**このコーパスでの誤検出率は100%。加えてCampの主目的は「後で探せること」なので、価値の大半がある `tool_result`（55.9MB）を壊すのは自己矛盾になる。

**採用する方針:**

- **生のまま保存する**
- **破壊しない検出器を置く。** `sensitive_findings` テーブルに「ここが引っかかった」だけ記録し、消さない。後から個別に判断できるようにする
- アプリ認証を入れる（D-011）
- SQLiteの保存時暗号化は鍵が同じ箱にある以上**バックアップ流出にしか効かない**。バックアップを外部に置く場合のみ検討する

**却下:**

- 危ない部分だけ暗号化 → 検出問題と同じもの。特定できないから困っているので、暗号化に置き換えても偽陰性率は同じまま鍵管理だけ増える
- 1Password等との連携 → シークレットマネージャが解くのは「**使う**シークレットをどこに置くか」で、「**漏れた**シークレットをどうするか」ではない。既にトランスクリプトに入ったものを後から金庫に入れることはできない

**Camp外の関連タスク（今日いちばん効く一手）:** `Vault Vault のあるノートに平文パスワードがある。ここをシークレットマネージャ参照に置き換えれば、以後のセッションは平文を持たなくなる。これはVault側の衛生の話でCampの機能ではない。

---

## D-009: バックエンドはGo、フロントはVite + React SPA（2026-09-01）

Next.jsは使わない（SSRが要らない単一ユーザーアプリで抽象層を2つ持つ理由が無い）。詳細と却下理由は `40-layout.md`。

---

## D-008: 権限確認UIは作れる。D-006の「妥協点」を撤回（2026-09-01）

`--permission-prompt-tool` は**存在する**。`.hideHelp()` で登録されているため `--help` に出ないだけだった。

```
$ claude --permission-prompt-tool
error: option '--permission-prompt-tool <tool>' argument missing   ← 受理されている
$ claude --this-flag-does-not-exist
error: unknown option '--this-flag-does-not-exist'                 ← 比較対象
```

リテラル `stdio` を渡すと `can_use_tool` 制御リクエストが stdin/stdout に流れ、**ホストが答えるまでターンがブロックされる**。Agent SDK の `canUseTool` と同じ経路。実フレームの捕捉と allow 後のファイル生成まで確認済み。

**→ プロジェクトごとの permission-mode プリセットに逃げる必要はない。承認UIを作る。**

注意: 既定のパーク期限5分（`CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`）。読み取り専用コマンドは安全fastpathで承認なしに走るので、**どの呼び出しが承認を出すかは予測できない**。UIは「来たら出す」設計にする。

詳細は `30-session-protocol.md`。

---

## D-007: プラン上限は制御プロトコルで取る。D-005を覆す（2026-09-01）

statusLine は「唯一の現実的な公式ルート」ではなかった。

- `rate_limit_event` が**要求なしにストリームへ届く**（`unifiedWindows.five_hour` / `seven_day`）
- `get_usage` 制御リクエストは statusLine より豊富（`utilization`、ISO `resets_at`、`severity`、拘束中の窓を示す `is_active`）
- `get_context_usage` はカテゴリ別のコンテキスト内訳
- **これらの制御フレームはトークンを消費しない**

さらに **Codex はレート制限を会話記録に直接埋め込んでいる**（`primary.window_minutes: 300` = 5時間枠、`secondary: 10080` = 7日枠、`plan_type`）。

**→ どちらのエージェントも statusLine を必要としない。** D-005 の「Admin APIは使えない」という部分だけは有効なまま。

---

## D-006: セッション駆動は `claude` CLI のヘッドレス双方向JSONモード（2026-09-01）

**却下: Claude Agent SDK。** Proサブスクの範囲に収める制約と両立しない。公式ドキュメントの明記:

> Unless previously approved, Anthropic does not allow third party developers to offer claude.ai login or rate limits for their products, including agents built on the Claude Agent SDK. Use the API key authentication methods described in the Quickstart instead.

Agent SDKはAPIキー＋クレジット前提。

**採用:**

```bash
claude -p --input-format stream-json --output-format stream-json \
       --verbose --include-partial-messages --resume <session_id>
```

- `--input-format stream-json` は "realtime streaming input" なので、stdin/stdoutで多ターン対話が成立する（v2.1.252の `--help` で確認）
- 構造化NDJSONが直接来るので、TUIをパースしない
- `--bare` を付けなければOAuth（claude.aiログイン）を使う。根拠: ドキュメントが `--bare` について "bare mode doesn't use your subscription login" / "In bare mode, Claude Code never reads OAuth credentials or the system keychain" と書いている

**~~妥協点: 対話的権限確認は無い~~ → D-008 で撤回。** `--permission-prompt-tool` は隠しフラグとして実在し、承認UIは作れる。この節を書いた時点では `--help` にないことをもって「存在しない」と誤って結論していた。

**却下: node-pty でTUIをラップ。** ANSI再描画を画面として扱うとメッセージ単位の構造が取れない。

**併存: `claude remote-control`。** 既にsystemdで常駐しており claude.ai/code から新規セッション作成まで動く。置き換え先にはしない（独立保持・横断検索・可視化・Vault連携が得られないため）。

---

## D-005: プラン上限はstatusLineのstdin JSONから取る（2026-09-01）— **D-007で覆した**

statusLineコマンドがstdinで受け取るJSONに実数で入っている:

```json
"rate_limits": {
  "five_hour":   { "used_percentage": 23.5, "resets_at": 1738425600 },
  "seven_day":   { "used_percentage": 41.2, "resets_at": 1738857600 },
  "spend_limit": { "used_percentage": 62.8, "resets_at": 1740787200 }
}
```

`resets_at` はUnix epoch秒。ローリング窓による近似計算は不要になった。既存の `~/.claude/statusline.sh` に横流しの1行を足す。

**制約:** statusLineは対話TUIセッションでしか発火しない（`claude -p` では動かない）。ただしレート制限はアカウント単位なので、どこか1つ対話セッションが開いていれば更新される。

**却下: Admin API。** `client.beta.organization` の使用量・コストレポートは組織管理用でAdmin APIキー（`sk-ant-admin...`）が要る。個人Proサブスクの枠とは別系統で、通常のAPIキーは拒否される。

---

## D-004: Obsidianを最終的に置き換える（2026-09-01）

Vault連携は副次的な機能ではなく主目的側。動機はObsidianへの不満:

- ビューアーをカスタムしてなんぼのソフトで、答えが見つかりにくくプラグイン探しが手間
- 選択肢は多い割にピタッとはまるものが少ない
- Basesや可視化系プラグインの成果物は**人も扱いにくくAI向きでもない**

**要件:** ビュー定義は「人間にレンダリングできる」だけでなく「モデルへの文脈として供給できる」形にする。ここが既存Obsidianプラグイン（Obsidianの中でしかレンダリングされない）との決定的な違い。

---

## D-003: 他ホストへはSSH経由・エージェントなし（2026-09-01）

各ホストへのSSHエイリアスと鍵は既に設定済みなので、常駐エージェントを配らない。履歴取得もリモート起動もsshで行う。

---

## D-002: 実行基盤は新規VMではなく `general-console` を拡張（2026-09-01）

lpve上のVM 200。claude・13リポジトリ・全ホストへのSSH鍵が既に揃っているため。k8sは採らない（PTY・長時間プロセス・HOMEの永続化との相性、かつlpve上に稼働中クラスタが無い）。

2026-09-01にメモリ8GB→16GB、ディスク64GB→128GBへ増設して前提条件を解消済み。

---

## D-001: 履歴は自前SQLiteへ複製し、元が消えても行を消さない（2026-09-01）

「独立して保持」の核心。CLI側でJSONLが消えても行は削除せず、消えた事実だけ記録する。

JSONLは追記専用なのでバイトオフセットを覚えておけば差分追尾が安い（※この前提は検証対象）。
