package ingest

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Files は取り込みが読むファイルの出どころ。手元は os、向こうのホストは ssh（M47）。
//
// **1回の取り込みで開くのは1つ。** 呼ぶたびに繋ぎ直すと、変わっていない記録 40 本でも
// 40 回 ssh することになる。しかも台帳のトランザクションを握ったまま回線を待つと、
// store は接続を1本しか持たないので、その間 campd の API も承認も止まる。
type Files interface {
	Open() (FileSet, error)
}

// FileSet は1回ぶんの読み口。使い終わったら Close する。
type FileSet interface {
	// List は置き場の一覧を返す。
	//
	// at は「このファイルのこの位置の再開点も、ついでに欲しい」。
	// **向こうのホストでは一覧と一緒に1回で取る**——同じ実体かを見るためだけに
	// ファイルの数だけ繋ぎ直さないため。手元では Windows を空で返してよく、
	// そのとき呼び出し側は Window で1本ずつ聞く。
	List(at map[string]int64) (*Listing, error)

	// Window は at の直前 resumeWindow バイトのハッシュ。読めなければ空文字。
	// **エラーを返さない**——「読めない」は rotatedFrom の判断材料であって、
	// 取り込み全体を落とす理由ではない（いまの resumeSHA と同じ約束）。
	Window(path string, at int64) string

	// Prefetch は次に読む範囲をまとめて取り寄せる。**手元では何もしない。**
	// 向こうのホストではここで一度に運び、Reader はその写しを返す。
	Prefetch(want []Range) error

	// Reader は path の off から読む。stop は終点（0 は最後まで）で、
	// **手元では使わない**（os のファイルは呼び出し側が終点で止める）。
	// 向こうのホストでは、取り寄せる範囲を決めるのに要る。
	Reader(path string, off, stop int64) (io.ReadCloser, error)

	Close() error
}

// Listing は1回の一覧。
type Listing struct {
	// Root は**解決された置き場の実パス**。向こうのホストでは $HOME や環境変数から
	// 向こうが決めるので、campd は頼む時点では知らない。
	Root  string
	Files []FileInfo

	// Unreadable は権限で開けなかったもの。**黙って落とさず、数えて返す。**
	Unreadable []string

	// Deferred は「そこに在るが、今回は運ばなかった」ファイル（1回に運ぶ量の蓋に達した）。
	//
	// **一覧（Files）には入れない**——読めないものを走査に渡すと、そのファイルで落ちて
	// 取り込み全体が DB へ書く前に終わり、次回も同じところで落ちる。
	// **同時に「消えた」扱いにもしない**——在ることは分かっているので、印を付けたら嘘になる。
	Deferred []string

	// Windows は List の at に対して取れた再開点。取れなかったものは入れない。
	Windows map[string]string
}

// FileInfo は一覧の1件。
//
// Dev・Inode は**同じ実体かの判定には使わない**（2026-09-03 の実測で、再起動で
// dev が変わっただけで 72 ファイル全部が「別の実体」と判定された）。台帳には
// 今までどおり残すので持ち回るだけで、向こうのホストでは 0 になる。
type FileInfo struct {
	Path  string
	Size  int64
	MTime time.Time
	Dev   int64
	Inode int64
}

// Range は取り寄せる範囲。Stop 0 は「そのファイルの最後まで」。
type Range struct {
	Path string
	Off  int64
	Stop int64
}

// LocalFiles は手元のファイル。**既定はこれ**で、いままでの挙動と同じ。
type LocalFiles struct{ Root string }

func (l LocalFiles) Open() (FileSet, error) { return &localSet{root: l.Root}, nil }

type localSet struct{ root string }

// List は root 以下を歩く。**1本読めないだけで全部を落とさない。**
//
// M25.5 で campd は専用ユーザーになり、会話記録は ACL で読ませている。
// ACL は持ち主の権限で定期的に配り直すので、**新しいセッションのファイルは、
// 次の配り直しまで camp から読めない**。そこで walk ごと落としていたため、
// 1本の新しいファイルが取り込み全体を止めていた（2026-09-04 の outer gate で再現）。
// しかも失敗は journal にしか出ないので、**黙って止まる。**
func (s *localSet) List(at map[string]int64) (*Listing, error) {
	lst := &Listing{Root: s.root}
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				lst.Unreadable = append(lst.Unreadable, path)
				return nil
			}
			if os.IsNotExist(err) {
				return nil // 歩いている最中に消えた
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			if os.IsPermission(err) {
				lst.Unreadable = append(lst.Unreadable, path)
				return nil
			}
			return err
		}
		fi := FileInfo{Path: path, Size: info.Size(), MTime: info.ModTime().UTC()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			fi.Dev, fi.Inode = int64(st.Dev), int64(st.Ino)
		}
		lst.Files = append(lst.Files, fi)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lst, nil
}

// Window は手元ではその場で読む。**一覧と一緒に取る意味があるのは向こうのホストだけ。**
func (s *localSet) Window(path string, at int64) string { return resumeSHA(path, at) }

// Prefetch は手元では何もしない。ファイルはそこにある。
func (s *localSet) Prefetch([]Range) error { return nil }

func (s *localSet) Reader(path string, off, _ int64) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (s *localSet) Close() error { return nil }
