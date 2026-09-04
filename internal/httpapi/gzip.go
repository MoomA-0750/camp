package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ビューは1本で数MBのJSONになる（Health/テーブルは実測 4.0MB）。
// Camp は Tailscale 越しのモバイル回線から使う前提なので、そのまま
// 素で流すと待ち時間が長い。**そして長く待つほど、その間に別の
// リクエストと重なる確率が上がる。**
//
// 小さい応答は畳まない（畳むほうが大きくなる）。
const gzipMinBytes = 1400

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// gzipWriter は書き始めたバイト数を見てから畳むかどうかを決める。
// ヘッダを先に決め打つと、短い応答まで畳んでしまう。
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	buf     []byte
	code    int
	decided bool
	passed  bool
}

func (g *gzipWriter) WriteHeader(code int) {
	g.code = code
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if g.decided {
		if g.passed {
			return g.ResponseWriter.Write(b)
		}
		return g.gz.Write(b)
	}
	g.buf = append(g.buf, b...)
	if len(g.buf) < gzipMinBytes {
		return len(b), nil
	}
	g.decide(true)
	return len(b), nil
}

func (g *gzipWriter) decide(compress bool) {
	g.decided = true
	if !compress {
		g.passed = true
		g.flushHeader()
		g.ResponseWriter.Write(g.buf)
		return
	}
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.flushHeader()
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
	gz.Write(g.buf)
	g.buf = nil
}

func (g *gzipWriter) flushHeader() {
	if g.code == 0 {
		g.code = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.code)
}

// Flush は途中まで書いたぶんを送り出す。
//
// **SSE のような流し続ける応答は、これが無いと何も届かない。** そちらは
// そもそも畳まない経路へ回しているが、包みが Flusher を隠さないようにしておく。
func (g *gzipWriter) Flush() {
	if !g.decided {
		g.decide(false)
	}
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) close() {
	if !g.decided {
		g.decide(false)
	}
	if g.gz != nil {
		g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// withGzip は Accept-Encoding を見て応答を畳む。
func withGzip(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next(g, r)
	}
}
