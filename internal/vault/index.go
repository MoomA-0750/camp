package vault

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// IndexResult は1回の索引。
type IndexResult struct {
	VaultID   int64
	Root      string
	Scanned   int
	Added     int
	Changed   int
	Same      int
	Missing   int // 前回あって今回無かった
	Restored  int // missing だったものが戻ってきた
	Stored    int // blobs に新しく入れた数
	Links     int // 本文に現れた wikilink の延べ数
	LinkRows  int // note_links の行数（同じノートから同じ先へは1本に畳む）
	Resolved  int
	Dangling  int
	Ambiguous int
	Bytes     int64
	Pruned    []string
	ByKind    map[string]int
	Took      time.Duration
}

// bodyKinds は中身まで保存する種別。画像やPDFは行だけ持つ。
var bodyKinds = map[string]bool{KindMarkdown: true, KindBase: true, KindCanvas: true}

// Index は Vault を索引する。**Vault には一切書かない。**
//
// 中身は file_backups と同じ blobs へ寄せる。内容アドレスなので、
// 同じ中身のノートが複数あっても1つしか持たないし、あとでノートが
// Vault から消えても中身は Camp に残る。
func Index(db *store.DB, hostName, root, name string) (*IndexResult, error) {
	started := time.Now()
	sc, err := Scan(root)
	if err != nil {
		return nil, err
	}

	hostID, err := upsertHost(db, hostName)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = path.Base(sc.Root)
	}
	vaultID, err := upsertVault(db, hostID, sc.Root, name)
	if err != nil {
		return nil, err
	}

	// 既存の索引を先に全部読む。SetMaxOpenConns(1) なので、
	// Rows を開いたまま Exec はできない。
	type known struct {
		id      int64
		hash    string
		missing bool
	}
	prev := map[string]known{}
	rows, err := db.Query(
		`select id, path, coalesce(sha256, ''), missing_at is not null from notes where vault_id = ?`,
		vaultID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k known
		var p string
		if err := rows.Scan(&k.id, &p, &k.hash, &k.missing); err != nil {
			rows.Close()
			return nil, err
		}
		prev[p] = k
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res := &IndexResult{
		VaultID: vaultID, Root: sc.Root, Scanned: len(sc.Files),
		Bytes: sc.Bytes, Pruned: sc.Pruned, ByKind: sc.ByKind,
	}
	now := time.Now().UTC().Format(timeFmt)
	seen := make(map[string]struct{}, len(sc.Files))
	links := make(map[string][]Link, len(sc.Files))

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, f := range sc.Files {
		seen[f.Rel] = struct{}{}

		var hash string
		if bodyKinds[f.Kind] {
			body, rerr := Read(sc.Root, f.Rel)
			if rerr != nil {
				// 走査と索引の間に消えた。次回 missing で拾う。
				continue
			}
			if f.Kind == KindMarkdown {
				links[f.Rel] = ExtractLinks(body)
			}
			sum := sha256.Sum256(body)
			hash = hex.EncodeToString(sum[:])
			stored, serr := putBlob(tx, hash, body, now)
			if serr != nil {
				return nil, serr
			}
			if stored {
				res.Stored++
			}
		}

		k, had := prev[f.Rel]
		switch {
		case !had:
			res.Added++
		case k.hash != hash || k.missing:
			res.Changed++
			if k.missing {
				res.Restored++
			}
		default:
			res.Same++
		}

		title := strings.TrimSuffix(path.Base(f.Rel), f.Ext)
		first := now
		if had {
			first = "" // 既存は first_seen_at を上書きしない
		}
		if _, err := tx.Exec(`
			insert into notes(vault_id, path, title, kind, ext, mtime, size, sha256,
			                  content_hash, first_seen_at, missing_at)
			values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, null)
			on conflict(vault_id, path) do update set
			  title = excluded.title, kind = excluded.kind, ext = excluded.ext,
			  mtime = excluded.mtime, size = excluded.size,
			  sha256 = excluded.sha256, content_hash = excluded.content_hash,
			  missing_at = null`,
			vaultID, f.Rel, title, f.Kind, f.Ext, f.MTime, f.Size,
			nullStr(hash), nullStr(hash), nullStr(first)); err != nil {
			return nil, fmt.Errorf("note %s: %w", f.Rel, err)
		}
	}

	// 消えたものに印を付ける。行は消さない——「Campにしか残っていない」を作るため。
	for p, k := range prev {
		if _, ok := seen[p]; ok || k.missing {
			continue
		}
		if _, err := tx.Exec(`update notes set missing_at = ? where id = ?`, now, k.id); err != nil {
			return nil, err
		}
		res.Missing++
	}

	if err := writeLinks(tx, vaultID, sc, links, res); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`update vaults set scanned_at = ? where id = ?`, now, vaultID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	res.Took = time.Since(started)
	return res, nil
}

