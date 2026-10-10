package audit

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
)

// `replay` —— **配置漂移检测**。
//
// `explain` 回答「当时发生了什么」;`replay` 回答更难的那个问题:
// **「现在这份配置,还能不能复现当时那次阶梯?」**
//
// 做法:取回 L0(原始请求),用**当前配置**逐层重跑改写,然后对每一层做两个检查:
//
//  1. **记录自洽性**:记录里存的原文,哈希回去是否等于它自己记的 request_hash。
//     不等 → 审计文件被截断或改过(这本身就是值得报警的事);
//  2. **漂移检查**:把两侧请求体里的占位符 nonce 归一化后比哈希。
//     不等 → 从这一层起,历史记录与当前行为已经脱钩(规则表改了、层被关了)。
//
// 只有这两个检查都过,才谈得上「可重放」——否则这个词就是空话。

// nonceMark 是 nonce 归一化后用的占位标记。
const nonceMark = "NONCE"

type StepVerdict string

const (
	// VerdictMatch 复算出的请求体与记录一致(占位符 nonce 已归一化)。
	VerdictMatch StepVerdict = "match"
	// VerdictDrift 不一致:配置已漂移。
	VerdictDrift StepVerdict = "drift"
	// VerdictUnverifiable 记录里没有原文,无法复算。
	VerdictUnverifiable StepVerdict = "unverifiable"
)

type ReplayStep struct {
	Seq                  uint32      `json:"seq"`
	Layer                string      `json:"layer"`
	RecordedHash         string      `json:"recorded_hash"`
	ReplayedHash         string      `json:"replayed_hash,omitempty"`
	RecordSelfConsistent *bool       `json:"record_self_consistent,omitempty"`
	Verdict              StepVerdict `json:"verdict"`
	Note                 string      `json:"note,omitempty"`
}

type ReplayReport struct {
	Trace           string       `json:"trace"`
	Session         string       `json:"session"`
	Wire            string       `json:"wire"`
	Model           string       `json:"model"`
	RecordedOutcome string       `json:"recorded_outcome"`
	Steps           []ReplayStep `json:"steps"`
	// SkippedDerived 是被跳过的**派生记录**条数(工具调用 + 工具轮的响应)。
	// 这两类记录的「请求体」不是改写管线的产物,拿去比只会得到假漂移。
	SkippedDerived int    `json:"skipped_derived"`
	Verified       bool   `json:"verified"`
	Rendered       string `json:"rendered"`
}

