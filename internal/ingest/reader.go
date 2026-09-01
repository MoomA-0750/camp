package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrPartialLine は最後の行が改行で終わっていないことを示す。
// 稼働中のファイルをフラッシュ途中で読むと起きる。呼び出し側は
// このバイト列を次回に持ち越し、オフセットを進めてはいけない。
var ErrPartialLine = errors.New("ingest: 改行で終わっていない行")

// Reader は JSONL を1行ずつ読む。
// bufio.Scanner ではなく Reader を使うのは、tool_result が
// 数MBに達することがあり Scanner の上限に当たるため。
type Reader struct {
	br     *bufio.Reader
	offset int64

	// Pending は末尾の未完了バイト列。EOF に達した時のみ埋まる。
	Pending []byte
}

func NewReader(r io.Reader, startOffset int64) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 1<<20), offset: startOffset}
}

// Offset は次に読む行の先頭バイト位置。
// 完了した行だけを消費した後の値なので、そのまま ingested_offset にできる。
func (r *Reader) Offset() int64 { return r.offset }

// Next は次の行を返す。空行は読み飛ばす。
// 末尾が改行で終わっていない場合、そのバイト列を Pending に入れて io.EOF を返す。
func (r *Reader) Next() (raw []byte, offset int64, err error) {
	for {
		start := r.offset
		line, err := r.br.ReadBytes('\n')

		if err == io.EOF {
			if len(line) > 0 {
				// 改行が来ていない = 書き込み途中。消費しない。
				r.Pending = line
			}
			return nil, 0, io.EOF
		}
		if err != nil {
			return nil, 0, err
		}

		r.offset += int64(len(line))
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(trimmed)) == 0 {
			continue // 空行
		}
		return trimmed, start, nil
	}
}

// ParseLine は1行を Line に落とす。Raw と Offset も埋める。
//
// 厳密なデコードに失敗しても行を捨てない。最低限の共通フィールドだけを
// 拾い直し、Degraded を立てて返す。「独立保持」を謳う以上、形が想定外
// だからという理由で記録を失ってはいけない。
func ParseLine(raw []byte, offset int64) (*Line, error) {
	var l Line
	err := json.Unmarshal(raw, &l)
	if err == nil {
		l.Raw = raw
		l.Offset = offset
		return &l, nil
	}

	var loose struct {
		Type      string `json:"type"`
		UUID      string `json:"uuid"`
		SessionID string `json:"sessionId"`
		RunID     string `json:"session_id"`
		Timestamp string `json:"timestamp"`
	}
	if err2 := json.Unmarshal(raw, &loose); err2 != nil {
		// JSON ですらない。ここまで来たら本当に壊れている。
		return nil, fmt.Errorf("offset %d: %w", offset, err2)
	}

	return &Line{
		Type:       loose.Type,
		UUID:       loose.UUID,
		SessionID:  loose.SessionID,
		RunID:      loose.RunID,
		Timestamp:  loose.Timestamp,
		Raw:        raw,
		Offset:     offset,
		Degraded:   true,
		DegradedBy: err.Error(),
	}, nil
}

// WalkResult は1ファイルを読み切った結果。
type WalkResult struct {
	Pending   []byte  // 末尾の未完了バイト列。オフセットを進めてはいけない
	EndOffset int64   // 完了した行だけを消費した後の位置
	Broken    []error // JSON として読めなかった行。読み取りは止めない
}

// WalkFile はファイルを先頭から読み、各行に fn を適用する。
func WalkFile(path string, fn func(*Line) error) (WalkResult, error) {
	return WalkFileFrom(path, 0, fn)
}

// WalkFileFrom は start バイト目から読む。差分追尾で使う。
//
// start は「完了した行だけを消費した後の位置」なので、前回の走査で
// 未完了だった末尾は自然に読み直される。pending_tail は診断用であって
// 再開に必須ではない。
//
// 1行が壊れていてもファイル全体の読み取りは止めない。壊れた行は
// Broken に積んで先へ進む。fn がエラーを返した場合だけ中断する。
func WalkFileFrom(path string, start int64, fn func(*Line) error) (WalkResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return WalkResult{}, err
	}
	defer f.Close()

	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return WalkResult{}, err
		}
	}

	var res WalkResult
	rd := NewReader(f, start)
	for {
		raw, off, err := rd.Next()
		if err == io.EOF {
			res.Pending = rd.Pending
			res.EndOffset = rd.Offset()
			return res, nil
		}
		if err != nil {
			res.EndOffset = rd.Offset()
			return res, err
		}
		line, err := ParseLine(raw, off)
		if err != nil {
			res.Broken = append(res.Broken, err)
			continue
		}
		if err := fn(line); err != nil {
			res.EndOffset = rd.Offset()
			return res, err
		}
	}
}
