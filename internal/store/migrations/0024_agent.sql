-- Phase 3.6。Camp が起こしたセッションが、どのエージェントか（claude / codex）。
--
-- NOT NULL DEFAULT なので、それより前の行は claude で埋まる（それまで claude しか
-- 起こせなかった）。NULL を「claude」と読む約束をコードに散らさない（Fable の設計レビュー 10）。
--
-- claude_id 列は「エージェント自身のセッション id」として使い、Codex ではスレッド id を入れる。
-- 列名は変えない（名前を変える移行のほうが危ない）。形式が違う（Claude は UUIDv4、
-- Codex は 01a0… の UUIDv7 系）ので混ざらない。取り込むときは agent と組で読む。
ALTER TABLE runtime_sessions ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude';
