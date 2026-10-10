package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/detect"
)

// Config 与 xp-core AuditCfg 对齐。
type Config struct {
	Enabled             bool
	Dir                 string
	RawCapture          bool
	Redact              bool
	RedactPatterns      []string
	IncludeBodies       bool
	MaxBodyCaptureBytes int
	RotateMB            uint64
}

func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		Dir:                 "var/audit",
		RawCapture:          false,
		Redact:              true,
		RedactPatterns:      DefaultRedactPatterns(),
		IncludeBodies:       true,
		MaxBodyCaptureBytes: 64 * 1024,
		RotateMB:            64,
	}
}

// Sink 落审计记录。写失败会向上报,但审计平面不参与在线决策。
type Sink interface {
	Write(record *TraceRecord) error
	Flush() error
	// CurrentPath 返回正在写的文件(tail 用它提示用户)。
	CurrentPath() (string, bool)
}

// NullSink 丢弃一切(审计关闭时使用,避免热路径上到处 if enabled)。
type NullSink struct{}

func (NullSink) Write(*TraceRecord) error    { return nil }
func (NullSink) Flush() error                { return nil }
func (NullSink) CurrentPath() (string, bool) { return "", false }

// TeeSink 广播到多个 sink。
type TeeSink struct{ sinks []Sink }

func NewTee(sinks ...Sink) *TeeSink { return &TeeSink{sinks} }

func (t *TeeSink) Write(r *TraceRecord) error {
	for _, s := range t.sinks {
		if err := s.Write(r); err != nil {
			return err
		}
	}
	return nil
}

func (t *TeeSink) Flush() error {
	for _, s := range t.sinks {
		if err := s.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (t *TeeSink) CurrentPath() (string, bool) {
	for _, s := range t.sinks {
		if p, ok := s.CurrentPath(); ok {
			return p, true
		}
	}
	return "", false
}

// CountingSink 是即时计数器:/admin/stats 用它回答「现在」,
// 文件扫描回答「历史」——两者刻意分开,前者 O(1)。
type CountingSink struct {
	Written  atomic.Uint64
	Refused  atomic.Uint64
	Accepted atomic.Uint64
}

func (c *CountingSink) Write(r *TraceRecord) error {
	c.Written.Add(1)
	if r.IsAccepted() {
		c.Accepted.Add(1)
	} else {
		c.Refused.Add(1)
	}
	return nil
}
func (c *CountingSink) Flush() error                { return nil }
func (c *CountingSink) CurrentPath() (string, bool) { return "", false }

// JsonlSink 按行 JSON 追加写,按大小轮转:
//   - 追加写、崩溃安全、不需要索引;
//   - 可以直接 grep / jq / tail -f,本地审计最需要的三件事;
//   - 轮转按文件名时间戳排序,读回时天然有序。
type JsonlSink struct {
	dir         string
	rotateBytes uint64
	mu          sync.Mutex
	file        *os.File
	path        string
	bytes       uint64
	lines       uint64
}

func OpenJsonl(dir string, rotateMB uint64) (*JsonlSink, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := jsonlPath(dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	rb := rotateMB * 1024 * 1024
	if rb < 1024*1024 {
		rb = 1024 * 1024
	}
	return &JsonlSink{dir: dir, rotateBytes: rb, file: f, path: path}, nil
}

func jsonlPath(dir string) string {
	stamp := time.Now().UTC().Format("20060102T150405.000")
	return filepath.Join(dir, fmt.Sprintf("audit-%s-%d.jsonl", stamp, os.Getpid()))
}

func (s *JsonlSink) Write(r *TraceRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bytes >= s.rotateBytes {
		path := jsonlPath(s.dir)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_ = s.file.Close()
		s.file, s.path, s.bytes = f, path, 0
	}
	if _, err := s.file.Write(line); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.bytes += uint64(len(line))
	s.lines++
	return nil
}

func (s *JsonlSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Sync()
}

func (s *JsonlSink) CurrentPath() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, true
}

func (s *JsonlSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// LiveCounters 是进程存活期间的累计计数。
type LiveCounters struct {
	RecordsWritten uint64 `json:"records_written"`
	Accepted       uint64 `json:"accepted"`
	Rejected       uint64 `json:"rejected"`
}

// Plane 是审计平面的运行时形态:配置 + 脱敏器 + sink + 即时计数。
type Plane struct {
	Config   Config
	Redactor *Redactor
	Sink     Sink
	counters *CountingSink
}

// Build 按配置构建。审计关闭时返回 NullSink,热路径无需分支。
func Build(cfg Config) (*Plane, error) {
	var redactor *Redactor
	if cfg.Redact {
		r, err := NewRedactor(cfg.RedactPatterns)
		if err != nil {
			return nil, err
		}
		redactor = r
	} else {
		redactor = DisabledRedactor()
	}
	counters := &CountingSink{}
	if !cfg.Enabled {
		return &Plane{Config: cfg, Redactor: redactor, Sink: NullSink{}, counters: counters}, nil
	}
	jsonl, err := OpenJsonl(cfg.Dir, cfg.RotateMB)
	if err != nil {
		return nil, fmt.Errorf("audit sink: %w", err)
	}
	return &Plane{
		Config:   cfg,
		Redactor: redactor,
		Sink:     NewTee(jsonl, counters),
		counters: counters,
	}, nil
}

// Live 是即时计数(不需要读盘)。
func (p *Plane) Live() LiveCounters {
	return LiveCounters{
		RecordsWritten: p.counters.Written.Load(),
		Accepted:       p.counters.Accepted.Load(),
		Rejected:       p.counters.Refused.Load(),
	}
}

// Summarize 把判定结果压成审计摘要。
func Summarize(v *detect.Verdict) VerdictSummary {
	families := make([]string, len(v.Families))
	for i, f := range v.Families {
		families[i] = string(f)
	}
	return VerdictSummary{
		Outcome:    string(v.Outcome),
		Confidence: v.Confidence,
		Escalate:   v.Escalate,
		UpstreamOK: v.UpstreamOK,
		Families:   families,
		Signals:    v.RuleIDs(),
		Reason:     v.Reason,
	}
}
