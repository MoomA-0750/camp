package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/views"
)

// 定義の読み書きと履歴（M50 の決定6、2026-09-13）。
//
// 定義を DB に持つと、**直す口が無ければ `sqlite3` を直打ちするしかない**。だから口を作った。
// ここで縛るのは、その口が「壊れたものを入れない」「直したら直ちに効く」「誰がいつ直したか
// 残る」こと。

const defYAML = "base: A\nviews:\n  - name: t\n    kind: table\n    shape:\n      order: [a]\n"

func getJSONAs(t *testing.T, c *http.Client, url string, v any) int {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	} else {
		io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode
}

func postDef(t *testing.T, c *http.Client, url, def string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"def": def})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// 定義が無ければ 404。**空の定義を返さない**——「無い」と「空」を混ぜると、
// 画面が「まだ変換していない」を「中身が空の定義がある」と読む。
func TestAMissingDefinitionIs404(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 5)
	c := loggedIn(t, srv)

	if code := getJSONAs(t, c, srv.URL+"/api/views/A/def", nil); code != http.StatusNotFound {
		t.Fatalf("定義が無いのに %d", code)
	}
}

// 書いて、読み戻せて、履歴に残る。**書いたら直ちに効く**（控えを捨てている）。
func TestADefinitionCanBeSavedAndTakesEffectAtOnce(t *testing.T) {
	srv, db := newServer(t)
	// `.base` 側のビュー名は t、独自定義側も t だが order が違う（どちらを見たか分かる）。
	seedVault(t, db, map[string]string{"A.base": okBase}, 5)
	c := loggedIn(t, srv)

	// 先に一覧を叩いて控えを温めておく（控えが残ると、書いても古い定義で描かれる）。
	var before []struct{ Base, Name string }
	if code := getJSONAs(t, c, srv.URL+"/api/views", &before); code != http.StatusOK {
		t.Fatalf("一覧が %d", code)
	}

	if code, body := postDef(t, c, srv.URL+"/api/views/A/def", defYAML); code != http.StatusOK {
		t.Fatalf("保存が %d: %s", code, body)
	}

	// 読み戻せる。
	var got views.Def
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/def", &got); code != http.StatusOK {
		t.Fatalf("読み戻しが %d", code)
	}
	if got.Base != "A" || got.Body != defYAML {
		t.Fatalf("読み戻した定義が違う: %+v", got)
	}
	if got.ConvertedAt != "" {
		t.Fatalf("手で書いたのに変換の時刻が入った: %q", got.ConvertedAt)
	}
	// **手編集かどうかはサーバーが決めて渡す**（画面が時刻で決め直すと食い違う）。
	var flags struct {
		HandEdited bool `json:"hand_edited"`
	}
	getJSONAs(t, c, srv.URL+"/api/views/A/def", &flags)
	if !flags.HandEdited {
		t.Fatal("最後に書いたのは人なのに hand_edited が立っていない")
	}

	// **控えを捨てているので、待たずに独自定義で描かれる。**
	var after []struct {
		Base   string `json:"base"`
		Name   string `json:"name"`
		FromDB bool   `json:"from_db"`
	}
	if code := getJSONAs(t, c, srv.URL+"/api/views", &after); code != http.StatusOK {
		t.Fatalf("一覧が %d", code)
	}
	var seen bool
	for _, v := range after {
		if v.Base == "A" {
			seen = true
			if !v.FromDB {
				t.Fatal("保存したのに `.base` を描き続けている（控えを捨てていない）")
			}
		}
	}
	if !seen {
		t.Fatalf("台紙 A が一覧から消えた: %+v", after)
	}

	// 履歴に1件、書き手は user。
	var hist []views.HistoryEntry
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/history", &hist); code != http.StatusOK {
		t.Fatalf("履歴が %d", code)
	}
	if len(hist) != 1 || hist[0].By != views.ByUser {
		t.Fatalf("履歴が残らない: %+v", hist)
	}
}

// **壊れた定義は入らない。** 入れてしまうと画面から直せなくなる。
func TestABrokenDefinitionIsRejectedByTheAPI(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 5)
	c := loggedIn(t, srv)

	for _, bad := range []string{
		"views: [",                           // YAML が壊れている
		"base: B\nviews: []\n",               // 台紙の名前が食い違う
		"base: A\nviews:\n  - kind: table\n", // ビューの name が無い
	} {
		code, _ := postDef(t, c, srv.URL+"/api/views/A/def", bad)
		if code != http.StatusBadRequest {
			t.Fatalf("壊れた定義を %d で通した: %q", code, bad)
		}
	}
	// 1つも入っていない。
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/def", nil); code != http.StatusNotFound {
		t.Fatalf("壊れた定義で行ができた: %d", code)
	}
}

