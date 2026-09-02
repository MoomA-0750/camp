// Package mcp は Camp を MCP サーバーとして開く。stdio の JSON-RPC 2.0。
//
// **読み取り専用。** 書き込みは Phase 3 まで作らない。
//
// ここが Obsidian のプラグインとの決定的な違いになる場所。Bases の成果物は
// Obsidian の中でしかレンダリングされないが、Camp のビューは同じ定義から
// 人向けの表とモデル向けの文脈の両方を出す。`view` ツールが返すのは
// 画面と同じ Run() の結果。
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/MoomA-0750/camp/internal/store"
)

const protocolVersion = "2025-06-18"

// Server は1本の stdio 接続。
type Server struct {
	db   *store.DB
	in   *bufio.Reader
	out  io.Writer
	mu   sync.Mutex // 書き込みを混ぜない
	name string
	ver  string

	tools map[string]Tool
	cache viewCache
}

// Tool は1つの道具。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(args map[string]any) (any, error)
}

func New(db *store.DB, in io.Reader, out io.Writer, version string) *Server {
	s := &Server{
		db: db, in: bufio.NewReaderSize(in, 1<<20), out: out,
		name: "camp", ver: version,
	}
	s.tools = map[string]Tool{}
	for _, t := range s.builtinTools() {
		s.tools[t.Name] = t
	}
	return s
}

// --- JSON-RPC ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve は1行1メッセージの JSON-RPC を回す。
func (s *Server) Serve() error {
	for {
		line, err := s.in.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = trimSpace(line)
		if len(line) == 0 {
			if err == io.EOF {
				return nil
			}
			continue
		}
		var req request
		if e := json.Unmarshal(line, &req); e != nil {
			s.send(response{JSONRPC: "2.0",
				Error: &rpcError{Code: -32700, Message: "JSON が読めない"}})
			continue
		}
		s.handle(&req)
		if err == io.EOF {
			return nil
		}
	}
}

func trimSpace(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func (s *Server) handle(req *request) {
	// 通知（id 無し）には返さない。
	notify := len(req.ID) == 0

	result, err := s.dispatch(req)
	if notify {
		return
	}
	if err != nil {
		s.send(response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32603, Message: err.Error()}})
		return
	}
	s.send(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(req *request) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]string{"name": s.name, "version": s.ver},
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			// 何ができるかを最初に伝える。読み取り専用であることを明示する。
			"instructions": "Camp は Claude Code のセッション履歴と Obsidian Vault を" +
				"1つのSQLiteに保持している。すべて読み取り専用。" +
				"ビュー（views_list / view）は Vault の .base をそのまま読むが、" +
				"列は定義が挙げたものだけでなく実在する全部を返す。",
		}, nil

	case "notifications/initialized", "ping":
		return map[string]any{}, nil

	case "tools/list":
		names := make([]string, 0, len(s.tools))
		for n := range s.tools {
			names = append(names, n)
		}
		sortStrings(names)
		list := make([]map[string]any, 0, len(names))
		for _, n := range names {
			t := s.tools[n]
			list = append(list, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.Schema,
			})
		}
		return map[string]any{"tools": list}, nil

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		t, ok := s.tools[p.Name]
		if !ok {
			return nil, fmt.Errorf("知らない道具 %q", p.Name)
		}
		out, err := t.Run(p.Arguments)
		if err != nil {
			// 道具の失敗はプロトコルの失敗ではない。isError で返す。
			return map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
			}, nil
		}
		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(body)}},
			"structuredContent": out,
		}, nil
	}
	return nil, fmt.Errorf("知らないメソッド %q", req.Method)
}

func (s *Server) send(r response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	s.out.Write(append(b, '\n'))
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
