-- Phase 3 / M28。**待っている承認を、campd の外に置く。**
--
-- 承認要求が来ると、子は答えるまでそのターンを止めて待つ。待ちが campd の
-- メモリにしか無いと、campd を入れ替えた瞬間に「誰が何を訊かれていたか」が
-- 消える——子はまだ待っているのに、画面には何も出ない。
--
-- 期限も置く。`claude` 自身のパーク期限は5分（CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS）で、
-- そこまで放っておくと**子が勝手に諦める**。Camp はその手前で自分から拒否して、
-- 「期限切れで拒否した」と書く。**何が起きたか分からない記録を残さない。**
CREATE TABLE approvals (
  id          INTEGER PRIMARY KEY,
  session_id  TEXT NOT NULL,
  request_id  TEXT NOT NULL,
  tool        TEXT,
  detail_json TEXT,               -- 何を承認しようとしているか（画面に出す）
  asked_at    TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  answered_at TEXT,
  behavior    TEXT,               -- allow / deny
  reason      TEXT,               -- user / timeout / session_ended
  UNIQUE(session_id, request_id)
);

CREATE INDEX ix_approvals_open ON approvals(session_id, asked_at)
  WHERE answered_at IS NULL;
