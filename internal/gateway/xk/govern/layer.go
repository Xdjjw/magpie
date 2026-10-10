// Package govern is the escalation brain, ported from xkeysmith's xp-govern
// (Rust) as part of this fork's semantic plane. The Rust crate stays the
// spec and the test oracle; corpus/unit tests here mirror its tests one to
// one and must stay green together.
//
// 三件事,全部刻意做成可穷举的纯逻辑:
//   - Budget:单次请求的预算与早停(调用数 / token / 墙钟 / 工具轮);
//   - Plan:升级决策 plan(verdict, layer, budget, policy) -> Decision;
//   - SessionRegistry:会话层记忆(「上次成功的层」+ 解锁集)。
package govern

import (
	"strconv"
)

// LayerID 是改写层号 L0..L4。刻意用有序整数:**层号可比大小**,
// 「层单调递增」「会话记忆降低起点」这类不变量用 < 直接表达。
type LayerID uint8

const (
	// L0 原始请求直接转发。
	L0 LayerID = 0
	// L1 注入 LAB_INSTRUCTIONS + 上下文伪装。
	L1 LayerID = 1
	// L2 关键词/正则改写。
	L2 LayerID = 2
	// L3 全量中立化重写。
	L3 LayerID = 3
	// L4 目标抽象化 + 事后还原。
	L4 LayerID = 4
)

var AllLayers = []LayerID{L0, L1, L2, L3, L4}

func LayerFromU8(v uint8) (LayerID, bool) {
	if v > 4 {
		return 0, false
	}
	return LayerID(v), true
}

// Next returns the layer above; ok=false when already at the top.
func (l LayerID) Next() (LayerID, bool) { return LayerFromU8(uint8(l) + 1) }

func (l LayerID) Label() string { return "L" + strconv.Itoa(int(l)) }

func (l LayerID) Name() string {
	switch l {
	case L0:
		return "passthrough"
	case L1:
		return "context-injection"
	case L2:
		return "lexical-tamper"
	case L3:
		return "neutral-rewrite"
	default:
		return "target-abstraction"
	}
}

func (l LayerID) String() string { return l.Label() }

// MarshalJSON 序列化为 "L0".."L4"(与 Rust 的手写 Serialize 一致;审计与
// 界面都吃这个形状)。
func (l LayerID) MarshalJSON() ([]byte, error) { return []byte(`"` + l.Label() + `"`), nil }

// UnmarshalJSON 同时接受 "L3"(字符串)与 3(数字)——与 Rust 的
// Deserialize 兼容旧数据的行为对齐。
func (l *LayerID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
		if len(s) > 0 && s[0] == 'L' {
			s = s[1:]
		}
	}
	v, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return err
	}
	layer, ok := LayerFromU8(uint8(v))
	if !ok {
		return strconv.ErrRange
	}
	*l = layer
	return nil
}

// PipelineConfig 是会话与阶梯共用的管线策略(与 xp-core PipelineCfg
// 对齐;流式策略等阶段②再需要)。
type PipelineConfig struct {
	EntryLayer            LayerID
	MaxLayer              LayerID
	EnabledLayers         []LayerID
	SessionMemory         bool
	SessionEntryFloor     LayerID
	DowngradeAfterSuccess bool
}

func DefaultPipelineConfig() PipelineConfig {
	layers := make([]LayerID, len(AllLayers))
	copy(layers, AllLayers)
	return PipelineConfig{
		EntryLayer:            L0,
		MaxLayer:              L4,
		EnabledLayers:         layers,
		SessionMemory:         true,
		SessionEntryFloor:     L0,
		DowngradeAfterSuccess: true,
	}
}
