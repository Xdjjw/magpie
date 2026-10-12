package cli

import (
	"os"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// magpie search add, with a key and an address in either order, and rm.
func TestSearchCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := searchCmd([]string{"add", "tavily"}); err == nil {
		t.Error("tavily added without a key")
	}
	if err := searchCmd([]string{"add", "google", "k"}); err == nil {
		t.Error("an unknown search API added")
	}
	if err := searchCmd([]string{"add", "searxng"}); err == nil {
		t.Error("SearXNG added without its address")
	}
	for _, args := range [][]string{{"add", "Tavily", "tvly-1"}, {"add", "searxng", "url=https://sx.example.com"}, {"add", "brave", "url=https://brave.example.com", "b-1"}} {
		if err := searchCmd(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// a new address alone keeps the key
	if err := searchCmd([]string{"add", "brave", "url=https://brave2.example.com"}); err != nil {
		t.Fatal(err)
	}
	as := provider.SearchAPIs()
	if len(as) != 3 || as[0] != (provider.SearchAPI{Vendor: "tavily", Key: "tvly-1"}) || as[1].URL != "https://sx.example.com" ||
		as[2] != (provider.SearchAPI{Vendor: "brave", Key: "b-1", URL: "https://brave2.example.com"}) {
		t.Fatalf("%+v", as)
	}
	// a new key alone keeps the base URL the user set (#1580); url= with
	// nothing puts the vendor's own back
	if err := searchCmd([]string{"add", "brave", "b-2"}); err != nil {
		t.Fatal(err)
	}
	if as := provider.SearchAPIs(); as[2] != (provider.SearchAPI{Vendor: "brave", Key: "b-2", URL: "https://brave2.example.com"}) {
		t.Fatalf("a new key took the base URL away: %+v", as[2])
	}
	if err := searchCmd([]string{"add", "brave", "url="}); err != nil {
		t.Fatal(err)
	}
	if as := provider.SearchAPIs(); as[2] != (provider.SearchAPI{Vendor: "brave", Key: "b-2"}) || as[2].Base() != "https://api.search.brave.com" {
		t.Fatalf("url= kept the base URL: %+v", as[2])
	}
	for _, bad := range []string{"url=ftp://x.example.com", "url=https://x.example.com/?k=1", "url=https://x.example.com/#a", "url=x.example.com"} {
		if err := searchCmd([]string{"add", "tavily", "tvly-2", bad}); err == nil {
			t.Errorf("%s taken", bad)
		}
	}
	if err := searchCmd(nil); err != nil {
		t.Fatal(err)
	}
	if err := searchCmd([]string{"rm", "tavily"}); err != nil {
		t.Fatal(err)
	}
	if err := searchCmd([]string{"rm", "tavily"}); err == nil {
		t.Error("removed twice")
	}
	if as := provider.SearchAPIs(); len(as) != 2 || as[0].Vendor != "searxng" {
		t.Fatalf("after rm %+v", as)
	}
}

// magpie search add you, with no key, adds You.com's free search (#1626).
func TestSearchCLIYouWithoutKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := searchCmd([]string{"add", "you"}); err != nil {
		t.Fatal(err)
	}
	if as := provider.SearchAPIs(); len(as) != 1 || as[0] != (provider.SearchAPI{Vendor: "you"}) {
		t.Fatalf("%+v", as)
	}
}
