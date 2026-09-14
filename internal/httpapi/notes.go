package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"

	"github.com/MoomA-0750/camp/internal/noteedit"
	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/vault"
)

// ノートを編集する口（Phase 5 / M53）。**書くのは実行面**で、ここは頼むだけ。
//
//   - 開くときはディスクを直に読む（索引の遅れを書き始めた中身にしない）
//   - 保存は自動保存からも来る。**指示の紙（AGENTS.md など）はパスワードを入れ直さないと書かない**
//     （本人の決定8）
//   - ぶつかったら、重ならなければ合わせ、重なれば書かずに両方の版を返す（本人の決定6）
func (s *Server) noteRoutes() {
	if s.opts.Notes == nil {
		return
	}
	m := s.mux
	m.HandleFunc("GET /api/notes/{id}/source", s.handleNoteSource)
	m.HandleFunc("POST /api/notes/{id}/source", s.handleNoteSave)
	m.HandleFunc("GET /api/notes/{id}/source/sha", s.handleNoteDiskSHA)
	m.HandleFunc("POST /api/notes/{id}/resolve", s.handleNoteResolve)
	m.HandleFunc("GET /api/notes/sync", s.handleNoteSync)
	m.HandleFunc("GET /api/note-writes/{wid}", s.handleNoteWrite)
	// M55: 行き来・新しいノート・日次ログ・.trash へ移す
	m.HandleFunc("GET /api/vaults/{id}/names", s.handleVaultNames)
	m.HandleFunc("POST /api/vaults/{id}/notes", s.handleNoteCreate)
	m.HandleFunc("POST /api/vaults/{id}/daily", s.handleNoteDaily)
	m.HandleFunc("POST /api/notes/{id}/trash", s.handleNoteTrash)
	m.HandleFunc("GET /api/notes/{id}/writes", s.handleNoteWrites)
}

// handleNoteWrites はそのノートに Camp が書いた版の一覧。
func (s *Server) handleNoteWrites(w http.ResponseWriter, r *http.Request) {
	out, err := s.opts.Notes.Writes(int64(atoi(r.PathValue("id"))))
	if errors.Is(err, fs.ErrNotExist) {
		s.fail(w, r, http.StatusNotFound, "そのノートは無い")
		return
	}
	s.respond(w, r, out, err)
}

// handleVaultNames は名前の一覧（スイッチャーと wikilink の補完の元）。gen が同じなら中身を返さない。
func (s *Server) handleVaultNames(w http.ResponseWriter, r *http.Request) {
	out, err := s.opts.Notes.Names(int64(atoi(r.PathValue("id"))), r.URL.Query().Get("gen"))
	s.respond(w, r, out, err)
}

// noteEditErr は書く口の失敗を HTTP の状態に分ける。
func (s *Server) noteEditErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, r, http.StatusNotFound, "Vault に無い: "+err.Error())
	case errors.Is(err, noteedit.ErrBadName):
		s.fail(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, noteedit.ErrNeedReauth):
		s.fail(w, r, http.StatusForbidden, "エージェントの指示の紙なので、パスワードを入れ直さないと直せない")
	case errors.Is(err, noteedit.ErrReadOnly):
		s.fail(w, r, http.StatusForbidden, err.Error())
	case errors.Is(err, session.ErrNoAgent), errors.Is(err, session.ErrNoNoteVault), errors.Is(err, session.ErrOtherVault):
		s.fail(w, r, http.StatusServiceUnavailable, "いまは書けない: "+err.Error())
	default:
		s.fail(w, r, http.StatusInternalServerError, err.Error())
	}
}

// readNoteBody は本文を含む要求を読む（本文の上限は保存と同じ）。
func (s *Server) readNoteBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxNoteRequest+1))
	switch {
	case err != nil:
		s.fail(w, r, http.StatusBadRequest, "本文が読めない")
		return false
	case len(b) > maxNoteRequest:
		s.fail(w, r, http.StatusRequestEntityTooLarge, "本文が大きすぎる")
		return false
	case json.Unmarshal(b, v) != nil:
		s.fail(w, r, http.StatusBadRequest, "本文が JSON ではない")
		return false
	}
	return true
}

// handleNoteCreate は新しいノートを作る。既にあれば 409 と、そのノート。
func (s *Server) handleNoteCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Body string `json:"body"`
	}
	if !s.readNoteBody(w, r, &body) {
		return
	}
	out, err := s.opts.Notes.Create(int64(atoi(r.PathValue("id"))), body.Path, body.Body)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, out)
	case errors.Is(err, noteedit.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "その名前のノートは既にある: " + out.Path, "note_id": out.NoteID, "path": out.Path})
	default:
		s.noteEditErr(w, r, err)
	}
}

