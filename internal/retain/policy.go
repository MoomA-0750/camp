package retain

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Policy は retention の1行。
type Policy struct {
	ID          int64
	Name        string
	Kind        Rule
	Scope       string
	KeepDays    sql.NullInt64
	AppliesFrom sql.NullString
	Enabled     bool
	Note        string
}

// Policies は登録されている規則を返す。enabled だけに絞れる。
func Policies(db *store.DB, onlyEnabled bool) ([]Policy, error) {
	rows, err := db.Query(`
		select id, name, kind, scope, keep_days, applies_from, enabled, coalesce(note,'')
		  from retention
		 where (? = 0 or enabled = 1)
		 order by id`, b2i(onlyEnabled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		var kind string
		var en int
		if err := rows.Scan(&p.ID, &p.Name, &kind, &p.Scope, &p.KeepDays,
			&p.AppliesFrom, &en, &p.Note); err != nil {
			return nil, err
		}
		p.Kind = Rule(kind)
		p.Enabled = en == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetEnabled は規則の有効・無効を切り替える。
func SetEnabled(db *store.DB, name string, on bool) error {
	r, err := db.Exec(`update retention set enabled = ? where name = ?`, b2i(on), name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("そんな規則は無い: %s", name)
	}
	return nil
}

// PlanRow は1メッセージぶんの見積り。
type PlanRow struct {
	MessageID   int64
	SessionID   string
	Bytes       int  // 落とせるバイト数
	Recoverable bool // 元ファイルが今もあるか
	Policy      string
}

// Plan は「何件・何バイト・どのセッションが対象か」を出す。
//
// **実行の前に必ずここを通る。** --dry-run と --apply が別の道を通ると、
// 見せた数と消える数が食い違う。だから apply も Plan の結果をそのまま消す。
//
// 元ファイルが消えている行は既定で外す。戻せない削除は明示しない限りしない。
func Plan(db *store.DB, includeUnrecoverable bool) ([]PlanRow, error) {
	pols, err := Policies(db, true)
	if err != nil {
		return nil, err
	}
	if len(pols) == 0 {
		return nil, nil
	}

	exists, err := sourceFileLiveness(db)
	if err != nil {
		return nil, err
	}

	// 規則ごとに候補を絞ってから raw_json を読む。**全行を読まない。**
	// 33,621 行の raw_json を全部読むと 161 MB をなめることになる。
	byMessage := map[int64]*PlanRow{}
	for _, p := range pols {
		rows, err := candidates(db, p)
		if err != nil {
			return nil, err
		}
		for _, c := range rows {
			_, _, changed, err := Trim(c.raw, []Rule{p.Kind})
			if err != nil || !changed {
				continue
			}
			// 同じ行に複数の規則が当たることがあるので、まとめて測り直す。
			if _, ok := byMessage[c.id]; !ok {
				byMessage[c.id] = &PlanRow{
					MessageID:   c.id,
					SessionID:   c.session,
					Recoverable: c.srcID.Valid && exists[c.srcID.Int64],
					Policy:      p.Name,
				}
			} else {
				byMessage[c.id].Policy += " + " + p.Name
			}
		}
	}

	var out []PlanRow
	for id, r := range byMessage {
		// まとめて当てたときの実際の削減量を測る。
		var raw []byte
		if err := db.QueryRow(`select raw_json from messages where id = ?`, id).Scan(&raw); err != nil {
			return nil, err
		}
		_, removed, changed, err := Trim(raw, allRules(pols))
		if err != nil || !changed {
			continue
		}
		r.Bytes = removed
		if !r.Recoverable && !includeUnrecoverable {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

// Apply は Plan の対象を実際に落とし、tombstone を残す。
func Apply(db *store.DB, plan []PlanRow, actor string) (Outcome, error) {
	var out Outcome
	pols, err := Policies(db, true)
	if err != nil {
		return out, err
	}
	rules := allRules(pols)
	now := time.Now().UTC().Format(time.RFC3339)

	for _, p := range plan {
		var raw []byte
		var srcID, off sql.NullInt64
		if err := db.QueryRow(`
			select raw_json, source_file_id, byte_offset from messages where id = ?`,
			p.MessageID).Scan(&raw, &srcID, &off); err != nil {
			return out, err
		}
		trimmed, removed, changed, err := Trim(raw, rules)
		if err != nil || !changed {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(`update messages set raw_json = ? where id = ?`, trimmed, p.MessageID); err != nil {
			tx.Rollback()
			return out, err
		}
		if err := insertTombstone(tx, tombstone{
			Kind: KindTrim, Ref: fmt.Sprint(p.MessageID),
			SourceFileID: srcID, ByteOffset: off,
			Reason: p.Policy, Actor: actor, At: now,
			Bytes: int64(removed), Recoverable: p.Recoverable,
			Note: "行は残し、読めない部分だけ落とした",
		}); err != nil {
			tx.Rollback()
			return out, err
		}
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out.Messages++
		out.BytesRemoved += int64(removed)
		if !p.Recoverable {
			out.Unrecoverable++
		}
	}
	return out, nil
}

type candidate struct {
	id      int64
	session string
	srcID   sql.NullInt64
	raw     []byte
}

// candidates は規則ごとに、raw_json を読む価値のある行だけを絞る。
func candidates(db *store.DB, p Policy) ([]candidate, error) {
	where := ""
	switch p.Kind {
	case RuleThinkingSignature:
		// thinking を持つのは assistant だけ。派生行の有無では絞れない
		// （署名だけの thinking はブロックを作らないため）。
		where = `m.type = 'assistant' and instr(m.raw_json, '"thinking"') > 0`
	case RuleDuplicateImage:
		where = `instr(m.raw_json, '"toolUseResult"') > 0 and instr(m.raw_json, '"image"') > 0`
	default:
		return nil, fmt.Errorf("知らない規則: %s", p.Kind)
	}

	args := []any{}
	if p.KeepDays.Valid {
		where += ` and m.timestamp < ?`
		args = append(args, time.Now().UTC().AddDate(0, 0, -int(p.KeepDays.Int64)).Format(time.RFC3339))
	}
	if p.AppliesFrom.Valid {
		where += ` and coalesce(sf.first_seen_at, '') >= ?`
		args = append(args, p.AppliesFrom.String)
	}
	if len(p.Scope) > 8 && p.Scope[:8] == "session:" {
		where += ` and m.session_id = ?`
		args = append(args, p.Scope[8:])
	}

	rows, err := db.Query(`
		select m.id, m.session_id, m.source_file_id, m.raw_json
		  from messages m
		  left join source_files sf on sf.id = m.source_file_id
		 where `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.session, &c.srcID, &c.raw); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// sourceFileLiveness は元ファイルが今もディスクにあるかを一度に調べる。
func sourceFileLiveness(db *store.DB) (map[int64]bool, error) {
	rows, err := db.Query(`select id, path from source_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		_, statErr := os.Stat(path)
		out[id] = statErr == nil
	}
	return out, rows.Err()
}

func allRules(pols []Policy) []Rule {
	out := make([]Rule, 0, len(pols))
	for _, p := range pols {
		out = append(out, p.Kind)
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
