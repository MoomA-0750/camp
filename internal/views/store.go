package views

// ビュー定義を Camp の DB に持つ（Phase 4 / M50、2026-09-13。移行 0030）。
//
// **本人の決定**: Vault には書かない。`.base` は読み取りのまま残し、しばらく併読する。
// 台紙1枚が1行で、ビューはその中の配列（`native.go`）。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Def は DB に入っている台紙1枚ぶんの定義。
type Def struct {
	Base         string `json:"base"`
	Body         string `json:"body"`
	Origin       string `json:"origin,omitempty"`
	OriginSHA256 string `json:"origin_sha256,omitempty"`
	ConvertedAt  string `json:"converted_at,omitempty"`
	UpdatedAt    string `json:"updated_at"`
	// BaseSHA256 は人が保存するときに「どの版を読んで直したか」（画面が読み込んだ本文の指紋）。
	// 保存には使うが、読み出しでは返さない（本文から誰でも計算できる）。
	BaseSHA256 string `json:"-"`
}

// HandEdited は「変換し直そうとしたが、そのあと人が直している」。
//
// **変換は手編集を黙って踏み潰さない。** M51 で life-tracker の変換を直すときに
// `-convert` を必ず走らせ直すので、そこで消えると気づけない（設計レビューの指摘6）。
var HandEdited = errors.New("変換のあとで手で直されている（上書きするなら force）")

// ErrNoDef はその台紙の定義が DB に無い。
var ErrNoDef = errors.New("その台紙の定義は無い")

// IsHandEdited は SaveDef が「手編集を上書きしようとした」で断ったか。
// 呼ぶ側（CLI・API）が errors を取り込まずに見分けられるように、ここに置く。
func IsHandEdited(err error) bool { return errors.Is(err, HandEdited) }

// IsNoDef はその台紙の定義が無いか。
func IsNoDef(err error) bool { return errors.Is(err, ErrNoDef) }

