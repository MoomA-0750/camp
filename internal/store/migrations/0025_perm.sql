-- Phase 3.7 の M41。セッションごとの確認の度合い（cli / ask / edits / auto / full）。
--
-- NOT NULL DEFAULT なので、それより前の行は cli（本人の設定のまま、CLI と同じ）で埋まる。
-- **ただし既存の Codex の行は cli ではない**——Phase 3.6 では Camp 専用の置き場で
-- approvalPolicy=untrusted を渡して起こしていた（Fable の設計レビュー）。表示だけの legacy を入れる
-- （頼める値ではない。session.validPerm は通さない）。
ALTER TABLE runtime_sessions ADD COLUMN perm TEXT NOT NULL DEFAULT 'cli';
UPDATE runtime_sessions SET perm = 'legacy' WHERE agent = 'codex';
