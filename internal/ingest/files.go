package ingest

import (
	"database/sql"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// ファイルに触った経路（origin）と、何をしたか（op）。
//
// origin は「どの記録から拾ったか」、op は「何をしたか」。同じ1回の編集が
// 複数の origin に現れる（Edit の tool_result と file-history-delta など）ので、
// 片方だけを見て件数を数えないこと。
const (
	originHistory    = "file-history" // file-history-delta。バックアップの控え
	originTool       = "tool-result"  // Read/Edit/Write の結果
	originAttachment = "attachment"   // 添付として本文に混ぜられたファイル

	opRead     = "read"          // Claude が読んだ
	opEdit     = "edit"          // Edit ツール
	opWrite    = "write"         // Write ツール
	opBackup   = "backup"        // file-history がバックアップを取った
	opExternal = "external-edit" // 人間が外で編集し、その差分が渡された
	opAttach   = "attach"        // 人間が添付した
	opMention  = "mention"       // 要約をまたいでファイル参照だけが残った
)

// fileRef は1行から読み取った「ファイルに触った」1件。
type fileRef struct {
	AbsPath       string
	Op            string
	Origin        string
	BackupName    string
	BackupVersion int64 // 0 は「無い」
	At            string

	// TargetUUID は file-history-delta の messageId。
	// file-history 行は uuid も sessionId も持たないので、同じファイルの中で
	// この uuid を持つアシスタント行を探して、編集を発行したターンに繋ぐ。
	TargetUUID string
	// ToolUseID は op をツール名から決めるための手がかり。
	ToolUseID string
}

// attachmentOp は attachment.type を op に翻訳する。
// 対象外の型（task_reminder, skill_listing など）は空を返す。
func attachmentOp(kind string) string {
	switch kind {
	case "edited_text_file":
		return opExternal
	case "file":
		return opAttach
	case "compact_file_reference":
		return opMention
	}
	return ""
}

// extractFileRefs は1行から、触られたファイルを全部拾う。
//
// パスは絶対のものだけ採る。相対パスは cwd を当てないと復元できず、
// cwd はセッションの途中で変わる（M3 で実測）。当て推量で絶対化すると、
// 存在しないノートへのリンクが静かに増える。
func extractFileRefs(l *Line) []fileRef {
	if l.Type == "file-history-delta" {
		p := l.AbsPath()
		if !isAbsPath(p) {
			return nil
		}
		r := fileRef{
			AbsPath:    p,
			Op:         opBackup,
			Origin:     originHistory,
			At:         l.Timestamp,
			TargetUUID: l.MessageID,
		}
		if l.Backup != nil {
			r.BackupName = l.Backup.BackupFileName
			r.BackupVersion = int64(l.Backup.Version)
			if r.At == "" {
				r.At = l.Backup.BackupTime
			}
		}
		return []fileRef{r}
	}

	var out []fileRef
	written, read := l.ToolResultPaths()
	if isAbsPath(read) {
		out = append(out, fileRef{
			AbsPath: read, Op: opRead, Origin: originTool,
			At: l.Timestamp, ToolUseID: l.ToolResultUseID(),
		})
	}
	if isAbsPath(written) {
		// op は空のまま。ツール名を引いてから決める（resolveOp）。
		out = append(out, fileRef{
			AbsPath: written, Origin: originTool,
			At: l.Timestamp, ToolUseID: l.ToolResultUseID(),
		})
	}
	if kind, name := l.AttachmentInfo(); isAbsPath(name) {
		if op := attachmentOp(kind); op != "" {
			out = append(out, fileRef{
				AbsPath: name, Op: op, Origin: originAttachment, At: l.Timestamp,
			})
		}
	}
	return out
}

func isAbsPath(p string) bool { return strings.HasPrefix(p, "/") }

// relTo は root 配下なら相対パスを返す。
//
// 大文字小文字は畳まない。実コーパスには Obsidian-Vault と Obsidian-vault が
// 両方あり（4ファイルが名前だけ同じ）、Linux 上では別物である。畳むと、
// 触っていないノートを触ったことにしてしまう。
func relTo(root, abs string) string {
	if root == "" || !strings.HasPrefix(abs, root+"/") {
		return ""
	}
	return abs[len(root)+1:]
}

// fileWriter は session_files を書く。
//
// 引き当てが2つ要る。どちらも「その行だけでは決まらない」ものである。
//   - tool … tool_result からツール名（Edit なのか Write なのか）
//   - root … セッションのプロジェクトルート（rel_path 用）
//
// file-history の繋ぎ先はここでは決めない。前から読んでいる最中には
// まだ書かれていないことがほとんどだから（LinkFileHistory を参照）。
type fileWriter struct {
	ins  *sql.Stmt
	tool *sql.Stmt
	root *sql.Stmt

	roots map[string]string // session_id -> プロジェクトのルート
	names map[string]string // tool_use_id -> ツール名
}

const fileInsertSQL = `
	insert into session_files(
		session_id, message_id, abs_path, rel_path, op, origin,
		backup_name, backup_version, at)
	values(?,?,?,?,?,?,?,?,?)
	on conflict(session_id, abs_path, at, op) do nothing`

func newFileWriter(tx *sql.Tx) (*fileWriter, error) {
	w := &fileWriter{roots: map[string]string{}, names: map[string]string{}}
	var err error
	for _, p := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&w.ins, fileInsertSQL},
		{&w.tool, `select tool_name from message_blocks
		            where kind = 'tool_use' and tool_use_id = ? limit 1`},
		{&w.root, `select coalesce(p.repo_path, '') from sessions s
		            left join projects p on p.id = s.project_id
		           where s.id = ?`},
	} {
		if *p.dst, err = tx.Prepare(p.sql); err != nil {
			w.Close()
			return nil, err
		}
	}
	return w, nil
}

