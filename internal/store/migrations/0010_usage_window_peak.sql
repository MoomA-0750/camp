-- 窓ごとに「最後に見た値」だけでなく「その窓で到達した最大値」も残す。
-- 5時間枠は窓内で単調増加するので peak == 最新だが、7日枠は観測が飛ぶと
-- 落ちて見えることがある。ピークを別に持っておけば「その窓で壁に当たったか」
-- が後から分かる。samples は何回観測できたかで、値の信頼度の目安になる。
ALTER TABLE usage_windows ADD COLUMN peak_pct REAL;
ALTER TABLE usage_windows ADD COLUMN samples  INTEGER NOT NULL DEFAULT 0;
