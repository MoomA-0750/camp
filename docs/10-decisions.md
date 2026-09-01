# 意思決定ログ

新しい決定は上に追記する。覆した決定は消さず「覆した」と書き残す。

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
