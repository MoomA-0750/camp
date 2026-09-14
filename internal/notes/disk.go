package notes

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
)

// MaxLine は campd と実行面のあいだの1行の上限（session の maxLine と同じ値）。
// 本文は JSON の1行に乗るので、**JSON にした長さで**この下に収める（Fable の設計レビュー 7:
// 超えると実行面の読み手が落ち、制御口ごと切れる）。
const MaxLine = 1 << 20

// MaxBody は本文そのものの上限。JSON にすると伸びる（`"` や `\n` は2バイト）ので、
// 最後は呼ぶ側が JSON の長さで照らす。実データで最大の散文ノートは 556 KiB（JSON で 558 KiB）。
const MaxBody = 900 << 10

// Result は書いた結果。
type Result struct {
	// Status は written（書いた）・same（もう同じ中身だった。再送）・changed（書き始めたあとで
	// 変わっていた。書いていない）・exists（新しく作ろうとしたが既にある）。
	Status string `json:"status"`
	// DiskSHA は終わった時点のディスクの中身のハッシュ。changed なら今の中身。
	DiskSHA string `json:"disk_sha,omitempty"`
	// Other は exists のとき、大文字小文字・NFC だけが違う既にある名前（同じ名前なら空）。
	Other string `json:"other,omitempty"`
}

const (
	StatusWritten = "written"
	StatusSame    = "same"
	StatusChanged = "changed"
	StatusExists  = "exists"
)