// handleNoteDaily はその日の日次ログを返す（無ければテンプレートから作る）。**開くだけで作られないよう POST。**
func (s *Server) handleNoteDaily(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Date string `json:"date"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	out, err := s.opts.Notes.Daily(int64(atoi(r.PathValue("id"))), body.Date)
	if err != nil {
		s.noteEditErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleNoteTrash はノートを .trash/ へ移す。画面で見ている版とディスクが同じときだけ。
func (s *Server) handleNoteTrash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Base     string `json:"base_sha256"`
		Password string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	reauthed := false
	if body.Password != "" {
		if !s.reauth(w, r, body.Password, "note-trash:"+r.PathValue("id")) {
			return
		}
		reauthed = true
	}
	out, err := s.opts.Notes.Trash(int64(atoi(r.PathValue("id"))), body.Base, reauthed)
	if err != nil {
		s.noteEditErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleNoteWrite は Camp が書いた1件の本文を返す（上書きされたものを見る・書き戻すため）。
func (s *Server) handleNoteWrite(w http.ResponseWriter, r *http.Request) {
	out, err := s.opts.Notes.Write(int64(atoi(r.PathValue("wid"))))
	if errors.Is(err, fs.ErrNotExist) {
		s.fail(w, r, http.StatusNotFound, "その控えは無い")
		return
	}
	s.respond(w, r, out, err)
}

// handleNoteDiskSHA はディスクの今のハッシュだけ返す（開いている間に変わったかを見るため。全文を引かない）。
func (s *Server) handleNoteDiskSHA(w http.ResponseWriter, r *http.Request) {
	sha, err := s.opts.Notes.DiskSHA(int64(atoi(r.PathValue("id"))))
	if errors.Is(err, fs.ErrNotExist) {
		s.fail(w, r, http.StatusNotFound, "そのノートは Vault に無い")
		return
	}
	s.respond(w, r, map[string]string{"sha256": sha}, err)
}

// handleNoteResolve はプレビューの wikilink の行き先を返す。**POST だが何も書かない**
// （宛先が数百あると URL が長くなりすぎるため）。
func (s *Server) handleNoteResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Targets []string `json:"targets"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	out, err := s.opts.Notes.Resolve(int64(atoi(r.PathValue("id"))), body.Targets)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, r, http.StatusNotFound, "そのノートは無い")
	case errors.Is(err, noteedit.ErrBadRequest):
		s.fail(w, r, http.StatusBadRequest, err.Error())
	default:
		s.respond(w, r, out, err)
	}
}

func (s *Server) handleNoteSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.opts.Notes.Open(int64(atoi(r.PathValue("id"))))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, r, http.StatusNotFound, "そのノートは Vault に無い")
	default:
		s.respond(w, r, src, err)
	}
}

// maxNoteRequest は保存の本文の上限。本文（notes.MaxBody）が JSON で伸びる分を見込む。
// 最後は実行面へ渡す1行の長さで照らす（session.NoteWrite）。
const maxNoteRequest = 2 << 20

func (s *Server) handleNoteSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Base     string `json:"base_sha256"`
		Body     string `json:"body"`
		Password string `json:"password"` // 指示の紙だけ
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxNoteRequest+1))
	switch {
	case err != nil:
		s.fail(w, r, http.StatusBadRequest, "本文が読めない")
		return
	case len(b) > maxNoteRequest:
		s.fail(w, r, http.StatusRequestEntityTooLarge, "本文が大きすぎる")
		return
	case json.Unmarshal(b, &body) != nil:
		s.fail(w, r, http.StatusBadRequest, "本文が JSON ではない")
		return
	}
	id := int64(atoi(r.PathValue("id")))
	reauthed := false
	if body.Password != "" {
		if !s.reauth(w, r, body.Password, "note:"+r.PathValue("id")) {
			return
		}
		reauthed = true
	}
	res, err := s.opts.Notes.Save(id, body.Base, body.Body, reauthed)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, r, http.StatusNotFound, "そのノートは Vault に無い")
	case errors.Is(err, noteedit.ErrNeedReauth):
		s.fail(w, r, http.StatusForbidden, "エージェントの指示の紙なので、パスワードを入れ直さないと直せない")
	case errors.Is(err, noteedit.ErrReadOnly):
		s.fail(w, r, http.StatusForbidden, err.Error())
	case errors.Is(err, session.ErrNoAgent), errors.Is(err, session.ErrNoNoteVault), errors.Is(err, session.ErrOtherVault):
		// **書いた中身は画面に残る。** 実行面が戻れば、次の自動保存で書ける。
		s.fail(w, r, http.StatusServiceUnavailable, "いまは保存できない: "+err.Error())
	default:
		s.fail(w, r, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleNoteSync(w http.ResponseWriter, r *http.Request) {
	st, err := s.opts.Notes.Status()
	if err == nil && st != nil && s.sessions != nil {
		// 実行面が書けるか（書けなければ画面は保存しない旨を出す）。
		type out struct {
			*noteedit.Sync
			Vault   string `json:"vault,omitempty"`
			VaultID int64  `json:"vault_id,omitempty"` // 新しいノート・日次ログ・名前の一覧に使う
		}
		o := out{Sync: st, Vault: s.sessions.NoteVault()}
		if o.Vault != "" {
			o.VaultID, _ = vault.VaultByRoot(s.db, o.Vault)
		}
		s.respond(w, r, o, nil)
		return
	}
	s.respond(w, r, st, err)
}
