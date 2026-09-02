// Package vault は Obsidian Vault を読む。書かない。
package vault

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind はノートの種類。本文を読むのは markdown だけだが、
// .base や .pdf もリンク先になるので行そのものは持つ。
const (
	KindMarkdown = "markdown"
	KindBase     = "base"   // Obsidian Bases のビュー定義
	KindCanvas   = "canvas" // JSON Canvas
	KindAsset    = "asset"  // 画像・PDF など
	KindOther    = "other"
)

// File は Vault 内の1ファイル。
type File struct {
	Rel   string // Vault ルートからの相対パス。大文字小文字を畳まない
	Kind  string
	Ext   string // 小文字化した拡張子（"." 込み）。種別判定用で、パスには使わない
	Size  int64
	MTime string
}

// Result は1回の走査。
type Result struct {
	Root    string
	Files   []File
	Pruned  []string // 降りずに切ったディレクトリ（相対パス）
	Skipped int      // シンボリックリンク・読めなかったもの
	Bytes   int64
	ByKind  map[string]int
}

// kindOf は拡張子から種別を決める。判定だけ小文字化する。
func kindOf(ext string) string {
	switch ext {
	case ".md", ".markdown":
		return KindMarkdown
	case ".base":
		return KindBase
	case ".canvas":
		return KindCanvas
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".pdf",
		".mp3", ".mp4", ".wav", ".m4a", ".ogg", ".webm", ".flac":
		return KindAsset
	}
	return KindOther
}

// Scan は Vault を歩いて中身の一覧を作る。DBには何も書かない。
//
// 隠しディレクトリは**降りる前に切る**。`.claude/worktrees/` にはこの Vault の
// 11倍のファイルがあり（実測46,366本・442MB）、「歩いてから捨てる」形にすると
// 索引のたびにそこを全部読むことになる。Obsidian 自身もドット始まりの
// ディレクトリは見ないので、規則としても一致する。
func Scan(root string) (*Result, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	res := &Result{Root: abs, ByKind: map[string]int{}}

	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 読めないものは黙って飛ばす。1ファイルで索引全体を落とさない。
			res.Skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(abs, p)
		if rerr != nil {
			res.Skipped++
			return nil
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				res.Pruned = append(res.Pruned, rel)
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// シンボリックリンクは辿らない。Vault の外へ出るし、輪を作れる。
			res.Skipped++
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			res.Skipped++
			return nil
		}
		ext := strings.ToLower(filepath.Ext(rel))
		k := kindOf(ext)
		res.Files = append(res.Files, File{
			Rel: filepath.ToSlash(rel), Kind: k, Ext: ext,
			Size: info.Size(), MTime: info.ModTime().UTC().Format(timeFmt),
		})
		res.Bytes += info.Size()
		res.ByKind[k]++
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Rel < res.Files[j].Rel })
	sort.Strings(res.Pruned)
	return res, nil
}

// Read はノートの中身を返す。呼ぶのは markdown と base だけにする。
func Read(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}

const timeFmt = "2006-01-02T15:04:05Z"
