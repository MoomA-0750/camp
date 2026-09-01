# 意思決定ログ

新しい決定は上に追記する。覆した決定は消さず「覆した」と書き残す。

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

**妥協点:** `canUseTool` 相当の対話的権限確認は無い。v2.1.252の `--help` に `--permission-prompt-tool` は存在しない。`--permission-mode`（acceptEdits / auto / bypassPermissions / manual / dontAsk / plan）と `--allowedTools` / `--disallowedTools` による**事前指定**になる。プロジェクトごとにプリセットを持たせる。

**却下: node-pty でTUIをラップ。** ANSI再描画を画面として扱うとメッセージ単位の構造が取れない。

**併存: `claude remote-control`。** 既にsystemdで常駐しており claude.ai/code から新規セッション作成まで動く。置き換え先にはしない（独立保持・横断検索・可視化・Vault連携が得られないため）。

---

## D-005: プラン上限はstatusLineのstdin JSONから取る（2026-09-01）

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
