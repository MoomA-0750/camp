package retain

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// Hit は「どの表のどの列に何件写っているか」。
type Hit struct {
	Table  string
	Column string
	Count  int

	// Unknown は「数えられなかった」。**0件とは違う。**
	Unknown bool
	Why     string
}

// Sweep は既知の値が**どの表のどの列に**残っているかを総なめで数える。
//
// 2026-09-03 の outer gate まで、`campd redact` は messages / message_blocks /
// blobs の3表しか見ていなかった。完了後の再走査も同じ3表だったので、
// `sessions.first_user_message` に平文が残ったまま **「0件」と表示して正常終了した**
// （実測で再現）。その列は `/api/sessions` が一覧に返す。
//
// 「見る場所を人が数え上げる」方式は必ず漏れるので、機械に数えさせる。
// gzip で入っている列は展開してから見る。
func Sweep(db *store.DB, secret []byte) ([]Hit, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("空の値は探せない")
	}
	tables, err := sweepTables(db)
	if err != nil {
		return nil, err
	}
	var out []Hit
	for _, t := range tables {
		cols, err := columnsOf(db, t)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			n, err := countIn(db, t, c, secret)
			if err != nil {
				// 数えられない列は「無い」ではなく「分からない」。
				// 呼ぶ側が 0 件と読み違えないよう、必ず表に出す。
				out = append(out, Hit{Table: t, Column: c, Unknown: true, Why: err.Error()})
				continue
			}
			if n > 0 {
				out = append(out, Hit{Table: t, Column: c, Count: n})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Table+out[i].Column < out[j].Table+out[j].Column
	})
	return out, nil
}

// Total は総件数。**数えられなかった列は 1 件以上あるものとして数える。**
// 「見ていないから 0」を「無いから 0」と読ませない。
func Total(hits []Hit) int {
	n := 0
	for _, h := range hits {
		if h.Unknown {
			n++
			continue
		}
		n += h.Count
	}
	return n
}

// Unknowns は数えられなかった列だけを返す。
func Unknowns(hits []Hit) []Hit {
	var out []Hit
	for _, h := range hits {
		if h.Unknown {
			out = append(out, h)
		}
	}
	return out
}

// FTSShadow は FTS5 が自分で管理する影の表かどうか。
//
// **直接書き換えてはいけない。** 索引が本体とずれる。ここに写っている値は
// message_blocks を作り直せば消える。
func FTSShadow(table string) bool {
	return strings.HasPrefix(table, "messages_fts")
}

func sweepTables(db *store.DB) ([]string, error) {
	rows, err := db.Query(`
		select name from sqlite_master
		 where type = 'table' and name not like 'sqlite_%'
		 order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func columnsOf(db *store.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`pragma table_info(%q)`, table))
	if err != nil {
		// 仮想表など、table_info を引けないものは飛ばす。
		return nil, nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// countIn は1つの列を数える。gzip の中も見る。
//
// **数えられなかったことを「0件」にしない。**
// 2026-09-04 の outer gate の指摘。走査の失敗を 0 に潰すと、総なめは
// 「秘密が残っていないこと」の検知器ではなくなる（見ていないから 0 なのを、
// 無いから 0 と読む）。数えられない列は Unknown として持ち上げる。
func countIn(db *store.DB, table, col string, secret []byte) (int, error) {
	// instr は BLOB でも TEXT でも効く。まずそれで大きく削る。
	var n int
	q := fmt.Sprintf(`select count(*) from %q where instr(coalesce(%q, ''), ?) > 0`, table, col)
	if err := db.QueryRow(q, secret).Scan(&n); err != nil {
		return 0, err
	}
	if FTSShadow(table) {
		return n, nil
	}

	// gzip で入っている列は展開して見る。blobs.content がこれに当たる。
	gz, err := countGzip(db, table, col, secret)
	if err != nil {
		return n, err
	}
	return n + gz, nil
}

func countGzip(db *store.DB, table, col string, secret []byte) (int, error) {
	q := fmt.Sprintf(`select %q from %q where hex(substr(%q,1,2)) = '1F8B'`, col, table, col)
	rows, err := db.Query(q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v []byte
		if err := rows.Scan(&v); err != nil {
			return n, err
		}
		plain, err := decodeBlob("gzip", v)
		if err != nil {
			continue
		}
		if bytes.Contains(plain, secret) {
			n++
		}
	}
	return n, rows.Err()
}
