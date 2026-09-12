package remoteingest

import (
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/store"
)

// RunOnce は向こうのホストの記録を1回ぶん取り込む（M47）。
//
// **失敗は静かに次回へ回す**が、跡は台帳に残す——何日も入っていないことに画面で
// 気づけるように（Fable の指摘6）。失敗のたびに監査へ残すと、寝ている携帯で
// 1日ぶんが積もる。
//
// 蓋（PerFileCap）は**要約の側に渡す**。運んだ量と、要約が止まる位置を揃えるため
// （蓋を書き込む側で切ると、位置と再開点が食い違って世代が進む）。
func RunOnce(db *store.DB, sup *session.Supervisor, agent, host string) (*ingest.Result, error) {
	col, ok := ingest.CollectorFor(agent)
	if !ok {
		return nil, fmt.Errorf("知らないエージェント: %s", agent)
	}
	req, err := sup.RecordReq(agent, host, col.RecordSub())
	if err != nil {
		// 台帳の行が無い・許していない・固定が無い。**跡は残すが、静かに戻る。**
		_ = session.MarkRecordFail(db, host, agent, err.Error())
		return nil, err
	}
	fset, err := (Files{Sup: sup, Req: req}).Open()
	if err != nil {
		_ = session.MarkRecordFail(db, host, agent, err.Error())
		return nil, err
	}
	defer fset.Close()

	res, err := ingest.IngestFrom(db, col, host, fset, PerFileCap)
	if err != nil {
		_ = session.MarkRecordFail(db, host, agent, err.Error())
		return nil, err
	}
	if err := session.MarkRecordOK(db, host, agent, time.Now()); err != nil {
		return res, err
	}
	return res, nil
}
