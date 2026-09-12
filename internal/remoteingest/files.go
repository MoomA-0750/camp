// Package remoteingest は向こうのホストの記録を読む「読み手」（M47）。
//
// **ここが `internal/session` と `internal/ingest` を繋ぐ唯一の場所。** 取り込みは
// セッションの仕組みを知らず、セッションは取り込みを知らない（どちらも相手を import
// しない）。両方を知るのはこの層だけにして、向きを一方通行に保つ。
//
// 読むのは実行面経由。campd は ssh しない（D-025）。
package remoteingest

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/session"
)

const (
	// ChunkBytes は1回の頼みで運ぶ量の目安。実行面の側にも蓋がある。
	ChunkBytes = 256 << 10
	// TotalCap は1回の取り込みで運ぶ合計の上限。**携帯の回線と電池のため。**
	// 足りなければ次回に続きから読む（ingested_offset があるので安全）。
	TotalCap = 8 << 20
	// PerFileCap は1本あたりの上限。**取り込みに渡す蓋と同じ値にする**——
	// 要約が止まる位置と、運んだ量を揃えるため（蓋は要約の側で切る）。
	PerFileCap = 4 << 20
)

// Files は向こうのホストの記録を読む ingest.Files。
type Files struct {
	Sup *session.Supervisor
	Req session.RecReq
}

var _ ingest.Files = Files{}

func (f Files) Open() (ingest.FileSet, error) {
	return &set{sup: f.Sup, req: f.Req,
		at: map[string]int64{}, base: map[string]int64{},
		win: map[string][]byte{}, body: map[string][]byte{}}, nil
}

// set は1回ぶんの読み口。**一覧のときに要る中身まで取り寄せて、あとは手元で配る。**
//
// 台帳のトランザクションの中で回線を待たないため（store は接続を1本しか持たない）。
type set struct {
	sup *session.Supervisor
	req session.RecReq

	root string
	// at は前回位置、win はその直前 ResumeWindow バイト（向こうが返したもの）。
	at  map[string]int64
	win map[string][]byte
	// base は取り寄せた中身の開始位置、body はその中身。
	base map[string]int64
	body map[string][]byte
}

// List は一覧と窓を1回で取り、そのまま続きの中身も取り寄せる。
func (s *set) List(at map[string]int64) (*ingest.Listing, error) {
	lst, err := s.sup.RecList(s.req, at, ingest.ResumeWindow)
	if err != nil {
		return nil, err
	}
	s.root = lst.Root
	for p, b := range lst.Windows {
		s.win[p] = b
	}
	for p, o := range at {
		s.at[p] = o
	}

	out := &ingest.Listing{Root: lst.Root}
	var want []session.RecRange
	var total int64
	for _, fi := range lst.Files {
		off := at[fi.Path]
		if off > fi.Size {
			off = 0 // 切り詰められている。先頭から読み直す
		}
		if n := fi.Size - off; n > 0 {
			if n > PerFileCap {
				n = PerFileCap
			}
			if total+n > TotalCap {
				n = TotalCap - total
			}
			if n <= 0 {
				// 合計の蓋。**一覧には載せない**——中身を持たないファイルを走査へ渡すと、
				// そこで落ちて DB へ書く前に終わり、次回も同じところで落ちる（永久に進まない）。
				// **消えた扱いにもしない**ので、名前だけ伝える。
				out.Deferred = append(out.Deferred, fi.Path)
				continue
			}
			want = append(want, session.RecRange{Path: fi.Path, Off: off, N: n})
			s.base[fi.Path] = off
			total += n
		}
		out.Files = append(out.Files, ingest.FileInfo{
			Path:  fi.Path,
			Size:  fi.Size,
			MTime: time.Unix(fi.MTime, 0).UTC(),
		})
	}
	if err := s.fetch(want); err != nil {
		return nil, err
	}
	return out, nil
}

// fetch は範囲をまとめて取り寄せる。**束にして頼む**——1本ずつ繋ぎ直さないため。
func (s *set) fetch(want []session.RecRange) error {
	left := want
	for len(left) > 0 {
		var batch []session.RecRange
		var budget int64
		for len(left) > 0 && budget < ChunkBytes {
			r := left[0]
			left = left[1:]
			n := r.N
			if n > ChunkBytes-budget {
				n = ChunkBytes - budget
			}
			batch = append(batch, session.RecRange{Path: r.Path, Off: r.Off, N: n})
			budget += n
			if n < r.N {
				// 残りは次の束で。**位置を進めて頼み直す。**
				left = append([]session.RecRange{{Path: r.Path, Off: r.Off + n, N: r.N - n}}, left...)
			}
		}
		got, _, err := s.sup.RecRead(s.req, batch, budget)
		if err != nil {
			return err
		}
		for _, r := range batch {
			b := got[r.Path]
			if len(b) == 0 {
				continue
			}
			s.body[r.Path] = append(s.body[r.Path], b...)
		}
	}
	return nil
}

// Window は再開点のハッシュ。**手元と同じ関数に通す**——向こうで計算させない。
//
// 頼んだ位置（前回位置）の窓は向こうが返している。要約が読み終えた位置の窓は、
// **前回位置の直前の窓と、取り寄せた中身をつなげて**切り出す（そのために追加で繋がない）。
func (s *set) Window(path string, at int64) string {
	if at <= 0 {
		return ""
	}
	pre := s.win[path]
	base, okBase := s.base[path]
	if !okBase {
		base = s.at[path]
	}
	body := s.body[path]

	lo := base - int64(len(pre)) // つなげた中身の先頭が指す位置
	hi := base + int64(len(body))
	start := at - ingest.ResumeWindow
	if start < 0 {
		start = 0
	}
	if start < lo || at > hi {
		return "" // 手元に無い。読めないときと同じ扱い（rotatedFrom の判断材料）
	}
	buf := make([]byte, 0, at-start)
	for i := start; i < at; i++ {
		if i < base {
			buf = append(buf, pre[i-lo])
		} else {
			buf = append(buf, body[i-base])
		}
	}
	return ingest.WindowSHA(buf)
}

// Prefetch は何もしない。**一覧のときに取り寄せ終えている。**
func (s *set) Prefetch([]ingest.Range) error { return nil }

// Reader は取り寄せた中身から配る。**ここで回線を待たない。**
func (s *set) Reader(path string, off, stop int64) (io.ReadCloser, error) {
	base, ok := s.base[path]
	if !ok {
		return nil, fmt.Errorf("%s は取り寄せていない", path)
	}
	body := s.body[path]
	if off < base || off > base+int64(len(body)) {
		return nil, fmt.Errorf("%s の %d は取り寄せた範囲の外", path, off)
	}
	b := body[off-base:]
	if stop > 0 && stop-off < int64(len(b)) {
		b = b[:stop-off]
	}
	return nopCloser{bytes.NewReader(b)}, nil
}

func (s *set) Close() error { return nil }

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
