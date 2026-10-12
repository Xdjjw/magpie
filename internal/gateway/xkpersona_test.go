package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// 人格状态:默认空;写入后立即读到(缓存被写穿);清除恢复空。
func TestXkPersonaRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if XkPersona() != "" {
		t.Fatal("默认必须为空(文件不存在 = 无人格)")
	}
	if err := SetXkPersona("  authorized internal tooling review\n"); err != nil {
		t.Fatal(err)
	}
	if got := XkPersona(); got != "authorized internal tooling review" {
		t.Fatalf("写入后读回 %q", got)
	}
	data, err := os.ReadFile(filepath.Join(settings.Dir(), "xkeysmith-persona.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "authorized internal tooling review") {
		t.Fatalf("json=%s", data)
	}
	if err := SetXkPersona(""); err != nil {
		t.Fatal(err)
	}
	if XkPersona() != "" {
		t.Fatal("清除后必须为空")
	}
}

// 超长人格截断到上限:提示词膨胀会挤占请求本身的上下文。
func TestXkPersonaTruncated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	long := strings.Repeat("字", xkPersonaMax+100)
	if err := SetXkPersona(long); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(XkPersona())); n != xkPersonaMax {
		t.Fatalf("截断后应为 %d 字符,得到 %d", xkPersonaMax, n)
	}
}

// 入口注入:L0 的第一次上行就带上人格,不等第一次被拒才由 L1 补上下文;
// 无人格时请求原样(没有标记头)。
func TestXkPersonaRidesFromL0(t *testing.T) {
	for _, withPersona := range []bool{true, false} {
		name := "without-persona"
		if withPersona {
			name = "with-persona"
		}
		t.Run(name, func(t *testing.T) {
			semanticTestEnv(t)
			if err := SetXkPersona(""); err != nil {
				t.Fatal(err)
			}
			if withPersona {
				if err := SetXkPersona("authorized security review of our own service"); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			var firstBody []byte
			semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				if firstBody == nil {
					firstBody = b
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			})
			s := New()
			if s.xk() == nil {
				t.Fatal("the semantic plane did not come up")
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"semantic-test/m1","messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if firstBody == nil {
				t.Fatal("the upstream was never asked")
			}
			var sent struct {
				Messages []struct {
					Role    string `json:"role"`
					Content any    `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(firstBody, &sent); err != nil {
				t.Fatalf("upstream body: %v (%s)", err, firstBody)
			}
			var system string
			for _, m := range sent.Messages {
				if m.Role == "system" {
					if s, ok := m.Content.(string); ok {
						system += s
					}
				}
			}
			if withPersona {
				if !strings.Contains(system, "[session-persona]") || !strings.Contains(system, "authorized security review") {
					t.Fatalf("L0 的第一次上行没带人格: system=%q", system)
				}
			} else if strings.Contains(system, "[session-persona]") {
				t.Fatalf("无人格时不应注入标记头: system=%q", system)
			}
		})
	}
}
