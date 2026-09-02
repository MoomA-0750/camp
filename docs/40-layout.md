# リポジトリ構成

```
camp/
├── go.mod
├── cmd/campd/            エントリポイント（単一バイナリ）
├── internal/
│   ├── ingest/           JSONL差分追尾・パース・codex rollout
│   ├── store/            SQLite・スキーマ・マイグレーション
│   ├── search/           bigram索引・FTS5
│   ├── vault/            Vault索引・wikilink解決
│   ├── views/            ビュー機構（human renderer / context compiler）
│   ├── session/          claude -p の子プロセス監督
│   ├── mcp/              MCPサーバー（Campのビューと検索をツールとして公開）
│   ├── secrets/          破壊しない検出器（sensitive_findings）
│   └── httpapi/          ルーティング・認証・SPAフォールバック
└── web/                  Vite + React（TypeScript）
    └── dist/             ビルド成果物。campd が埋め込んで配る
```

## なぜGoか（D-009）

決め手は2つ。

1. **一番壊れやすい部分がGoの得意領域と重なる。** 独立レビューが最も攻撃したのはプロセスモデル（再起動後の照合、孤児プロセス、バックプレッシャー、stdoutを常時drainして境界付きログへ落とす）で、goroutine＋context＋容量制限チャネルがこれに素直に対応する。Nodeのstreamでも書けるが「バッファ無制限でプロセスが死ぬ」古典的失敗を踏みやすい
2. **静的バイナリ1個＋systemd unitでデプロイが終わる。** ホームラボ運用で効く

**差にならなかったもの:**

- 日本語FTS — bigramで解ける（辞書不要・約15行・言語non-dependent）。形態素解析（kagome）は不要で、検索の再現率ではbigramのほうが未知語問題が無いぶん有利ですらある
- SQLite — Node 22 の `node:sqlite`（experimental）でもFTS5は使えた。Goは `modernc.org/sqlite`（pure Go）で足りる
- unibridgeからの流用 — 中身は `unity/` `vpm/` `layouts/` `menus/` とBullMQ+Redisのジョブキューで、Campが使い回せる部分は実質無い（Campに必要なのはジョブキューではなくプロセス監督）

**代償:** フロントとの型共有を失う。単一ユーザーでAPI面が小さいので影響は限定的と判断した。

## ルーティング

SPAだがクライアントサイドルーティングで**本物のURLを持つ**。ブックマーク可能・リンク共有可能・戻る/進むが効く。

```
/sessions/<sessionId>
/sessions/<sessionId>/runs/<seq>
/notes/<vault相対パス>
/views/<viewId>?<フィルタ>
/search?q=
```

ノート↔セッションの相互リンクも、MCPサーバーがモデルに返すハンドルも、すべてこのURLに乗る。

**`httpapi` に catch-all を必ず置くこと。** 未マッチのパスは `index.html` を返す。忘れると深いURLの直接オープンとリロードが404になる。

## フロントのビルド成果物

`web/dist` を `embed.FS` で `campd` に埋め込み、単一バイナリで配る。開発時はViteのdevサーバーへプロキシする。

**M11 時点の暫定:** `web/dist` がまだ無いので、`internal/httpapi/assets/` に組み込みの2枚（`login.html`・仮の `index.html`）を置いて配っている。`campd serve -web <dir>` で実ビルドをディスクから配れる。M12 で埋め込みへ切り替える。

## 認証の位置（D-011 / D-020）

ゲートは `httpapi.Server.serve` の1本だけ。**ここを通らない経路を作らないこと。**

```
セキュリティヘッダ → /healthz だけ素通し → オリジン検証（状態を変える要求のみ）
  → ログイン経路2つ → 認証 → 振り分け → 未マッチの GET は殻
```

`internal/query` は一覧・詳細の読み取りをまとめた場所。HTTP からも、Phase 2 のMCPサーバーからも同じものを使う。

## 運用上の注意: システムの `sqlite3` CLI では FTS を触れない

Fedora の `sqlite3` コマンドは **FTS5 モジュールを持っていない**（`no such module: fts5`）。テーブルの中身は読めるが、`messages_fts` に対する `integrity-check` や `MATCH` は失敗する。

Camp本体は `modernc.org/sqlite` が FTS5 を同梱しているので問題ない。**点検は `campd doctor` を使うこと。**

```
$ campd doctor -v
ok    sqlite                   3.53.3
ok    journal_mode             wal
ok    foreign_keys             on
ok    fts5                     利用可
ok    messages_fts integrity   整合
ok    foreign_key_check        違反なし
```