func (w *fileWriter) Close() {
	for _, s := range []*sql.Stmt{w.ins, w.tool, w.root} {
		if s != nil {
			s.Close()
		}
	}
}

// projectRoot はセッションのプロジェクトルートを返す。セッションごとに1回だけ引く。
func (w *fileWriter) projectRoot(sessionID string) string {
	if r, ok := w.roots[sessionID]; ok {
		return r
	}
	var root string
	if err := w.root.QueryRow(sessionID).Scan(&root); err != nil {
		root = "" // プロジェクトが無いセッションもある。rel_path を諦めるだけ
	}
	w.roots[sessionID] = root
	return root
}

// resolveOp は tool_result のツール名から op を決める。
//
// 形（oldString があるか等）から当てる手もあるが、実測で Write の2件が
// type を持っておらず取り違える。tool_use を引けば取り違えは起きない。
func (w *fileWriter) resolveOp(toolUseID string) string {
	if toolUseID == "" {
		return opEdit
	}
	name, ok := w.names[toolUseID]
	if !ok {
		if err := w.tool.QueryRow(toolUseID).Scan(&name); err != nil {
			name = ""
		}
		w.names[toolUseID] = name
	}
	switch name {
	case "Write":
		return opWrite
	case "Read":
		return opRead
	}
	return opEdit
}

// write は1行ぶんのファイル参照を書く。書いた件数を返す。
//
// selfID はこの行自身の messages.id。file-history 行はいったんそれ自身を指す。
// 本来の繋ぎ先（編集を発行したアシスタント行）は LinkFileHistory が後から入れる。
func (w *fileWriter) write(selfID int64, sessionID string, l *Line) (int, error) {
	refs := extractFileRefs(l)
	if len(refs) == 0 {
		return 0, nil
	}
	root := w.projectRoot(sessionID)

	n := 0
	for _, r := range refs {
		if r.Op == "" {
			r.Op = w.resolveOp(r.ToolUseID)
		}
		if r.At == "" {
			continue // at は NOT NULL。時刻の無い参照は並べられないので捨てる
		}

		var backupName, relPath any
		if r.BackupName != "" {
			backupName = r.BackupName
		}
		if rel := relTo(root, r.AbsPath); rel != "" {
			relPath = rel
		}
		var ver any
		if r.BackupVersion > 0 {
			ver = r.BackupVersion
		}

		if _, err := w.ins.Exec(sessionID, selfID, r.AbsPath, relPath,
			r.Op, r.Origin, backupName, ver, r.At); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// LinkFileHistory は file-history の行を、編集を発行したアシスタント行に繋ぎ直す。
//
// なぜ取り込みの最中にやらないのか。実コーパスの 403 件のうち 372 件で、
// バックアップの記録がアシスタント行より「先」に書かれている。前から順に読む
// 取り込みでは、その時点で繋ぎ先がまだ存在しない。だから一旦 file-history 行
// 自身を指しておき、読み終えてから繋ぎ直す。
//
// 繋ぎ直したあとの指し先は assistant なので、この UPDATE は二度目以降 1行も
// 動かさない。取り込みを跨いで（delta だけ先に取り込まれ、アシスタント行が
// 次の実行で来る）でも、次の実行の最後に拾われる。
func LinkFileHistory(db *store.DB) (int64, error) {
	res, err := db.Exec(linkFileHistorySQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// raw_json は BLOB で持っている。SQLite 3.45 以降は BLOB を JSONB とみなすので、
// text に落としてから json_extract する。
const linkFileHistorySQL = `
	update session_files as f
	   set message_id = (
	         select t.id from messages d join messages t
	                on t.source_file_id = d.source_file_id
	               and t.uuid = json_extract(cast(d.raw_json as text), '$.messageId')
	          where d.id = f.message_id)
	 where f.origin = 'file-history'
	   and exists (
	         select 1 from messages d join messages t
	                on t.source_file_id = d.source_file_id
	               and t.uuid = json_extract(cast(d.raw_json as text), '$.messageId')
	          where d.id = f.message_id and d.type = 'file-history-delta')`
