# Camp

AIとの作業履歴と知識ベースを同じ場所に置く、個人用セルフホストWebアプリ。

Claude Codeのセッションと会話履歴をCLI側の都合から独立して保持し、横断検索でき、セッションの起動・対話もでき、トークン消費と残量が見え、Obsidian Vaultも同じ場所で読み書きできる。最終的にObsidianを置き換える。

**要件・仕様・意思決定ログの一次情報源はこのリポジトリではなくObsidian Vault側 `Human/Projects/Camp.md`。** このリポジトリはコード＋作業メモリ（`dev/active/*`、終わったものは `dev/done/*`）＋技術仕様（`docs/*`）という役割分担。

## ステータス

Phase 0 着手前。2026-09-01にリポジトリ作成。

2026-09-01に4方向の評価（取り込み層の実証・プロトコルの実証・Obsidian置き換えの実現可能性・codexによる独立レビュー）を経てフェーズを再構成した。

| Phase | 内容 | 状態 |
|---|---|---|
| 0 | 取り込み・SQLite・**日本語対応**全文検索・最小UI | 設計完了、実装前 |
| 1 | Vault読み取り＋ノート↔セッション相互リンク | 未着手 |
| 2 | ビュー機構の読み取り側＋LLM文脈コンパイラ＋**MCPサーバー公開** | 未着手 |
| 3 | セッション駆動（`claude -p` 双方向stream-json＋承認UI） | 未着手 |
| 4 | チャート・グラフビュー | 未着手 |
| 5 | 編集面（CodeMirror 6）→ 書き手の移行 | 未着手 |
| 6 | モバイル（まずPWA） | 未着手 |
| 将来 | Codex対応 | 保留 |

**当分Campは読み専用。** Vaultは従来どおりObsidian Gitで書く。正本の移行は編集面（Phase 5）ができてから。

## 動かす場所

`general-console`（mooma-lpve上のVM 200、Fedora 44、4コア/16GB/128GB）に常設し、Tailscale内に閉じる。**実質「認証付き任意コード実行エンドポイント」なので公開しない。**

## 制約

- **Claude Proサブスクの範囲内に収める。** APIクレジットは使わない。これがClaude Agent SDKを採れない理由（Agent SDKはAPIキー前提）
- セッション駆動は `claude` CLI のヘッドレス双方向JSONモードで行う
- **バックエンドはGo、フロントはVite + React SPA**（`docs/40-layout.md`）。Next.jsは使わない
- 履歴は生のまま保存し、リダクトしない。破壊しない検出器で記録だけ残す（`docs/10-decisions.md` D-010）
- Tailscale内に閉じたうえで**アプリ認証を最初から入れる**（D-011）
- プラン上限は `claude -p` の制御プロトコルから取る（`rate_limit_event` / `get_usage`。トークン消費ゼロ）
- 権限確認UIは `--permission-prompt-tool stdio` で作れる（`--help` に出ない隠しフラグだが実在する）

詳細は `docs/` を参照。
