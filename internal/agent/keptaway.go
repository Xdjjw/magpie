package agent

import (
	"bytes"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

// synced runs an agent's sync, which brings magpie's models in its files up
// to date, not where they are reached: a file whose magpie the user pointed
// at a magpie on another machine (a NAS's, or this one's LAN address, coeo91
// on Discord) keeps that address and its key, as ZCode's and WorkBuddy's
// writers do on their own (zcodeAddress). Without it, every start and
// update of magpie (SyncCatalog) put the agent back on 127.0.0.1.
func synced(sync func() error) error {
	return edit.Filtered(keptAway, sync)
}

// originRE is an http(s) URL's scheme and host, as it is written in a file.
var originRE = regexp.MustCompile(`https?://[A-Za-z0-9.\-_\[\]:%]+`)

// keyLineRE is a key and its value as a JSON, JSONC, YAML, TOML or .env
// file writes it: "apiKey": "x", api_key = "x", apiKey: x, KEY=x; not one
// whose value is an object or a list. The value is group 4 when quoted with
// ", 5 with ', 6 bare.
var keyLineRE = regexp.MustCompile(`(["']?)([A-Za-z0-9_\-]+)(["']?)[ \t]*[:=][ \t]*(?:"((?:[^"\\\n]|\\.)*)"|'([^'\n]*)'|([^\s"',{}\[\]#][^\n"',}\]#]*))`)

// keyName says a key of that name holds what an agent sends the gateway to
// be let in: apiKey, api_key, MAGPIE_QWEN_API_KEY, experimental_bearer_token,
// Authorization (not max_tokens).
func keyName(name string) bool {
	n := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	return n == "key" || strings.HasSuffix(n, "apikey") || strings.HasSuffix(n, "token") || strings.HasSuffix(n, "authorization")
}

// ourToken says v, with any "Bearer " before it, is a key magpie gives an
// agent: gateway.Token, one that names the agent (gateway.TokenFor), or the
// LAN sharing key.
func ourToken(v string) bool {
	v = strings.TrimPrefix(v, "Bearer ")
	return ourKey(v) || strings.HasPrefix(v, gateway.Token+"-")
}

type keyValue struct {
	start, end int    // the value's bytes, quotes left out
	value      string // as written
}

// keyValues are the values of the keys keyName takes in b.
func keyValues(b []byte) []keyValue {
	var out []keyValue
	for _, m := range keyLineRE.FindAllSubmatchIndex(b, -1) {
		if string(b[m[2]:m[3]]) != string(b[m[6]:m[7]]) || !keyName(string(b[m[4]:m[5]])) {
			continue
		}
		for g := 8; g <= 12; g += 2 {
			if m[g] >= 0 {
				v := strings.TrimRight(string(b[m[g]:m[g+1]]), " \t\r")
				out = append(out, keyValue{m[g], m[g] + len(v), v})
				break
			}
		}
	}
	return out
}

// keptAway is data, a sync's write of the file at path, with magpie kept
// where the file had it on another machine. That is a file without the
// gateway's own address (gateway.URL), with an address on another machine
// and none a WSL distro was seen at (a distro's way to Windows is magpie's
// to move, wslMovedFrom). Where the write brings the gateway's address in
// place of one on another machine, that one stays; where it brings magpie's
// own key in place of another, in a file that had none of magpie's keys,
// the other stays. An address on this machine's loopback (another port,
// localhost) is magpie's own and follows the gateway; with more than one
// address or key gone, which went where can't be told, and the write is
// left as it is. magpie's own files are never the user's pointing.
func keptAway(path string, data []byte) []byte {
	if strings.HasPrefix(path, filepath.Dir(provider.Path())+string(filepath.Separator)) {
		return data
	}
	g := gateway.URL()
	was, err := edit.Read(path)
	if err != nil || len(was) == 0 || bytes.Contains(was, []byte(g)) {
		return data
	}
	elsewhere := false
	for _, o := range originRE.FindAll(was, -1) {
		if wslHost(string(o)) {
			return data
		}
		elsewhere = elsewhere || onAnotherMachine(string(o))
	}
	if !elsewhere {
		return data
	}
	out := data
	if bytes.Contains(data, []byte(g)) {
		away := ""
		for _, o := range originRE.FindAll(was, -1) {
			if s := string(o); bytes.Count(data, o) < bytes.Count(was, o) && s != away {
				if away != "" {
					return data
				}
				away = s
			}
		}
		if !onAnotherMachine(away) {
			return data
		}
		out = replaceOrigin(data, g, away)
	}

	// the key: the one that went with the address, in place of magpie's
	theirs, before, after := "", keyCounts(was), keyCounts(out)
	for v, n := range before {
		if ourToken(v) {
			return out
		}
		if after[v] < n {
			if theirs != "" {
				return out
			}
			theirs = v
		}
	}
	if theirs == "" {
		return out
	}
	kvs := keyValues(out)
	for i := len(kvs) - 1; i >= 0; i-- {
		kv := kvs[i]
		if !ourToken(kv.value) {
			continue
		}
		v := theirs
		if strings.HasPrefix(kv.value, "Bearer ") {
			v = "Bearer " + theirs
		}
		out = append(out[:kv.start:kv.start], append([]byte(v), out[kv.end:]...)...)
	}
	return out
}

// keyCounts are the values of the keys keyName takes in b, "Bearer " left
// out, and how many times each is there.
func keyCounts(b []byte) map[string]int {
	n := map[string]int{}
	for _, kv := range keyValues(b) {
		n[strings.TrimPrefix(kv.value, "Bearer ")]++
	}
	return n
}

// replaceOrigin is b with each from (an origin) that isn't part of a longer
// one (http://127.0.0.1:3425 in http://127.0.0.1:34250) replaced by to.
func replaceOrigin(b []byte, from, to string) []byte {
	var out []byte
	for {
		i := bytes.Index(b, []byte(from))
		if i < 0 {
			return append(out, b...)
		}
		j := i + len(from)
		out = append(out, b[:i]...)
		if j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == '.' || b[j] == '-') {
			out = append(out, from...)
		} else {
			out = append(out, to...)
		}
		b = b[j:]
	}
}

// wslHost says base's host is one a WSL distro reached Windows at, now or
// before.
func wslHost(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	wsl.Lock()
	defer wsl.Unlock()
	for _, d := range wsl.seen {
		if d != nil && (d.Gateway == h || slices.Contains(d.Was, h)) {
			return true
		}
	}
	return false
}
