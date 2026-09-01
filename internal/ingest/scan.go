package ingest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScanReport は取り込み前の実測レポート。
// dev/active/phase0-plan.md の受け入れ条件をそのまま検算するためのもの。
type ScanReport struct {
	Files        int
	EmptyFiles   int
	Bytes        int64
	Lines        int
	ParseErrors  []string // ファイル単位の致命的な失敗
	BrokenLines  []string // JSON として読めなかった行（読み取りは継続する）
	PartialTails int

	// Degraded は厳密なデコードに失敗して最小限の項目だけ拾った行。
	// Raw は失っていないので、パーサを直せば後から作り直せる。
	Degraded        int
	DegradedSamples []string

	TypeCounts map[string]int

	UUIDs         int
	DistinctUUIDs int
	DupUUIDs      []string

	AssistantLines    int
	DistinctMessageID int
	NaiveOutputTokens int64
	DedupOutputTokens int64
	NaiveCacheRead    int64
	DedupCacheRead    int64
	ModelCounts       map[string]int

	SubagentFiles int
	CompactBounds int

	// SessionIDsPerFile / RunIDsPerFile はファイルごとの値の種類数。
	// main ファイルでは SessionID が1種、RunID が resume 回数ぶん出る。
	SessionIDsPerFile map[string]int
	RunIDsPerFile     map[string]int

	FileHistoryPaths map[string]struct{}
	ToolResultPaths  map[string]struct{}
	AttachmentPaths  map[string]struct{}
}

// ScanDir は root 以下の .jsonl を全部読み、実測レポートを返す。DBには書かない。
func ScanDir(root string) (*ScanReport, error) {
	rep := &ScanReport{
		TypeCounts:        map[string]int{},
		ModelCounts:       map[string]int{},
		SessionIDsPerFile: map[string]int{},
		RunIDsPerFile:     map[string]int{},
		FileHistoryPaths:  map[string]struct{}{},
		ToolResultPaths:   map[string]struct{}{},
		AttachmentPaths:   map[string]struct{}{},
	}

	seenUUID := map[string]struct{}{}
	seenMsgID := map[string]struct{}{}
	dedupOut := map[string]int64{}
	dedupCache := map[string]int64{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		rep.Files++
		rep.Bytes += info.Size()
		if info.Size() == 0 {
			rep.EmptyFiles++
			return nil
		}
		if strings.Contains(path, "/subagents/") {
			rep.SubagentFiles++
		}

		sessIDs := map[string]struct{}{}
		runIDs := map[string]struct{}{}

		res, walkErr := WalkFile(path, func(l *Line) error {
			rep.Lines++
			rep.TypeCounts[l.Type]++
			if l.Degraded {
				rep.Degraded++
				if len(rep.DegradedSamples) < 5 {
					rep.DegradedSamples = append(rep.DegradedSamples, l.DegradedBy)
				}
			}

			if l.UUID != "" {
				rep.UUIDs++
				if _, dup := seenUUID[l.UUID]; dup {
					if len(rep.DupUUIDs) < 20 {
						rep.DupUUIDs = append(rep.DupUUIDs, l.UUID)
					}
				} else {
					seenUUID[l.UUID] = struct{}{}
				}
			}
			if l.SessionID != "" {
				sessIDs[l.SessionID] = struct{}{}
			}
			if l.RunID != "" {
				runIDs[l.RunID] = struct{}{}
			}
			if l.Type == "system" && l.Subtype == "compact_boundary" {
				rep.CompactBounds++
			}

			if l.Type == "assistant" && l.Message != nil {
				rep.AssistantLines++
				rep.ModelCounts[l.Message.Model]++
				if u := l.Message.Usage; u != nil {
					rep.NaiveOutputTokens += u.OutputTokens
					rep.NaiveCacheRead += u.CacheReadInputTokens
					if id := l.Message.ID; id != "" {
						if _, seen := seenMsgID[id]; !seen {
							seenMsgID[id] = struct{}{}
							dedupOut[id] = u.OutputTokens
							dedupCache[id] = u.CacheReadInputTokens
						}
					}
				}
			}

			if p := l.AbsPath(); p != "" {
				rep.FileHistoryPaths[p] = struct{}{}
			}
			if p := l.ToolResultFilePath(); p != "" {
				rep.ToolResultPaths[p] = struct{}{}
			}
			if _, fn := l.AttachmentInfo(); fn != "" {
				rep.AttachmentPaths[fn] = struct{}{}
			}
			return nil
		})

		if walkErr != nil {
			rep.ParseErrors = append(rep.ParseErrors, fmt.Sprintf("%s: %v", path, walkErr))
		}
		for _, e := range res.Broken {
			rep.BrokenLines = append(rep.BrokenLines, fmt.Sprintf("%s: %v", path, e))
		}
		if len(res.Pending) > 0 {
			rep.PartialTails++
		}

		rel, _ := filepath.Rel(root, path)
		rep.SessionIDsPerFile[rel] = len(sessIDs)
		rep.RunIDsPerFile[rel] = len(runIDs)
		return nil
	})
	if err != nil {
		return nil, err
	}

	rep.DistinctUUIDs = len(seenUUID)
	rep.DistinctMessageID = len(seenMsgID)
	for _, v := range dedupOut {
		rep.DedupOutputTokens += v
	}
	for _, v := range dedupCache {
		rep.DedupCacheRead += v
	}
	return rep, nil
}