// putBlob は中身を blobs に入れる。既にあれば何もしない。
// 返り値は「新しく入れたか」。
func putBlob(tx *sql.Tx, hash string, body []byte, now string) (bool, error) {
	var exists int
	err := tx.QueryRow(`select 1 from blobs where sha256 = ?`, hash).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	codec, packed := ingest.PackBlob(body)
	_, err = tx.Exec(
		`insert into blobs(sha256, size, codec, content, stored_at) values(?, ?, ?, ?, ?)`,
		hash, len(body), codec, packed, now)
	return err == nil, err
}

func upsertHost(db *store.DB, name string) (int64, error) {
	if _, err := db.Exec(`insert into hosts(name) values(?) on conflict(name) do nothing`, name); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`select id from hosts where name = ?`, name).Scan(&id)
	return id, err
}

func upsertVault(db *store.DB, hostID int64, root, name string) (int64, error) {
	if _, err := db.Exec(
		`insert into vaults(host_id, name, root) values(?, ?, ?)
		 on conflict(host_id, root) do update set name = excluded.name`,
		hostID, name, root); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`select id from vaults where host_id = ? and root = ?`, hostID, root).Scan(&id)
	return id, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// writeLinks はリンクを張り直す。解決には Vault の全パスが要るので、
// ノートを全部入れ終わってから走らせる。
//
// 張り直しは「そのノートの分を消して入れ直す」。リンクは本文の付随物で、
// 人が付けた判断は乗っていないので、作り直して困るものが無い
// （`sensitive_findings` とは事情が違う。あちらは人の判定が乗るので消せない）。
func writeLinks(tx *sql.Tx, vaultID int64, sc *Result, links map[string][]Link, res *IndexResult) error {
	paths := make([]string, 0, len(sc.Files))
	for _, f := range sc.Files {
		paths = append(paths, f.Rel)
	}
	ix := NewLinkIndex(paths)

	ids := map[string]int64{}
	rows, err := tx.Query(`select id, path from notes where vault_id = ?`, vaultID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		ids[p] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for from, ls := range links {
		fromID, ok := ids[from]
		if !ok {
			continue
		}
		if _, err := tx.Exec(`delete from note_links where from_note_id = ?`, fromID); err != nil {
			return err
		}
		// 同じノートから同じターゲットへ複数回リンクしていることがある。
		// PRIMARY KEY(from_note_id, raw_target) なので最初の1本だけ残す。
		done := map[string]struct{}{}
		for _, l := range ls {
			res.Links++
			key := l.Target
			if l.SelfFrag {
				key = l.Frag
			}
			if _, dup := done[key]; dup {
				continue
			}
			done[key] = struct{}{}
			res.LinkRows++

			r := ix.Resolve(from, l.Target)
			if l.SelfFrag {
				r = Resolution{To: from}
			}
			var toID any
			if r.To != "" {
				if id, ok := ids[r.To]; ok {
					toID = id
					res.Resolved++
				}
			}
			if toID == nil {
				res.Dangling++
			}
			if r.Ambiguous {
				res.Ambiguous++
			}
			if _, err := tx.Exec(`
				insert into note_links(from_note_id, raw_target, to_note_id, resolved,
				                       alias, frag, embed, ambiguous, candidates, line)
				values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				fromID, key, toID, boolInt(toID != nil),
				nullStr(l.Alias), nullStr(l.Frag), boolInt(l.Embed),
				boolInt(r.Ambiguous), nullStr(strings.Join(r.Candidates, "\n")), l.Line); err != nil {
				return fmt.Errorf("link %s -> %s: %w", from, l.Target, err)
			}
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
