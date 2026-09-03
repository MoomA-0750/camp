package retain

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"os"

	"github.com/MoomA-0750/camp/internal/store"
)

// BackfillLineHashes は、行の身元を持っていない古い tombstone に sha256 を入れる。
//
// 0017 より前に作られた tombstone は `(source_file_id, byte_offset)` しか持たない。
// 元ファイルがまだ手元にあるうちに、その位置の行を読んでハッシュを入れておく。
// **ファイルが失われたあとでは二度と埋められない。**
//
// 返すのは (埋めた件数, 埋められなかった件数)。埋められないのは、元ファイルが
// 消えている・位置がファイル末尾を越えている・行を持たない削除（blob）の場合。
func BackfillLineHashes(db *store.DB) (filled, skipped int, err error) {
	rows, err := db.Query(`
		select t.id, sf.path, t.byte_offset
		  from tombstones t
		  join source_files sf on sf.id = t.source_file_id
		 where t.line_sha256 is null and t.byte_offset is not null
		 order by sf.path, t.byte_offset`)
	if err != nil {
		return 0, 0, err
	}
	type want struct {
		id   int64
		path string
		off  int64
	}
	var todo []want
	for rows.Next() {
		var w want
		if err := rows.Scan(&w.id, &w.path, &w.off); err != nil {
			rows.Close()
			return 0, 0, err
		}
		todo = append(todo, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(todo) == 0 {
		return 0, 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// 同じファイルを開き直さないよう、パスでまとめて処理する。
	var f *os.File
	var open string
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for _, w := range todo {
		if open != w.path {
			if f != nil {
				f.Close()
				f = nil
			}
			g, ferr := os.Open(w.path)
			if ferr != nil {
				skipped++
				open = w.path
				continue
			}
			f, open = g, w.path
		}
		if f == nil {
			skipped++
			continue
		}
		line, lerr := readLineAt(f, w.off)
		if lerr != nil || len(line) == 0 {
			skipped++
			continue
		}
		if _, err := tx.Exec(`update tombstones set line_sha256 = ? where id = ?`,
			LineHash(line), w.id); err != nil {
			return 0, 0, fmt.Errorf("tombstone %d: %w", w.id, err)
		}
		filled++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return filled, skipped, nil
}

// readLineAt は off から1行を読み、末尾の改行を落として返す。
func readLineAt(f *os.File, off int64) ([]byte, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}

var _ = sql.ErrNoRows
