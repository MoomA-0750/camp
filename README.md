# Camp

AIとの作業履歴と知識ベースを同じ場所に置く、個人用セルフホストWebアプリ。

Claude Codeのセッションと会話履歴をCLI側の都合から独立して保持し、横断検索でき、セッションの起動・対話もでき、トークン消費と残量が見え、Obsidian Vaultも同じ場所で読み書きできる。最終的にObsidianを置き換える。

**要件・仕様・意思決定ログの一次情報源はこのリポジトリではなくObsidian Vault側 `Human/Projects/Camp.md`。** このリポジトリはコード＋作業メモリ（`dev/active/*`、終わったものは `dev/done/*`）＋技術仕様（`docs/*`）という役割分担。

## ステータス

Phase 0 着手前。2026-09-01にリポジトリ作成。

| Phase | 内容 | 状態 |
|---|---|---|
| 0 | 取り込み・SQLite・全文検索・最小UI・statusLineフック | 設計中 |
| 1 | セッション駆動（`claude -p` 双方向stream-json） | 未着手 |
| 2 | Vault読み取り＋ノート↔セッション相互リンク | 未着手 |
| 3 | Vault編集・正本移行 | 未着手 |
| 4 | ビュー機構（Obsidian離脱の関門） | 未着手 |
| 5 | Android / Wear OS | 未着手 |

## 動かす場所

`general-console`（mooma-lpve上のVM 200、Fedora 44、4コア/16GB/128GB）に常設し、Tailscale内に閉じる。**実質「認証付き任意コード実行エンドポイント」なので公開しない。**

## 制約

- **Claude Proサブスクの範囲内に収める。** APIクレジットは使わない。これがClaude Agent SDKを採れない理由（Agent SDKはAPIキー前提）
- セッション駆動は `claude` CLI のヘッドレス双方向JSONモードで行う
- プラン上限（5時間枠・週次）はstatusLineのstdin JSONから取る

詳細は `docs/` を参照。
