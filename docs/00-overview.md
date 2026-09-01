# Camp — 全体像

一次情報源は Obsidian Vault の `Human/Projects/Camp.md`。ここは技術仕様のみを置く。

## 解こうとしている問題

AIとの作業が散らばっている。

- セッションはCLIごと・ホストごと・ディレクトリごとにサイロ化する
- 保存場所は `~/.claude/projects/<パス由来のマングル名>/<uuid>.jsonl` で、人間が辿れる形になっていない
- `--spawn worktree` によってさらに断片化する
- 保持期間はCLIの都合で決まる
- 結果、「あの件、前にどう考えたか」を横断で引けない

一方でVaultは知識の本体として育っているのに、**そのノートを生んだ思考過程はJSONLの中に埋もれていて、両者が繋がっていない。**

## 構成

```text
ブラウザ / Android / Wear OS
    ↓ Tailscale（tailscale serve でHTTPS）
general-console（lpve VM 200・Fedora 44・4コア/16GB/128GB）
    ├── Web UI（Next.js）
    ├── バックエンド（Hono / Node.js）
    │     ├── 取り込みワーカー（JSONL追尾 + codex SQLite）
    │     ├── セッション駆動（claude -p 双方向stream-json）
    │     ├── Vault API（正本の作業コピーを読み書き）
    │     └── SSH クライアント（他ホストの履歴取得・リモート起動）
    ├── SQLite（履歴の独立保持・全文検索・使用量集計）
    └── Vault 作業コピー（= 正本）
```

クライアントは全部同じAPIを叩く薄いクライアントにする。Android・Wear OSを後から足すときに実装を1本化するため。

## 役割は4つ

| | 役割 | 位置づけ |
|---|---|---|
| 1 | **保持と発見** — 独立保持、横断検索 | 土台。他は作り直せるが失った履歴は戻らない |
| 2 | **駆動** — どこからでもセッションを起こして話す | 差別化は薄い（`claude remote-control`が既にある） |
| 3 | **統合** — Vaultとセッションの相互リンク、最終的にObsidian置き換え | 主目的側 |
| 4 | **計器** — トークン内訳・プラン残量 | 横断的関心事 |

## ドキュメント

| ファイル | 内容 |
|---|---|
| `00-overview.md` | これ |
| `10-decisions.md` | 意思決定ログ（なぜそう決めたか） |
| `20-data-model.md` | SQLiteスキーマと取り込み仕様 |
| `30-session-protocol.md` | `claude -p` 双方向stream-jsonのプロトコル |
| `40-view-engine.md` | ビュー機構（人間向け表示とLLM文脈供給の両立） |
