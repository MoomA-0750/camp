-- 2026-09-11。**終わったセッションが、どう終わったかを残す。**
--
-- それまでは exit_reason（自由な文）しか無く、「動いている途中で終わったのか」
-- 「承認を待たせたまま終わったのか」を後から選り分けられなかった。
--
--   end_cause  … なぜ終わったか。決まった語（internal/session/end.go）
--   end_state  … 終わる直前に何をしていたか（starting / idle / running）。
--                止めろと言った・見張りが外れた、の手前の状態を採る
--   prev_state … stopping / orphaned に入る直前の状態。end_state を決めるための控え
--
-- **それより前の行は NULL のまま。** exit_reason の文から推し量って埋めない
-- ——「分からない」を「こうだった」と書かない。
ALTER TABLE runtime_sessions ADD COLUMN end_cause TEXT;
ALTER TABLE runtime_sessions ADD COLUMN end_state TEXT;
ALTER TABLE runtime_sessions ADD COLUMN prev_state TEXT;

CREATE INDEX ix_runtime_ended ON runtime_sessions(ended_at DESC) WHERE state = 'exited';
