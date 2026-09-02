package vault

import (
	"strings"
	"testing"
)

func targets(ls []Link) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Target)
	}
	return out
}

// `[[ ]]` は bash の test 構文と衝突する。実測で素朴に数えた703本のうち40本が
// コードの中身だった。ここが崩れると、リンクでないものを6%リンクとして数える。
func TestCodeIsNotLinked(t *testing.T) {
	body := "本文の [[本物]] は拾う\n" +
		"```bash\n" +
		"if [[ -n \"$var\" ]]; then\n" +
		"  [[ -z \"$TMUX\" ]] && tmux\n" +
		"fi\n" +
		"```\n" +
		"インラインの `[[wikilink]]` も拾わない\n" +
		"~~~sh\n[[ -f x ]]\n~~~\n" +
		"閉じたあとの [[もう一つ]] は拾う\n"
	got := targets(ExtractLinks([]byte(body)))
	want := []string{"本物", "もう一つ"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// frontmatter は除かない。このVaultは source: に [[..]] を書いている。
// 飛ばすと Data/Todo の相互リンクが丸ごと落ちる。
func TestFrontmatterLinksAreKept(t *testing.T) {
	body := "---\ncompleted: false\nsource:\n  - \"[[Obsidianスーパーアプリ化]]\"\n---\n\n本文\n"
	got := targets(ExtractLinks([]byte(body)))
	if len(got) != 1 || got[0] != "Obsidianスーパーアプリ化" {
		t.Fatalf("frontmatter のリンクが落ちている: %v", got)
	}
}

// 改行を跨がない。跨ぐと表の1マスから遥か下までを1つのターゲットとして飲む。
func TestLinksDoNotCrossNewlines(t *testing.T) {
	body := "| [[A`） | zsh/zle。桁数計算が壊れる |\n\n……ずっと下……\n\n本文 [[実在]] 終わり\n"
	got := targets(ExtractLinks([]byte(body)))
	for _, g := range got {
		if strings.Contains(g, "\n") || len(g) > 100 {
			t.Fatalf("改行を飲み込んでいる: %q", g)
		}
	}
}

// [[target|alias]] [[target#見出し]] ![[target]] を分解する。
func TestLinkShapesAreSplit(t *testing.T) {
	ls := ExtractLinks([]byte(
		"[[Human/Logs/2026-07-02|2026-07-02]]\n![[Homelab.base#すべて]]\n[[ノート^blk]]\n"))
	if len(ls) != 3 {
		t.Fatalf("3本のはずが %d: %+v", len(ls), ls)
	}
	if ls[0].Target != "Human/Logs/2026-07-02" || ls[0].Alias != "2026-07-02" {
		t.Errorf("エイリアスを分解できていない: %+v", ls[0])
	}
	if !ls[1].Embed || ls[1].Target != "Homelab.base" || ls[1].Frag != "#すべて" {
		t.Errorf("埋め込み・見出しを分解できていない: %+v", ls[1])
	}
	if ls[2].Target != "ノート" || ls[2].Frag != "^blk" {
		t.Errorf("ブロック参照を分解できていない: %+v", ls[2])
	}
}

// [[#見出し]] は「このノートのこの見出し」。Obsidian では有効なリンクなので
// 捨てない（実測4本、すべて AI/Context/body.md）。
func TestSelfHeadingLinkIsKept(t *testing.T) {
	ls := ExtractLinks([]byte("……([[#年別トレンド(Apple Health)|年別トレンド]]参照)……\n"))
	if len(ls) != 1 {
		t.Fatalf("1本のはずが %d", len(ls))
	}
	if !ls[0].SelfFrag {
		t.Errorf("自ノート内リンクと判定できていない: %+v", ls[0])
	}
	if ls[0].Alias != "年別トレンド" {
		t.Errorf("エイリアスが取れていない: %+v", ls[0])
	}
}

// --- 解決 ---

func idx() *LinkIndex {
	return NewLinkIndex([]string{
		"Human/Logs/2026-07-11.md",
		"Data/Health/2026-07-11.md",
		"Human/Projects/Camp.md",
		"Human/Dashboards/To-Do.base",
		"attachments/SerEng_ iSCSI.pdf",
		"Human/Logs/一意.md",
	})
}

func TestResolveByBasename(t *testing.T) {
	r := idx().Resolve("AI/Profile/x.md", "一意")
	if r.To != "Human/Logs/一意.md" || r.Ambiguous {
		t.Fatalf("%+v", r)
	}
}

// リンク先は .md に限らない。.base や .pdf も解決できないと宙吊りに化ける。
func TestResolveNonMarkdown(t *testing.T) {
	ix := idx()
	if r := ix.Resolve("Home.md", "To-Do.base"); r.To != "Human/Dashboards/To-Do.base" {
		t.Errorf(".base を解決できていない: %+v", r)
	}
	if r := ix.Resolve("Home.md", "SerEng_ iSCSI.pdf"); r.To != "attachments/SerEng_ iSCSI.pdf" {
		t.Errorf(".pdf を解決できていない: %+v", r)
	}
}

func TestResolveByPath(t *testing.T) {
	r := idx().Resolve("Home.md", "Human/Logs/2026-07-11")
	if r.To != "Human/Logs/2026-07-11.md" || r.Ambiguous {
		t.Fatalf("パス指定で一意に決まるはず: %+v", r)
	}
}

// 候補が複数残ったら**黙って1つ選ばない**。選んだ先は返すが、曖昧だったことと
// 候補を必ず残す。Obsidian の規則を突き合わせて検証する手段が今は無いので、
// 一致したふりをしない。
func TestAmbiguityIsRecordedNotHidden(t *testing.T) {
	r := idx().Resolve("AI/Profile/価値観.md", "2026-07-11")
	if !r.Ambiguous {
		t.Fatal("曖昧だと記録していない")
	}
	if len(r.Candidates) != 2 {
		t.Fatalf("候補を2つ残すはず: %+v", r.Candidates)
	}
	if r.To == "" {
		t.Error("曖昧でも1つは選ぶ（バックリンクが繋がらなくなるので）")
	}
}

// 同じフォルダにあるものが最優先。ここで決まれば曖昧ではない。
func TestSameFolderWinsAndIsNotAmbiguous(t *testing.T) {
	r := idx().Resolve("Human/Logs/別の日.md", "2026-07-11")
	if r.To != "Human/Logs/2026-07-11.md" {
		t.Fatalf("同じフォルダを選んでいない: %+v", r)
	}
	if r.Ambiguous {
		t.Error("同じフォルダで決まったのに曖昧と言っている")
	}
}

func TestDanglingIsEmpty(t *testing.T) {
	r := idx().Resolve("Home.md", "存在しないノート")
	if r.To != "" {
		t.Fatalf("宙吊りのはず: %+v", r)
	}
}

// 大文字小文字を畳まない。
func TestResolveIsCaseSensitive(t *testing.T) {
	ix := NewLinkIndex([]string{"Human/Projects/Camp.md"})
	if r := ix.Resolve("Home.md", "camp"); r.To != "" {
		t.Fatalf("小文字で当ててしまっている: %+v", r)
	}
	if r := ix.Resolve("Home.md", "Camp"); r.To == "" {
		t.Fatal("正しい表記で当たらない")
	}
}
