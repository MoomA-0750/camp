// Package web はフロントのビルド成果物を埋め込む。
//
// dist は git に置かない（ビルドのたびに中身が変わるため）。ただし
// //go:embed はディレクトリが空だとコンパイルを通さないので、
// .gitkeep だけ置いてある。**index.html が無ければ埋め込みは無効**として
// 扱い、httpapi は組み込みの仮の殻に落ちる。
//
// つまり node を持たないところでも campd はビルドでき、
// npm run build を通したところでは単一バイナリに画面ごと入る。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist は dist/ の中身。画面が入っていなければ ok が false。
func Dist() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	f, err := sub.Open("index.html")
	if err != nil {
		return nil, false
	}
	f.Close()
	return sub, true
}