// **`{base}/def` は `{id...}` より先に当たる。** 当たらないと、定義の口が
// 「Aというビューの def という名前のビュー」として 404 になる。
func TestTheDefinitionRouteWinsOverTheViewCatchAll(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 5)
	c := loggedIn(t, srv)

	if code, body := postDef(t, c, srv.URL+"/api/views/A/def", defYAML); code != http.StatusOK {
		t.Fatalf("定義の口に当たっていない: %d %s", code, body)
	}
	// ビューそのものは今までどおり開ける（`{id...}` が生きている）。
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/t", nil); code != http.StatusOK {
		t.Fatalf("ビューが開けない: %d", code)
	}
	// 履歴の口も同じく先に当たる。
	var hist []views.HistoryEntry
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/history", &hist); code != http.StatusOK {
		t.Fatalf("履歴の口に当たっていない: %d", code)
	}
}

// **チャートの定義は保存した直後に時系列として描かれ、壊れていれば入らない**（M51、2026-09-13）。
func TestAChartDefinitionComesBackAsASeries(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 5)
	c := loggedIn(t, srv)

	chart := "base: A\nviews:\n  - name: t\n    kind: chart\n    shape:\n" +
		"      time: {axis: file.mtime, bucket: day}\n      measures: {a: sum}\n" +
		"    emit:\n      human:\n        - {kind: chart, values: [a], chart: line, window: last-365-days}\n"
	if code, body := postDef(t, c, srv.URL+"/api/views/A/def", chart); code != http.StatusOK {
		t.Fatalf("保存が %d: %s", code, body)
	}
	var res views.Result
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/t", &res); code != http.StatusOK {
		t.Fatalf("ビューが %d", code)
	}
	if res.Emit == nil || len(res.Emit.Human) != 1 || res.Emit.Human[0].Window != "last-365-days" {
		t.Fatalf("描き方が画面に渡っていない: %+v", res.Emit)
	}
	if len(res.Series) != 1 || res.Series[0].Error != "" || len(res.Series[0].Points) != 5 ||
		res.Series[0].Points[4].T != "2026-01-05" || res.Series[0].Points[4].V != 4 {
		t.Fatalf("時系列が違う: %+v", res.Series)
	}

	broken := strings.Replace(chart, "bucket: day", "bucket: week", 1)
	if code, _ := postDef(t, c, srv.URL+"/api/views/A/def", broken); code != http.StatusBadRequest {
		t.Fatalf("知らない区切りの定義が %d で通った", code)
	}
}