// Written は人が直したか、変換が書いたか。**履歴に残す**（`.base` にあった git 履歴の代わり）。
const (
	ByConvert = "convert"
	ByUser    = "user"
)

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// SHA256 は `.base` の中身の指紋。併読中に Obsidian 側で `.base` が変わったのを
// 「変換の誤り」と誤診しないために控える（設計レビューの指摘1）。
func SHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// LoadDefs は DB にある定義を全部読む（台紙の名前順）。
func LoadDefs(db *store.DB, vaultID int64) ([]*Def, error) {
	rows, err := db.Query(`
		select base, def, coalesce(origin,''), coalesce(origin_sha256,''),
		       coalesce(converted_at,''), updated_at
		  from views where vault_id = ? order by base`, vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Def{}
	for rows.Next() {
		var d Def
		if err := rows.Scan(&d.Base, &d.Body, &d.Origin, &d.OriginSHA256,
			&d.ConvertedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// LoadDef は1枚だけ読む。
func LoadDef(db *store.DB, vaultID int64, base string) (*Def, error) {
	var d Def
	err := db.QueryRow(`
		select base, def, coalesce(origin,''), coalesce(origin_sha256,''),
		       coalesce(converted_at,''), updated_at
		  from views where vault_id = ? and base = ?`, vaultID, base).
		Scan(&d.Base, &d.Body, &d.Origin, &d.OriginSHA256, &d.ConvertedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDef
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// LoadNativeDefs は DB の定義を読んで回せる形（`*Base`）にする。
//
// **1枚が読めないだけで全部を落とさない**（`LoadBases` と同じ方針）。読めなかった台紙は
// `ParseError` を持つ `*Base` として返し、行そのものは残す。
func LoadNativeDefs(db *store.DB, vaultID int64) ([]*Base, error) {
	defs, err := LoadDefs(db, vaultID)
	if err != nil {
		return nil, err
	}
	out := make([]*Base, 0, len(defs))
	for _, d := range defs {
		b, err := ParseNative(d.Base, []byte(d.Body))
		if err != nil {
			out = append(out, &Base{Path: "db:" + d.Base, Name: d.Base,
				ParseError: err.Error()})
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// ErrConflict は「読んだあとで、ほかが書き換えた」。**黙って上書きしない**
// （実装後レビュー、2026-09-13。codex の指摘2）。画面の2つのタブ、画面と `-convert` が
// 同時に書くと、あとから書いたほうが先の書き換えを消していた。
var ErrConflict = errors.New("読んだあとで、ほかが定義を書き換えた（読み直してから書く）")

// IsConflict は SaveDef が競合で断ったか。
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// SaveDef は定義を書く。**履歴を必ず1行残す。**
//
// by が ByConvert で、既にある行が「変換のあとで人が直している」（`handEdited`）なら、force で
// ない限り書かずに HandEdited を返す。
//
// **読んだ版の上にしか書かない。** 人の保存は d.BaseSHA256（画面が読み込んだ本文の指紋）を持って
// 来る。変換は自分で読んだ本文を使う。どちらも `update ... where def = <読んだ本文>` で書くので、
// 間にほかが書いていれば 0 行になり ErrConflict を返す。手編集の判定もこの読んだ版で行うので、
// 「判定したあとに人が保存し、変換がそれを踏む」も同じ守りで止まる。
func SaveDef(db *store.DB, vaultID int64, d *Def, by string, force bool) error {
	if d.Base == "" {
		return errors.New("台紙の名前が無い")
	}
	// 読めない定義を保存しない。**壊れたものを入れると画面から直せなくなる。**
	if _, err := ParseNative(d.Base, []byte(d.Body)); err != nil {
		return fmt.Errorf("定義が読めない: %w", err)
	}
	if by != ByConvert && by != ByUser {
		return fmt.Errorf("知らない書き手: %q", by)
	}

	cur, err := LoadDef(db, vaultID, d.Base)
	switch {
	case errors.Is(err, ErrNoDef):
		cur = nil
		if by == ByUser && d.BaseSHA256 != "" {
			return fmt.Errorf("%s: %w", d.Base, ErrConflict) // 読んだときはあったのに消えている
		}
	case err != nil:
		return err
	default:
		if by == ByUser && d.BaseSHA256 != SHA256([]byte(cur.Body)) {
			return fmt.Errorf("%s: %w", d.Base, ErrConflict)
		}
		if by == ByConvert {
			edited, err := handEditedBody(db, vaultID, d.Base, cur.Body)
			if err != nil {
				return err
			}
			if !force && edited {
				return fmt.Errorf("%s: %w", d.Base, HandEdited)
			}
			// 何も変わらない再変換は書かない（履歴を「変換」で埋めない）。
			if cur.Body == d.Body && cur.Origin == d.Origin && cur.OriginSHA256 == d.OriginSHA256 {
				return nil
			}
		}
	}

	at := nowUTC()
	conv := d.ConvertedAt
	if by == ByConvert {
		conv = at
	} else if cur != nil {
		conv = cur.ConvertedAt
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if cur == nil {
		if _, err := tx.Exec(`
			insert into views(vault_id, base, def, origin, origin_sha256, converted_at, updated_at)
			values(?,?,?,?,?,?,?)`,
			vaultID, d.Base, d.Body, nz(d.Origin), nz(d.OriginSHA256), nz(conv), at); err != nil {
			// 読んだときは無かったのに、間にほかが作った（UNIQUE で落ちる）。
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%s: %w", d.Base, ErrConflict)
			}
			return err
		}
	} else {
		res, err := tx.Exec(`
			update views set
				def = ?,
				-- **手で直しても、どこから変換したかは消さない**（2026-09-13、実ブラウザで見つけた）。
				-- 変換元を持たない手の保存が origin と origin_sha256 を NULL で上書きしていた。
				origin = coalesce(?, origin),
				origin_sha256 = coalesce(?, origin_sha256),
				converted_at = ?,
				updated_at = ?
			 where vault_id = ? and base = ? and def = ?`,
			d.Body, nz(d.Origin), nz(d.OriginSHA256), nz(conv), at, vaultID, d.Base, cur.Body)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%s: %w", d.Base, ErrConflict)
		}
	}
	if _, err := tx.Exec(`insert into view_history(vault_id, base, def, at, by) values(?,?,?,?,?)`,
		vaultID, d.Base, d.Body, at, by); err != nil {
		return err
	}
	return tx.Commit()
}

// HandEditedDef は「変換したあとに、`.base` で言える部分を人が直したか」。画面が注意を出すのに使う。
// **画面で決め直さない**（`-convert` が上書きするかと食い違う）。
func HandEditedDef(db *store.DB, vaultID int64, base string) (bool, error) {
	cur, err := LoadDef(db, vaultID, base)
	if errors.Is(err, ErrNoDef) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return handEditedBody(db, vaultID, base, cur.Body)
}

// handEditedBody は「最後に変換が書いた版」と「いまの版」を、**`.base` で言える部分だけ**比べる
// （本人の決定、2026-09-13 の実装後レビューのあと）。
//
// 再変換は `.base` で言えない部分（時間軸・集約・グラフのビュー）を引き継ぐ（`CarryOver`）ので、
// そこを手で直しただけの台紙は上書きしても失われない。**守るのは、絞り込み・列・並び・集計・
// 描き方（`.base` の columnConfigs・timeFrame・columnSize で言える部分）を人が直した台紙だけ。**
//
// 以前は「履歴の最後の書き手が人か」で決めていた。それだと銀行の `last` を足しただけで台紙が
// 丸ごと再変換から外れ、Obsidian 側の変更が届かなくなる（Fable の指摘1）。
//
// 変換が一度も書いていない台紙（手で書いたもの）は、常に手編集として守る。
func handEditedBody(db *store.DB, vaultID int64, base, body string) (bool, error) {
	conv, err := LastConverted(db, vaultID, base)
	if errors.Is(err, ErrNoDef) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	a, err := ParseNative(base, []byte(conv))
	if err != nil {
		return true, nil // 読めない版とは比べられない。守る側に倒す
	}
	b, err := ParseNative(base, []byte(body))
	if err != nil {
		return true, nil
	}
	return len(DiffBases(a, b)) > 0 || len(DiffEmit(a, b)) > 0, nil
}

// LastConverted は変換が最後に書いた版の本文。無ければ ErrNoDef。
func LastConverted(db *store.DB, vaultID int64, base string) (string, error) {
	var body string
	err := db.QueryRow(`
		select def from view_history
		 where vault_id = ? and base = ? and by = ?
		 order by id desc limit 1`, vaultID, base, ByConvert).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoDef
	}
	return body, err
}

// Revision は定義の版。**書くたびに必ず変わる**（履歴は追記だけなので、その最後の id）。
// 控え（画面と MCP のキャッシュ）はこれを鍵に含める——定義を直したのに、もう片方が最大
// 30 秒古い定義で描いていた（codex の指摘8）。
func Revision(db *store.DB, vaultID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`select coalesce(max(id), 0) from view_history where vault_id = ?`, vaultID).Scan(&id)
	return id, err
}

// HistoryEntry は書き換え1回。
type HistoryEntry struct {
	At   string `json:"at"`
	By   string `json:"by"`
	Body string `json:"body"`
}

// History は新しい順に返す。
func History(db *store.DB, vaultID int64, base string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(`
		select at, by, def from view_history
		 where vault_id = ? and base = ?
		 order by id desc limit ?`, vaultID, base, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.At, &e.By, &e.Body); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}
