package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 审计读回:tail / explain / stats。
//
// 这是审计平面的**产出**:把 JSONL 变成人能读的升级阶梯。
// 没有这一步,前面的记录只是垃圾数据。

type Reader struct{ Dir string }

func NewReader(dir string) *Reader { return &Reader{Dir: dir} }

// Files 返回所有审计文件,按文件名升序(文件名内嵌时间戳 → 天然时序)。
func (r *Reader) Files() []string {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(r.Dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// readFile 读一个文件里的全部记录。半行(进程被杀)跳过,不让整个审计不可读。
func readFile(path string) []*TraceRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []*TraceRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, &rec)
	}
	return out
}

// Tail 返回最后 n 条记录。
func (r *Reader) Tail(n int) []*TraceRecord {
	var all []*TraceRecord
	for _, f := range r.Files() {
		all = append(all, readFile(f)...)
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// normalizeID 去掉非字母数字并小写:完整 UUID 与 12 位短 id 都能匹配。
func normalizeID(raw string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(raw) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ByTrace 按 trace id(完整或短)取记录,按 seq 排序。
func (r *Reader) ByTrace(trace string) []*TraceRecord {
	needle := normalizeID(trace)
	if needle == "" {
		return nil
	}
	var out []*TraceRecord
	for _, f := range r.Files() {
		for _, rec := range readFile(f) {
			if strings.HasPrefix(normalizeID(rec.Trace.String()), needle) {
				out = append(out, rec)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// LadderStep 是阶梯表里的一行。
type LadderStep struct {
	Seq            uint32   `json:"seq"`
	Layer          string   `json:"layer"`
	Decision       string   `json:"decision"`
	StopReason     string   `json:"stop_reason,omitempty"`
	DecisionReason string   `json:"decision_reason,omitempty"`
	VerdictOutcome string   `json:"verdict"`
	Confidence     float64  `json:"confidence"`
	Signals        []string `json:"signals"`
	Rules          []string `json:"rules"`
	Notes          []string `json:"notes"`
	LatencyMS      uint64   `json:"latency_ms"`
	Tokens         uint64   `json:"tokens"`
	Outcome        string   `json:"outcome"`
	Error          string   `json:"error,omitempty"`
}

// Explanation 是一次请求的完整升级阶梯。
type Explanation struct {
	Trace         string         `json:"trace"`
	Session       string         `json:"session"`
	Wire          string         `json:"wire"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	Stream        bool           `json:"stream"`
	Steps         []LadderStep   `json:"steps"`
	Outcome       string         `json:"outcome"`
	TotalLatency  uint64         `json:"total_latency_ms"`
	TotalTokens   uint64         `json:"total_tokens"`
	MaskedTargets []MaskedTarget `json:"masked_targets"`
	Rules         []string       `json:"rules"`
	Rendered      string         `json:"rendered"`
}

// Explain 生成一次请求的升级阶梯;找不到返回 nil。
func (r *Reader) Explain(trace string) *Explanation {
	records := r.ByTrace(trace)
	if len(records) == 0 {
		return nil
	}
	return buildExplanation(records)
}

func buildExplanation(records []*TraceRecord) *Explanation {
	last := records[len(records)-1]
	ex := &Explanation{
		Trace:    last.Trace.String(),
		Session:  last.Session,
		Wire:     string(last.Wire),
		Provider: last.Provider,
		Model:    last.Model,
		Stream:   last.Stream,
		Outcome:  last.Outcome,
	}
	seenRule := map[string]bool{}
	seenMask := map[string]bool{}
	for _, rec := range records {
		ex.TotalLatency += rec.LatencyMS
		ex.TotalTokens += rec.Usage.TotalTokens
		for _, rule := range rec.Rules {
			if !seenRule[rule] {
				seenRule[rule] = true
				ex.Rules = append(ex.Rules, rule)
			}
		}
		for _, m := range rec.MaskedTargets {
			if !seenMask[m.Placeholder] {
				seenMask[m.Placeholder] = true
				ex.MaskedTargets = append(ex.MaskedTargets, m)
			}
		}
		step := LadderStep{
			Seq:            rec.Seq,
			Layer:          rec.Layer.Label(),
			Decision:       rec.Decision,
			VerdictOutcome: rec.Verdict.Outcome,
			Confidence:     rec.Verdict.Confidence,
			Signals:        rec.Verdict.Signals,
			Rules:          rec.Rules,
			Notes:          rec.Notes,
			LatencyMS:      rec.LatencyMS,
			Tokens:         rec.Usage.TotalTokens,
			Outcome:        rec.Outcome,
		}
		if rec.StopReason != nil {
			step.StopReason = *rec.StopReason
		}
		if rec.DecisionReason != nil {
			step.DecisionReason = *rec.DecisionReason
		}
		if rec.Error != nil {
			step.Error = *rec.Error
		}
		ex.Steps = append(ex.Steps, step)
	}
	ex.Rendered = renderExplanation(ex)
	return ex
}

// renderExplanation 把阶梯渲染成人能读的表格(CLI 与界面都吃它)。
func renderExplanation(ex *Explanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "trace %s  session %s\n", shortTrace(ex.Trace), ex.Session)
	fmt.Fprintf(&b, "%s · %s · %s · stream=%v\n", ex.Wire, ex.Provider, ex.Model, ex.Stream)
	fmt.Fprintf(&b, "outcome %s   latency %dms   tokens %d\n\n", ex.Outcome, ex.TotalLatency, ex.TotalTokens)
	b.WriteString("  seq  layer  decision            conf  outcome   rules\n")
	for _, s := range ex.Steps {
		fmt.Fprintf(&b, "  %3d  %-5s  %-19s %.2f  %-9s %s\n",
			s.Seq, s.Layer, s.Decision, s.Confidence, s.Outcome, strings.Join(s.Rules, ","))
		if s.DecisionReason != "" {
			fmt.Fprintf(&b, "       why: %s\n", s.DecisionReason)
		}
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "       note: %s\n", n)
		}
	}
	if len(ex.MaskedTargets) > 0 {
		b.WriteString("\nmasked targets:\n")
		for _, m := range ex.MaskedTargets {
			fmt.Fprintf(&b, "  %s  %s → %s\n", m.Kind, m.Original, m.Placeholder)
		}
	}
	return b.String()
}

func shortTrace(t string) string {
	if len(t) > 12 {
		return t[:12]
	}
	return t
}

// Stats 是审计统计(默认只看最近若干文件,避免为一个数字扫全库)。
type Stats struct {
	Records      uint64            `json:"records"`
	Traces       uint64            `json:"traces"`
	ByOutcome    map[string]uint64 `json:"by_outcome"`
	ByLayer      map[string]uint64 `json:"by_layer"`
	TotalTokens  uint64            `json:"total_tokens"`
	TotalLatency uint64            `json:"total_latency_ms"`
	Escalations  uint64            `json:"escalations"`
}

func (s *Stats) AvgLatencyMS() uint64 {
	if s.Records == 0 {
		return 0
	}
	return s.TotalLatency / s.Records
}

// Stats 统计最近 maxFiles 个文件。
func (r *Reader) Stats(maxFiles int) *Stats {
	files := r.Files()
	if maxFiles < 1 {
		maxFiles = 1
	}
	if len(files) > maxFiles {
		files = files[len(files)-maxFiles:]
	}
	stats := &Stats{ByOutcome: map[string]uint64{}, ByLayer: map[string]uint64{}}
	seen := map[string]bool{}
	for _, f := range files {
		for _, rec := range readFile(f) {
			stats.Records++
			stats.ByOutcome[rec.Outcome]++
			stats.ByLayer[rec.Layer.Label()]++
			stats.TotalTokens += rec.Usage.TotalTokens
			stats.TotalLatency += rec.LatencyMS
			if rec.Decision == "escalate" {
				stats.Escalations++
			}
			if t := rec.Trace.String(); !seen[t] {
				seen[t] = true
				stats.Traces++
			}
		}
	}
	return stats
}

func (s *Stats) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "records %d  traces %d  escalations %d  avg latency %d ms  tokens %d\n",
		s.Records, s.Traces, s.Escalations, s.AvgLatencyMS(), s.TotalTokens)
	b.WriteString("outcomes:\n")
	for _, k := range sortedKeys(s.ByOutcome) {
		fmt.Fprintf(&b, "  %-20s %d\n", k, s.ByOutcome[k])
	}
	b.WriteString("layers:\n")
	for _, k := range sortedKeys(s.ByLayer) {
		fmt.Fprintf(&b, "  %-20s %d\n", k, s.ByLayer[k])
	}
	return b.String()
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