// ノートから辿るグラフ（M52、2026-09-13）。既定は1歩。壊れた depth は 400、無いノートは 404。
func TestTheGraphEndpoint(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 4)
	// seedVault のノートは id 2〜5（1 は A.base）。2 → 3 → 4 と一列につなぐ。
	for _, q := range []string{
		`insert into note_links(from_note_id,raw_target,to_note_id,resolved) values (2,'1',3,1), (3,'2',4,1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c := loggedIn(t, srv)

	var g views.Graph
	if code := getJSONAs(t, c, srv.URL+"/api/graph?note=2", &g); code != http.StatusOK {
		t.Fatalf("グラフが %d", code)
	}
	if g.Center != 2 || g.Depth != 1 || len(g.Nodes) != 2 || g.Unlinked != 1 {
		t.Fatalf("既定の1歩になっていない: %+v", g)
	}
	if code := getJSONAs(t, c, srv.URL+"/api/graph?note=2&depth=2", &g); code != http.StatusOK || len(g.Nodes) != 3 {
		t.Fatalf("2歩: %d %+v", code, g)
	}
	if code := getJSONAs(t, c, srv.URL+"/api/graph", &g); code != http.StatusOK || g.Center != 3 {
		t.Fatalf("中心の既定はリンクの一番多いノート: %d %+v", code, g)
	}
	if code := getJSONAs(t, c, srv.URL+"/api/graph?note=2&depth=9", nil); code != http.StatusBadRequest {
		t.Fatalf("depth=9 が %d", code)
	}
	if code := getJSONAs(t, c, srv.URL+"/api/graph?note=2&depth=abc", nil); code != http.StatusBadRequest {
		t.Fatalf("depth=abc が %d（読めない値を「全部」にしてはいけない）", code)
	}
	if code := getJSONAs(t, c, srv.URL+"/api/graph?note=999", nil); code != http.StatusNotFound {
		t.Fatalf("無いノートが %d", code)
	}
}

// graph のビューは、結果に節と辺が付いて返る（M52）。行どうしのリンクだけを辺にする。
func TestAGraphViewComesBackWithNodesAndEdges(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 4)
	if _, err := db.Exec(`insert into note_links(from_note_id,raw_target,to_note_id,resolved) values (2,'1',3,1)`); err != nil {
		t.Fatal(err)
	}
	c := loggedIn(t, srv)
	def := "base: A\nviews:\n  - name: t\n    kind: graph\n    emit:\n      human:\n        - {kind: graph, colors: [{tag: x, color: \"#123456\"}]}\n"
	if code, body := postDef(t, c, srv.URL+"/api/views/A/def", def); code != http.StatusOK {
		t.Fatalf("保存が %d: %s", code, body)
	}
	var res views.Result
	if code := getJSONAs(t, c, srv.URL+"/api/views/A/t", &res); code != http.StatusOK {
		t.Fatalf("ビューが %d", code)
	}
	if res.Graph == nil || len(res.Graph.Nodes) != 2 || len(res.Graph.Edges) != 1 || res.Graph.Unlinked != 2 {
		t.Fatalf("節と辺が付いていない: %+v", res.Graph)
	}
}

// **読んだ版の上にしか書かない**（実装後レビュー、codex の指摘2）。GET が返す body_sha256 を持って来ない
// 保存、古い版の指紋での保存は 409。
func TestAStaleSaveIsAConflict(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 2)
	c := loggedIn(t, srv)
	if code, body := postDef(t, c, srv.URL+"/api/views/A/def", defYAML); code != http.StatusOK {
		t.Fatalf("最初の保存が %d: %s", code, body)
	}
	var got struct {
		Body       string `json:"body"`
		BodySHA256 string `json:"body_sha256"`
	}
	getJSONAs(t, c, srv.URL+"/api/views/A/def", &got)
	if got.BodySHA256 != views.SHA256([]byte(got.Body)) {
		t.Fatalf("body_sha256 が本文の指紋でない: %+v", got)
	}
	post := func(def, sha string) int {
		b, _ := json.Marshal(map[string]string{"def": def, "base_sha256": sha})
		res, err := c.Post(srv.URL+"/api/views/A/def", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	first := strings.Replace(defYAML, "order: [a]", "order: [a, b]", 1)
	if code := post(first, got.BodySHA256); code != http.StatusOK {
		t.Fatalf("読んだ版の上の保存が %d", code)
	}
	if code := post(strings.Replace(defYAML, "order: [a]", "order: [c]", 1), got.BodySHA256); code != http.StatusConflict {
		t.Fatalf("古い版の上の保存が %d（409 のはず）", code)
	}
	if code := post(first, ""); code != http.StatusConflict {
		t.Fatalf("版を持たない保存が %d（409 のはず）", code)
	}
}

// **定義がどこから書き換わっても、控えを待たずに効く**（実装後レビュー、codex の指摘8）。
// 画面の保存口を通らない書き換え（`campd views -convert`）でも、次の一覧で独自定義から描く。
func TestADefinitionWrittenOutsideTheAPITakesEffectAtOnce(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 3)
	c := loggedIn(t, srv)
	var before []struct {
		Base   string `json:"base"`
		FromDB bool   `json:"from_db"`
	}
	getJSONAs(t, c, srv.URL+"/api/views", &before) // 控えを温める
	if len(before) == 0 || before[0].FromDB {
		t.Fatalf("前提が違う: %+v", before)
	}
	// CLI の変換と同じ口で、API を通さずに書く。
	if err := views.SaveDef(db, 1, &views.Def{Base: "A", Body: defYAML}, views.ByConvert, false); err != nil {
		t.Fatal(err)
	}
	var after []struct {
		Base   string `json:"base"`
		FromDB bool   `json:"from_db"`
	}
	getJSONAs(t, c, srv.URL+"/api/views", &after)
	if len(after) == 0 || !after[0].FromDB {
		t.Fatalf("API の外で書いた定義が控えに隠れた: %+v", after)
	}
}
