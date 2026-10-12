package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/plugin"
)

// A Grok id named at an effort is priced as its model; a fast, mini or
// build one no catalog lists stays unpriced (#224).
func TestGrokEffortListPrice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755)
	os.WriteFile(catalog.CachePath(), []byte(`{
	  "xai":{"id":"xai","models":{
	    "grok-4.7":{"id":"grok-4.7","cost":{"input":2,"output":6,"cache_read":0.5}},
	    "grok-4.6":{"id":"grok-4.6","cost":{"input":2,"output":6,"cache_read":0.5}}}},
	  "openai":{"id":"openai","models":{
	    "gpt-6-astra":{"id":"gpt-6-astra","cost":{"input":10,"output":50}}}}}`), 0o644)
	catalog.Reset()
	t.Cleanup(catalog.Reset)

	grok := Provider{ID: "grok", Name: "Grok (SuperGrok)"}
	for _, id := range []string{"grok-4.7", "grok-4.7-low", "grok-4.7-medium", "grok-4.7-high", "grok-4.7-xhigh",
		"GROK-4.7-High", "x-ai/grok-4.7-xhigh", "grok-4.6-minimal",
		// a model at a window of its own (#1608)
		"grok-4.7@500k", "grok-4.6@500k"} {
		for name, price := range map[string]func(string) (catalog.Price, bool){"MakerPrice": MakerPrice, "ListPrice": grok.ListPrice} {
			pr, ok := price(id)
			if !ok || pr.Input != 2 || pr.Output != 6 || pr.CacheRead != 0.5 {
				t.Errorf("%s(%q) = %+v, %v; want grok's $2/$6/$0.5", name, id, pr, ok)
			}
		}
	}
	for _, id := range []string{"grok-4.7-fast", "grok-4.7-low-fast", "grok-4.7-xhigh-fast", "grok-4.7-build",
		"grok-4.7-build-fast", "grok-4.7-mini", "grok-4.7-mini-high", "grok-4.7-max",
		// only Grok's ids are read this way
		"gpt-6-astra-high", "cursor-grok-4.7-high", "gpt-6-astra@500k", "grok-4.7-build@500k"} {
		if pr, ok := grok.ListPrice(id); ok {
			t.Errorf("ListPrice(%q) = %+v; want unpriced", id, pr)
		}
		if pr, ok := MakerPrice(id); ok {
			t.Errorf("MakerPrice(%q) = %+v; want unpriced", id, pr)
		}
	}
	if got := PricedName("grok-4.7-xhigh"); got != "grok-4.7" {
		t.Errorf("PricedName(grok-4.7-xhigh) = %q", got)
	}
	if got := PricedName("grok-4.7-low-fast"); got != "grok-4.7-low-fast" {
		t.Errorf("PricedName(grok-4.7-low-fast) = %q", got)
	}
}

// A Cursor account's fast Grok ids are priced at what Cursor bills fast
// mode (https://cursor.com/docs/models: Grok 4.7 (Fast) $4 / $1 cache
// read / $12, 3x standard past 256k), whether the built-in or the plugin
// serves them, so a routing group of grok-4.7-fast counts its cost
// (TradingLife on Discord). A "-fast" Grok id elsewhere is a model of its
// own, and stays as the catalogs price it.
func TestCursorGrokFastPrice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755)
	os.WriteFile(catalog.CachePath(), []byte(`{
	  "xai":{"id":"xai","models":{
	    "grok-4.7":{"id":"grok-4.7","cost":{"input":2,"output":6,"cache_read":0.5}}}}}`), 0o644)
	catalog.Reset()
	t.Cleanup(catalog.Reset)

	builtin := Provider{ID: "cursor", Name: "Cursor", Account: &Account{Agent: "cursor"}}
	moved := Provider{ID: "cursor", Name: "Cursor", Account: &Account{plugin: &plugin.Provider{ID: "cursor"}}}
	for _, p := range []Provider{builtin, moved} {
		for _, id := range []string{"grok-4.7-fast", "grok-4.7-low-fast", "grok-4.7-high-fast", "grok-4.7-xhigh-fast",
			"GROK-4.7-Fast", "grok-4.7-fast@500k", "grok-4.7-medium-fast@256k"} {
			pr, ok := p.ListPrice(id)
			if !ok || pr.Input != 4 || pr.Output != 12 || pr.CacheRead != 1 {
				t.Errorf("%s ListPrice(%q) = %+v, %v; want Cursor's fast $4/$12/$1", p.PluginProvider(), id, pr, ok)
				continue
			}
			// past 256k, 3x standard
			if long := pr.At(300000); long.Input != 6 || long.Output != 18 || long.CacheRead != 1.5 {
				t.Errorf("%s ListPrice(%q) past 256k = %+v; want $6/$18/$1.5", p.PluginProvider(), id, long)
			}
			if short := pr.At(200000); short.Input != 4 {
				t.Errorf("%s ListPrice(%q) at 200k = %+v; want $4", p.PluginProvider(), id, short)
			}
		}
		// not fast: grok's own price
		if pr, ok := p.ListPrice("grok-4.7-high"); !ok || pr.Input != 2 {
			t.Errorf("ListPrice(grok-4.7-high) = %+v, %v; want $2", pr, ok)
		}
		// a fast id of a model Cursor gives no fast price for
		if pr, ok := p.ListPrice("grok-4.6-fast"); ok {
			t.Errorf("ListPrice(grok-4.6-fast) = %+v; want unpriced", pr)
		}
	}
	// elsewhere a fast id is not Cursor's fast mode
	grok := Provider{ID: "grok", Name: "Grok", Account: &Account{plugin: &plugin.Provider{ID: "grok"}}}
	for _, id := range []string{"grok-4.7-fast", "grok-4.7-build-fast"} {
		if pr, ok := grok.ListPrice(id); ok {
			t.Errorf("grok ListPrice(%q) = %+v; want unpriced", id, pr)
		}
		if pr, ok := MakerPrice(id); ok {
			t.Errorf("MakerPrice(%q) = %+v; want unpriced", id, pr)
		}
	}
}
