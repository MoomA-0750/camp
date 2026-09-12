-- エージェントの語彙を1つに揃える（本人の決定 2026-09-12）。
--
-- `docs/20-data-model.md` は `sessions.agent` を「claude | codex」と書いていたが、取り込みは
-- `'claude-code'` を直に書いていた。走っているセッション（`runtime_sessions.agent`）は `claude` /
-- `codex` なので、**台帳も同じ語彙に揃える**。`?agent=claude-code` で絞っていた URL は外れる。
--
-- usage_windows は UNIQUE(agent, kind, ends_at, source) を持つ。素の UPDATE だと、同じ窓が既に
-- `claude` で入っていたときに衝突する（いまは `claude` を書く経路が無いので起きないが、書き方で
-- 防いでおく）。`ends_at` は NULL がありうるので `IS` で比べる。
DELETE FROM usage_windows
 WHERE agent = 'claude-code'
   AND EXISTS (
     SELECT 1 FROM usage_windows u2
      WHERE u2.agent = 'claude'
        AND u2.kind = usage_windows.kind
        AND u2.ends_at IS usage_windows.ends_at
        AND u2.source = usage_windows.source);

UPDATE usage_windows SET agent = 'claude' WHERE agent = 'claude-code';
UPDATE sessions      SET agent = 'claude' WHERE agent = 'claude-code';
