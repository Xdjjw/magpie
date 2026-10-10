package govern

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// SessionState 是一个会话的记忆。
//
// 真实使用中同一会话的敏感度是稳定的:上一轮需要 L3 才能过,这一轮
// 大概率也需要。所以记住「上次有效的层」,下一轮直接从那层起步。
// 两条约束保证记忆不会变成风险:
//   - SessionEntryFloor:记忆不能把起点压到配置的下限以下;
//   - DowngradeAfterSuccess:连续成功就降一层,避免长期停在最重的改写上。
type SessionState struct {
	Key                  string
	EntryLayer           LayerID
	LastSuccessLayer     *LayerID // nil = 尚未成功过
	Requests             uint64
	Escalations          uint64
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
	UnlockedSkills       []string
	UnlockedPatterns     []string
	updated              time.Time
}

func newSessionState(key string, entry LayerID) *SessionState {
	return &SessionState{Key: key, EntryLayer: entry, updated: time.Now()}
}

func (s *SessionState) age() time.Duration { return time.Since(s.updated) }

// SessionView 是 /admin/sessions 的投影(json tag 与 Rust 版线型一致)。
type SessionView struct {
	Key              string   `json:"key"`
	EntryLayer       LayerID  `json:"entry_layer"`
	LastSuccessLayer *LayerID `json:"last_success_layer"`
	Requests         uint64   `json:"requests"`
	Escalations      uint64   `json:"escalations"`
	AgeS             uint64   `json:"age_s"`
}

// SessionRegistry 并发安全(Rust 版用 DashMap;这里一把 RWMutex 足够,
// 会话量级由 MaxSessions 封顶,热路径读写都是 O(1))。
type SessionRegistry struct {
	mu        sync.RWMutex
	ttl       time.Duration
	max       int
	m         map[string]*SessionState
	base      time.Time
	lastSweep atomic.Uint64
}

func NewSessionRegistry(l Limits) *SessionRegistry {
	max := l.MaxSessions
	if max < 1 {
		max = 1
	}
	return &SessionRegistry{
		// ttl = 0 表示「立即过期」,测试与回放会用到,不能强行抬到 1s。
		ttl:  time.Duration(l.SessionTTLSeconds) * time.Second,
		max:  max,
		m:    make(map[string]*SessionState),
		base: time.Now(),
	}
}

func (r *SessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

func (r *SessionRegistry) IsEmpty() bool { return r.Len() == 0 }

// EntryLayer 查这一轮该从哪层起步。
func (r *SessionRegistry) EntryLayer(key string, cfg *PipelineConfig, defaultEntry LayerID) LayerID {
	if !cfg.SessionMemory {
		return defaultEntry
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.m[key]; ok {
		layer := s.EntryLayer
		if layer < cfg.SessionEntryFloor {
			layer = cfg.SessionEntryFloor
		}
		return layer
	}
	return defaultEntry
}

// Record 记录一次请求的结果并更新记忆。
// acceptedLayer == nil 表示这一轮**全层耗尽**(诚实错误)。
func (r *SessionRegistry) Record(key string, cfg *PipelineConfig, acceptedLayer *LayerID, escalated bool) {
	if !cfg.SessionMemory {
		return
	}
	r.sweepIfNeeded()
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.m[key]
	if !ok {
		state = newSessionState(key, cfg.EntryLayer)
		r.m[key] = state
	}
	state.Requests++
	state.updated = time.Now()
	if escalated {
		state.Escalations++
	}
	if acceptedLayer == nil {
		state.ConsecutiveSuccesses = 0
		state.ConsecutiveFailures++
		// 全层失败:下一轮直接从最重的层起步,别再从 L0 白爬。
		entry := cfg.MaxLayer
		if entry < cfg.SessionEntryFloor {
			entry = cfg.SessionEntryFloor
		}
		state.EntryLayer = entry
		return
	}
	layer := *acceptedLayer
	l := layer
	state.LastSuccessLayer = &l
	state.ConsecutiveSuccesses++
	state.ConsecutiveFailures = 0
	// 记忆起点 = 上次成功层;若允许降级,每连续成功两次降一层。
	target := layer
	if cfg.DowngradeAfterSuccess && state.ConsecutiveSuccesses >= 2 && layer > cfg.SessionEntryFloor {
		target = layer - 1
		state.ConsecutiveSuccesses = 0
	}
	if target < cfg.SessionEntryFloor {
		target = cfg.SessionEntryFloor
	}
	state.EntryLayer = target
}

// UnlockSet 读本会话的解锁集。未知会话返回空集。
func (r *SessionRegistry) UnlockSet(key string) ([]string, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.m[key]; ok {
		return append([]string(nil), s.UnlockedSkills...), append([]string(nil), s.UnlockedPatterns...)
	}
	return nil, nil
}

// RecordUnlocks 写回本会话的解锁集:与「上次有效的层」同一套机制,
// 状态跟着会话走,且可以从审计记录重建(每条 skill:<name> 都记在案)。
func (r *SessionRegistry) RecordUnlocks(key string, cfg *PipelineConfig, skills, patterns []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.m[key]
	if !ok {
		state = newSessionState(key, cfg.EntryLayer)
		r.m[key] = state
	}
	state.updated = time.Now()
	for _, s := range skills {
		if !containsString(state.UnlockedSkills, s) {
			state.UnlockedSkills = append(state.UnlockedSkills, s)
		}
	}
	for _, p := range patterns {
		if !containsString(state.UnlockedPatterns, p) {
			state.UnlockedPatterns = append(state.UnlockedPatterns, p)
		}
	}
}

// SweepIfNeeded 清理过期会话;超过容量时清掉最旧的一半。
// 清扫频率与 TTL 挂钩:TTL 很短时几乎每次请求都清,TTL 很长时最多 60s 一次。
func (r *SessionRegistry) sweepIfNeeded() {
	interval := r.ttl
	if interval > 60*time.Second {
		interval = 60 * time.Second
	}
	nowMS := uint64(time.Since(r.base).Milliseconds())
	last := r.lastSweep.Load()
	if nowMS-last < uint64(interval.Milliseconds()) && r.Len() <= r.max {
		return
	}
	r.lastSweep.Store(nowMS)

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.m {
		if v.age() >= r.ttl {
			delete(r.m, k)
		}
	}
	if len(r.m) > r.max {
		type aged struct {
			key string
			age time.Duration
		}
		ages := make([]aged, 0, len(r.m))
		for k, v := range r.m {
			ages = append(ages, aged{k, v.age()})
		}
		sort.Slice(ages, func(i, j int) bool { return ages[i].age > ages[j].age })
		for _, a := range ages[:len(ages)/2] {
			delete(r.m, a.key)
		}
	}
}

// Snapshot 导出全部会话的视图。
func (r *SessionRegistry) Snapshot() []SessionView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SessionView, 0, len(r.m))
	for _, s := range r.m {
		out = append(out, SessionView{
			Key:              s.Key,
			EntryLayer:       s.EntryLayer,
			LastSuccessLayer: s.LastSuccessLayer,
			Requests:         s.Requests,
			Escalations:      s.Escalations,
			AgeS:             uint64(s.age().Seconds()),
		})
	}
	return out
}

func (r *SessionRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m = make(map[string]*SessionState)
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