// Print はレポートを人が読める形で書き出す。
func (r *ScanReport) Print(w *os.File) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	p("ファイル        %d（うち0バイト %d）\n", r.Files, r.EmptyFiles)
	p("バイト          %d\n", r.Bytes)
	p("行              %d\n", r.Lines)
	p("致命的失敗      %d\n", len(r.ParseErrors))
	for _, e := range r.ParseErrors {
		p("  %s\n", e)
	}
	p("JSON不正な行    %d\n", len(r.BrokenLines))
	for i, e := range r.BrokenLines {
		if i >= 3 {
			p("  … 他 %d 件\n", len(r.BrokenLines)-3)
			break
		}
		p("  %s\n", e)
	}
	p("縮退した行      %d\n", r.Degraded)
	for _, e := range r.DegradedSamples {
		p("  %s\n", e)
	}
	p("未完了の末尾    %d\n", r.PartialTails)
	p("subagentファイル %d\n", r.SubagentFiles)
	p("compact_boundary %d\n\n", r.CompactBounds)

	p("行タイプ %d 種:\n", len(r.TypeCounts))
	types := make([]string, 0, len(r.TypeCounts))
	for t := range r.TypeCounts {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return r.TypeCounts[types[i]] > r.TypeCounts[types[j]] })
	for _, t := range types {
		p("  %-22s %d\n", t, r.TypeCounts[t])
	}

	p("\nuuid            %d（distinct %d）", r.UUIDs, r.DistinctUUIDs)
	if r.UUIDs == r.DistinctUUIDs {
		p(" 全部ユニーク\n")
	} else {
		p(" 重複あり: %v\n", r.DupUUIDs)
	}

	p("\nassistant行     %d\n", r.AssistantLines)
	p("distinct msg.id %d\n", r.DistinctMessageID)
	p("output_tokens   素朴 %d / 重複排除 %d\n", r.NaiveOutputTokens, r.DedupOutputTokens)
	p("cache_read      素朴 %d / 重複排除 %d\n", r.NaiveCacheRead, r.DedupCacheRead)

	p("\nモデル:\n")
	models := make([]string, 0, len(r.ModelCounts))
	for m := range r.ModelCounts {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return r.ModelCounts[models[i]] > r.ModelCounts[models[j]] })
	for _, m := range models {
		p("  %-32s %d\n", m, r.ModelCounts[m])
	}

	union := map[string]struct{}{}
	for k := range r.FileHistoryPaths {
		union[k] = struct{}{}
	}
	for k := range r.ToolResultPaths {
		union[k] = struct{}{}
	}
	for k := range r.AttachmentPaths {
		union[k] = struct{}{}
	}
	p("\n絶対パス: file-history %d / toolUseResult %d / attachment %d / 和集合 %d\n",
		len(r.FileHistoryPaths), len(r.ToolResultPaths), len(r.AttachmentPaths), len(union))

	p("\nsessionId が2種以上のファイル（0であるべき。subagentは親と同じなので1）:\n")
	n := 0
	for f, c := range r.SessionIDsPerFile {
		if c > 1 {
			p("  %s: %d\n", f, c)
			n++
		}
	}
	if n == 0 {
		p("  なし\n")
	}

	p("\nrunId が最も多いファイル（resume回数）:\n")
	files := make([]string, 0, len(r.RunIDsPerFile))
	for f := range r.RunIDsPerFile {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return r.RunIDsPerFile[files[i]] > r.RunIDsPerFile[files[j]] })
	for i, f := range files {
		if i >= 5 || r.RunIDsPerFile[f] == 0 {
			break
		}
		p("  %-70s %d\n", f, r.RunIDsPerFile[f])
	}
}