// Disk は1つの Vault に書く者。**実行面が持つ。** Vault の場所は campd から受け取らない。
type Disk struct {
	root *os.Root
	dir  string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// OpenDisk は Vault を開く。
func OpenDisk(dir string) (*Disk, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Disk{root: r, dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

// Dir は開いた Vault の場所。
func (d *Disk) Dir() string { return d.dir }

// Close は閉じる。
func (d *Disk) Close() error { return d.root.Close() }

// lock は同じパスへの書き込みを直列にする（Fable の設計レビュー 10: 実行面は要求ごとに
// goroutine を起こすので、再送と本送が同時に走りうる）。
func (d *Disk) lock(rel string) func() {
	d.mu.Lock()
	m := d.locks[rel]
	if m == nil {
		m = &sync.Mutex{}
		d.locks[rel] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Read は今の中身を返す。無ければ fs.ErrNotExist。
func (d *Disk) Read(rel string) ([]byte, error) {
	if err := d.plainPath(rel); err != nil {
		return nil, err
	}
	return d.root.ReadFile(rel)
}

// plainPath は、途中にも最後にも symlink が無く、最後が（在れば）普通のファイルであることを見る。
//
// os.Root は Vault の外へ出ないことを保証するが、Vault の中の symlink は辿る。**辿った先の
// 別のノートを書き換えない**ために、書く口では symlink を一切通さない（今の Vault に 0 本）。
func (d *Disk) plainPath(rel string) error {
	if err := cleanRel(rel); err != nil {
		return err
	}
	segs := strings.Split(rel, "/")
	for i := range segs {
		p := strings.Join(segs[:i+1], "/")
		fi, err := d.root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) && i == len(segs)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s は symlink なので通らない", p)
		case i < len(segs)-1 && !fi.IsDir():
			return fmt.Errorf("%s はディレクトリでない", p)
		case i == len(segs)-1 && !fi.Mode().IsRegular():
			return fmt.Errorf("%s は普通のファイルでない", p)
		}
	}
	return nil
}

// Write は本文を書く。create なら「まだ無いこと」を条件に、そうでなければ「今の中身のハッシュが
// base であること」を条件にする。
//
// 書き方は、同じディレクトリの一時ファイル → fsync → 元の権限 → **直前にもう一度照らす** → rename。
// 直前の照合で狭められるのは Camp 側の数ミリ秒の隙だけで、エージェントの「読んでから書くまで」の
// 隙は閉じられない（設計書「守りの効き方」）。主な守りは commit の前の照合と blobs の控え。
//
// reauthed は「パスワードを入れ直した保存」。指示の紙（Instruction）はこれが無ければ書かない。
func (d *Disk) Write(rel, base string, body []byte, create, reauthed bool) (Result, error) {
	switch c, why := Classify(rel); {
	case c == ReadOnly:
		return Result{}, fmt.Errorf("%s は直せない: %s", rel, why)
	case c == Instruction && !reauthed:
		return Result{}, fmt.Errorf("%s はエージェントの指示の紙なので、パスワードを入れ直さないと直せない", rel)
	}
	if err := CheckText(body); err != nil {
		return Result{}, err
	}
	if len(body) > MaxBody {
		return Result{}, fmt.Errorf("本文が大きすぎる（%d バイト）", len(body))
	}
	if create {
		// 新しいノートは Editable の場所だけ（指示の紙は作らない）。名前も campd を信じずに照らす。
		if c, _ := Classify(rel); c != Editable {
			return Result{}, fmt.Errorf("%s には新しいノートを作らない", rel)
		}
		if n, err := NameRel(rel); err != nil {
			return Result{}, err
		} else if n != rel {
			return Result{}, fmt.Errorf("%s は NFC に揃っていない", rel)
		}
	}
	unlock := d.lock(rel)
	defer unlock()
	if err := d.plainPath(rel); err != nil {
		return Result{}, err
	}

	want := Sum(body)
	cur, mode, err := d.current(rel)
	if err != nil {
		return Result{}, err
	}
	if create && cur == "" {
		// **大文字小文字・NFC 違いの同じ名前が既にあれば作らない**（索引に無いファイルも見る）。
		if other, err := d.sameName(rel); err != nil {
			return Result{}, err
		} else if other != "" {
			return Result{Status: StatusExists, Other: other}, nil
		}
	}
	switch {
	case cur == want:
		// **冪等。** 書けたのに返事が届かず再送された（Fable の設計レビュー 9）。
		return Result{Status: StatusSame, DiskSHA: cur}, nil
	case create && cur != "":
		return Result{Status: StatusExists, DiskSHA: cur}, nil
	case !create && cur == "":
		return Result{}, fmt.Errorf("%s が無い（消された）", rel)
	case !create && cur != base:
		return Result{Status: StatusChanged, DiskSHA: cur}, nil
	}

	tmp, err := d.writeTemp(rel, body, mode)
	if err != nil {
		return Result{}, err
	}
	defer d.root.Remove(tmp) // rename できていれば「無い」で失敗するだけ

	// 直前にもう一度。
	if again, _, err := d.current(rel); err != nil {
		return Result{}, err
	} else if again != cur {
		return Result{Status: StatusChanged, DiskSHA: again}, nil
	}
	if create {
		// **rename は既にあるファイルを黙って上書きする。** 新しいノートは link で置き、
		// その間に誰かが作っていれば失敗させる。
		if err := d.root.Link(tmp, rel); err != nil {
			if errors.Is(err, fs.ErrExist) {
				now, _, _ := d.current(rel)
				return Result{Status: StatusExists, DiskSHA: now}, nil
			}
			return Result{}, err
		}
	} else if err := d.root.Rename(tmp, rel); err != nil {
		return Result{}, err
	}
	d.syncDir(path.Dir(rel))
	return Result{Status: StatusWritten, DiskSHA: want}, nil
}

// sameName は、rel と同じフォルダに、畳むと同じ名前になる別のファイル（やフォルダ）があればその名前を返す。
func (d *Disk) sameName(rel string) (string, error) {
	dir := path.Dir(rel)
	f, err := d.root.Open(dir)
	if err != nil {
		return "", err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return "", err
	}
	want := FoldName(path.Base(rel))
	for _, n := range names {
		if FoldName(n) == want && n != path.Base(rel) {
			return path.Join(dir, n), nil
		}
	}
	return "", nil
}

// TrashResult は `.trash/` へ移した結果。
type TrashResult struct {
	// Status は trashed（移した）・changed（見ていた版と違うので移していない）・gone（もう無い）・
	// kept（移したあとで違うと分かり、戻そうとしたが元の場所に誰かが作っていた。To に残した）。
	Status string `json:"status"`
	// To は `.trash/` の中の置き場所（trashed・kept）。
	To string `json:"to,omitempty"`
	// DiskSHA は移した（changed なら今の）中身のハッシュ。
	DiskSHA string `json:"disk_sha,omitempty"`
}

const (
	TrashDone    = "trashed"
	TrashChanged = "changed"
	TrashGone    = "gone"
	TrashKept    = "kept"
)

// afterTrashRename は試験で「照合と rename の間に書かれた」を作るための差し込み口。
var afterTrashRename func()

// TrashDir は Obsidian の「Vault のゴミ箱」。
const TrashDir = ".trash"

// Trash はノートを `.trash/` へ移す。**今の中身のハッシュが base のときだけ。**
//
// 手順（どこで割り込まれても中身はどこかに残る）:
//  1. `.trash/` の中の乱数の名前へ rename（行き先は誰も使わない名前なので、何も上書きしない）
//  2. 移した中身を照らす。違えば（照合と rename の間に書かれた）元の場所へ link で戻す。
//     戻す場所に誰かが作っていれば、移したものは `.trash/` に残して kept を返す
//  3. 合っていれば `.trash/<ベース名>`（在れば ` 1` ` 2`…）へ link で置き、乱数の名前を消す
func (d *Disk) Trash(rel, base string, reauthed bool) (TrashResult, error) {
	switch c, why := Classify(rel); {
	case c == ReadOnly:
		return TrashResult{}, fmt.Errorf("%s は直せない: %s", rel, why)
	case c == Instruction && !reauthed:
		return TrashResult{}, fmt.Errorf("%s はエージェントの指示の紙なので、パスワードを入れ直さないと移せない", rel)
	}
	unlock := d.lock(rel)
	defer unlock()
	if err := d.plainPath(rel); err != nil {
		return TrashResult{}, err
	}
	cur, _, err := d.current(rel)
	switch {
	case err != nil:
		return TrashResult{}, err
	case cur == "":
		return TrashResult{Status: TrashGone}, nil
	case cur != base:
		return TrashResult{Status: TrashChanged, DiskSHA: cur}, nil
	}
	if err := d.trashDir(); err != nil {
		return TrashResult{}, err
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return TrashResult{}, err
	}
	tmp := path.Join(TrashDir, ".camp-trash-"+hex.EncodeToString(rnd[:])+".tmp")
	if err := d.root.Rename(rel, tmp); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TrashResult{Status: TrashGone}, nil
		}
		return TrashResult{}, err
	}
	d.syncDir(path.Dir(rel))
	if afterTrashRename != nil {
		afterTrashRename()
	}
	moved, err := d.root.ReadFile(tmp)
	if err != nil {
		// 読めなくても消してはいない。乱数の名前のまま `.trash/` に残る。
		return TrashResult{Status: TrashKept, To: tmp}, nil
	}
	if got := Sum(moved); got != base {
		if err := d.root.Link(tmp, rel); err != nil {
			return TrashResult{Status: TrashKept, To: tmp, DiskSHA: got}, nil
		}
		d.root.Remove(tmp)
		d.syncDir(path.Dir(rel))
		return TrashResult{Status: TrashChanged, DiskSHA: got}, nil
	}
	to, err := d.placeInTrash(tmp, path.Base(rel))
	if err != nil {
		return TrashResult{Status: TrashDone, To: tmp, DiskSHA: base}, nil // 乱数の名前のまま `.trash/` にある
	}
	return TrashResult{Status: TrashDone, To: to, DiskSHA: base}, nil
}

// trashDir は `.trash/` が普通のフォルダであることを確かめ、無ければ作る。
func (d *Disk) trashDir() error {
	fi, err := d.root.Lstat(TrashDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := d.root.Mkdir(TrashDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		fi, err = d.root.Lstat(TrashDir)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s が普通のフォルダでない", TrashDir)
	}
	return nil
}

// placeInTrash は乱数の名前のファイルを `.trash/<name>`（在れば ` 1` ` 2`…）へ link で置き、乱数の名前を消す。
func (d *Disk) placeInTrash(tmp, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s %d%s", stem, i, ext)
		}
		to := path.Join(TrashDir, n)
		err := d.root.Link(tmp, to)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		d.root.Remove(tmp)
		d.syncDir(TrashDir)
		return to, nil
	}
	return "", fmt.Errorf("%s に置く名前が見つからない", TrashDir)
}

// current は今の中身のハッシュと権限。無ければ ""。
func (d *Disk) current(rel string) (string, fs.FileMode, error) {
	b, err := d.root.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0o644, nil
	}
	if err != nil {
		return "", 0, err
	}
	fi, err := d.root.Lstat(rel)
	if err != nil {
		return "", 0, err
	}
	return Sum(b), fi.Mode().Perm(), nil
}

func (d *Disk) writeTemp(rel string, body []byte, mode fs.FileMode) (string, error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	// ドット始まりにする。索引は降りないし、Classify も書き先として通さない。
	tmp := path.Join(path.Dir(rel), ".camp-write-"+hex.EncodeToString(rnd[:])+".tmp")
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(body)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		d.root.Remove(tmp)
		return "", err
	}
	// umask で削られた分を戻す（元のファイルの権限を写す）。
	if err := d.root.Chmod(tmp, mode); err != nil {
		d.root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func (d *Disk) syncDir(dir string) {
	if f, err := d.root.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}
