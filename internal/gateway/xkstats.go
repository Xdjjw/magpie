package gateway

// `magpie xk stats` 的聚合:把审计 JSONL 变成「每层 verdict 分布 + 拒绝率」。
//
// persona 前后各跑一段日常流量,两次 stats 一对比,拒绝率降没降是算
// 出来的,不是感觉出来的。只读审计文件,不碰请求路径。

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/settings"
)

// XkStatsLayer 是一层的尝试与三态判定分布。
type XkStatsLayer struct {
	Layer     string  `json:"layer"`
	Attempts  int     `json:"attempts"`
	Passed    int     `json:"passed"`
	Uncertain int     `json:"uncertain"`
	Refused   int     `json:"refused"`
	RefusePct float64 `json:"refuse_pct"`
}

// XkStatsSummary 是一个窗口内的阶梯统计。Rates 都是 0..1。
type XkStatsSummary struct {
	Attempts    int     `json:"attempts"`
	Requests    int     `json:"requests"`
	Passed      int     `json:"passed"`
	Uncertain   int     `json:"uncertain"`
	Refused     int     `json:"refused"`
	RefusalRate float64 `json:"refusal_rate"`
	// RequestRate 是「至少有一层被拒的请求」占比——一次请求爬阶梯被拒
	// 多次只算一次,这才是使用体感上的拒绝率。
	RequestRefusalRate float64        `json:"request_refusal_rate"`
	Accepted           int            `json:"accepted"`
	Escalated          int            `json:"escalated"`
	Stopped            int            `json:"stopped"`
	Layers             []XkStatsLayer `json:"layers"`
	// NotPassed 家族/信号计数:哪些检测族在驱动拒绝与升层,
	// persona 措辞和 L2 词面规则该补哪里,看这里。
	Families map[string]int `json:"families"`
	Signals  map[string]int `json:"signals"`
}

// XkStats 聚合最近 last 条阶梯尝试(last<=0 取全部)。没有审计数据时
// 返回零值摘要,不报错——「还没流量」也是一种要能打印的状态。
func XkStats(last int) (*XkStatsSummary, error) {
	s := &XkStatsSummary{Families: map[string]int{}, Signals: map[string]int{}}
	records := audit.NewReader(filepath.Join(settings.Dir(), "xkeysmith", "audit")).Tail(last)
	byLayer := map[govern.LayerID]*XkStatsLayer{}
	traces, refusedTraces := map[string]bool{}, map[string]bool{}
	for _, rec := range records {
		// 只统计阶梯尝试:有 verdict 的记录(工具回路等记录天然没有)。
		outcome := string(rec.Verdict.Outcome)
		if outcome == "" {
			continue
		}
		layer, ok := govern.LayerFromU8(uint8(rec.Layer))
		if !ok {
			layer = 255
		}
		row := byLayer[layer]
		if row == nil {
			row = &XkStatsLayer{Layer: layerLabel(layer)}
			byLayer[layer] = row
		}
		s.Attempts++
		row.Attempts++
		traces[rec.Trace.String()] = true
		switch outcome {
		case "refused":
			s.Refused++
			row.Refused++
			refusedTraces[rec.Trace.String()] = true
		case "uncertain":
			s.Uncertain++
			row.Uncertain++
		default:
			s.Passed++
			row.Passed++
		}
		if outcome != "passed" {
			for _, f := range rec.Verdict.Families {
				s.Families[f]++
			}
			for _, sig := range rec.Verdict.Signals {
				s.Signals[sig]++
			}
		}
		switch {
		case rec.Decision == "accept":
			s.Accepted++
		case strings.HasPrefix(rec.Decision, "escalate"):
			s.Escalated++
		case strings.HasPrefix(rec.Decision, "stop"):
			s.Stopped++
		}
	}
	s.Requests = len(traces)
	for _, row := range byLayer {
		if row.Attempts > 0 {
			row.RefusePct = float64(row.Refused) / float64(row.Attempts)
		}
		s.Layers = append(s.Layers, *row)
	}
	sort.Slice(s.Layers, func(i, j int) bool { return s.Layers[i].Layer < s.Layers[j].Layer })
	if s.Attempts > 0 {
		s.RefusalRate = float64(s.Refused) / float64(s.Attempts)
	}
	if s.Requests > 0 {
		s.RequestRefusalRate = float64(len(refusedTraces)) / float64(s.Requests)
	}
	return s, nil
}

func layerLabel(l govern.LayerID) string {
	if l == 255 {
		return "L?"
	}
	return l.Label()
}
