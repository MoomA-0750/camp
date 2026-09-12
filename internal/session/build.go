package session

import "runtime/debug"

// この実行ファイルの指紋と、実行面が同じものかの判定（2026-09-12）。
//
// **なぜ版の文字列では足りないか**: `Version` は `-ldflags` で埋める前提の `"dev"` 固定で、
// しかも campd と実行面は**同じバイナリ**。古い実行面も新しい campd も同じ文字列を名乗るので、
// 照らしても何も分からない。
//
// 実際に起きた取り違え（2026-09-12）: unit を入れ替えて実行面を再起動したあとに campd の
// バイナリを入れ替えたので、**実行面だけ古い実体を掴んだまま**動き続けた。向こうのホストで
// sh を走らせるのは実行面なので、campd を新しくしても記録が読めなかった。**画面には何も出なかった。**

// selfBuild はこの実行ファイルの指紋。Go がビルド時に埋める VCS の情報から作る
// （`-buildvcs=auto` の既定。Makefile に手を入れなくても入っている）。
//
// **取れないことがある**: VCS の外でビルドしたとき、`go test` の試験用バイナリ、
// `go run`。そのときは空を返し、呼ぶ側は「分からない」として扱う。
//
// **限界**: 作業木が汚れたままのビルドは `+dirty` が付くだけで、汚れ方の違いは見分けられない。
// 2つの違う dirty ビルドは同じ指紋になる。
func selfBuild() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}

// buildMismatch は実行面が campd と違うビルドかを返す。
//
//   - 自分の指紋が取れない（mine が空）: **何も言わない。** 分からないのに警告しない
//   - 相手が名乗らない（theirs が空）: **古い。** 名乗る仕組みより前のバイナリだから
//   - 両方あって違う: 古い（か、少なくとも食い違っている）
func buildMismatch(mine, theirs string) bool {
	if mine == "" {
		return false
	}
	return theirs != mine
}