// Replay 用当前管线复算一次阶梯。pristine 来自记录里的原文(L0 那条)。
func (r *Reader) Replay(trace string, pipeline *rewrite.Pipeline, enabledLayers []govern.LayerID, maxLayer govern.LayerID) (*ReplayReport, error) {
	all := r.ByTrace(trace)
	if len(all) == 0 {
		return nil, fmt.Errorf("在审计目录里找不到 trace `%s` 的记录", trace)
	}

	// 只复算**改写管线的产物**。派生记录(工具调用、工具轮响应)的请求体
	// 来自别处,拿去比只会得到假漂移。
	var records []*TraceRecord
	skipped := 0
	for _, rec := range all {
		if rec.ToolCall != nil || rec.Decision == "tool_call" || rec.Decision == "tool_round" {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("trace `%s` 只有派生记录(工具调用/工具轮),没有可复算的改写尝试", trace)
	}

	pristine, err := pristineFrom(records[0])
	if err != nil {
		return nil, err
	}
	report := &ReplayReport{
		Trace: records[0].Trace.String(), Session: records[0].Session,
		Wire: string(records[0].Wire), Model: records[0].Model,
		RecordedOutcome: records[len(records)-1].Outcome,
		SkippedDerived:  skipped,
	}

	for _, rec := range records {
		attempt, err := pipeline.Attempt(pristine, rec.Layer)
		if err != nil {
			return nil, fmt.Errorf("复算 %s 层失败: %w", rec.Layer.Label(), err)
		}
		replayedBody := mustJSONString(attempt.Envelope.Body)
		replayedNonce := ""
		if attempt.Vault != nil && !attempt.Vault.IsEmpty() {
			replayedNonce = attempt.Vault.Nonce()
		}
		recordedNonce := nonceFromRecord(rec)
		step := ReplayStep{Seq: rec.Seq, Layer: rec.Layer.Label(), RecordedHash: rec.RequestHash}

		// 没有原文 → 只能比哈希;而 L4 的 nonce 每次都不同,所以「做过抽象化
		// 又没留原文」这种情况必须如实标成 unverifiable,不能谎报漂移。
		if rec.RawRequest == nil {
			step.ReplayedHash = attempt.RequestHash()
			switch {
			case attempt.RequestHash() == rec.RequestHash:
				step.Verdict = VerdictMatch
			case len(rec.MaskedTargets) > 0:
				step.Verdict = VerdictUnverifiable
				step.Note = "记录没有原文,且该层做过目标抽象化,nonce 无法归一化"
			default:
				step.Verdict = VerdictDrift
				step.Note = "记录没有原文,只能按哈希比对,二者不同"
			}
			report.Steps = append(report.Steps, step)
			continue
		}

		raw := *rec.RawRequest
		selfConsistent := ShortHash([]byte(raw)) == rec.RequestHash
		recordedNorm := normalizeNonce(raw, recordedNonce)
		replayedNorm := normalizeNonce(replayedBody, replayedNonce)
		recordedHash := ShortHash([]byte(recordedNorm))
		replayedHash := ShortHash([]byte(replayedNorm))
		step.RecordedHash = recordedHash
		step.ReplayedHash = replayedHash
		sc := selfConsistent
		step.RecordSelfConsistent = &sc

		if recordedHash == replayedHash {
			step.Verdict = VerdictMatch
		} else {
			step.Verdict = VerdictDrift
		}
		var notes []string
		if !selfConsistent {
			notes = append(notes, "记录自洽性失败:原文哈希与记录中的 request_hash 不一致(文件可能被改过)")
		}
		if step.Verdict == VerdictDrift {
			notes = append(notes, "该层请求体与记录不一致")
			if !containsLayer(enabledLayers, rec.Layer) {
				notes = append(notes, rec.Layer.Label()+" 已不在 enabled_layers 里")
			}
			if maxLayer < rec.Layer {
				notes = append(notes, fmt.Sprintf("max_layer 现在是 %s,低于该层", maxLayer.Label()))
			}
		}
		step.Note = strings.Join(notes, ";")
		report.Steps = append(report.Steps, step)
	}

	report.Verified = true
	for _, s := range report.Steps {
		if s.Verdict != VerdictMatch {
			report.Verified = false
			break
		}
	}
	report.Rendered = renderReplay(report)
	return report, nil
}

func renderReplay(r *ReplayReport) string {
	var b strings.Builder
	short := r.Trace
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(&b, "trace %s  session %s\nwire %s  model %s\n\n", short, r.Session, r.Wire, r.Model)
	fmt.Fprintf(&b, "%-3s %-4s %-17s %-17s %s\n", "#", "LAYER", "RECORDED", "REPLAYED", "VERDICT")
	for _, s := range r.Steps {
		replayed := s.ReplayedHash
		if replayed == "" {
			replayed = "-"
		}
		fmt.Fprintf(&b, "%-3d %-4s %-17s %-17s %s\n", s.Seq, s.Layer, s.RecordedHash, replayed, s.Verdict)
		if s.Note != "" {
			fmt.Fprintf(&b, "      └─ %s\n", s.Note)
		}
	}
	b.WriteString("\n")
	if r.SkippedDerived > 0 {
		fmt.Fprintf(&b, "(跳过 %d 条派生记录:工具调用与工具轮响应不是改写管线的产物)\n", r.SkippedDerived)
	}
	if r.Verified {
		fmt.Fprintf(&b, "result: replay verified — 当前配置能复现这次阶梯(%d 层)\n", len(r.Steps))
	} else {
		first := "?"
		for _, s := range r.Steps {
			if s.Verdict != VerdictMatch {
				first = s.Layer
				break
			}
		}
		fmt.Fprintf(&b, "result: 无法复现 — 从 %s 起与记录不一致(记录 outcome=%s)\n", first, r.RecordedOutcome)
		b.WriteString("提示: 这些记录是旧配置下产生的,不能用来论证当前行为;\n")
		b.WriteString("      要么按旧配置复核,要么用新配置重跑一次并记下新的 trace。\n")
	}
	return b.String()
}

// pristineFrom 从记录里重建原始请求。
//
// 优先用 RawRequest(raw_capture 打开时);否则用 RequestPreview 里的正文
// (脱敏预览不是原文,无法复算 → 返回错误而不是给假结果)。
func pristineFrom(rec *TraceRecord) (*rewrite.Envelope, error) {
	if rec.RawRequest == nil {
		return nil, fmt.Errorf(
			"记录没有原文(audit.raw_capture = false),无法复算;打开 raw_capture 后再产生新记录即可")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(*rec.RawRequest), &body); err != nil {
		return nil, fmt.Errorf("记录里的原文不是合法 JSON: %w", err)
	}
	wire := rewrite.WireOpenAi
	if rec.Wire == WireAnthropic {
		wire = rewrite.WireAnthropic
	}
	return rewrite.NewEnvelope(body, wire), nil
}

// nonceFromRecord 从记录里的 masked_targets 反推 nonce。
//
// 占位符形如 «TGT-<nonce>-<n>»,取第一条即可。
func nonceFromRecord(rec *TraceRecord) string {
	for _, m := range rec.MaskedTargets {
		if nonce := nonceOfPlaceholder(m.Placeholder); nonce != "" {
			return nonce
		}
	}
	return ""
}

func nonceOfPlaceholder(placeholder string) string {
	// «TGT-ABCD1234-0»
	inner := strings.TrimPrefix(placeholder, "«")
	inner = strings.TrimSuffix(inner, "»")
	parts := strings.Split(inner, "-")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

// normalizeNonce 把 nonce 换成一个稳定标记,于是两侧可以逐字节比较。
func normalizeNonce(text, nonce string) string {
	if nonce == "" {
		return text
	}
	return strings.ReplaceAll(text, nonce, nonceMark)
}

func mustJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func containsLayer(layers []govern.LayerID, l govern.LayerID) bool {
	for _, x := range layers {
		if x == l {
			return true
		}
	}
	return false
}
