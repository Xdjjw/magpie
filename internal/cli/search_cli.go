package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// The web search APIs, from the terminal: magpie search (#419).

const searchUsage = `usage:
  magpie search                              the search APIs a model's web search goes to
  magpie search add <api> <key> [url=<address>]   add one, or give one a new key
                                             (several keys as k1,k2: the next is tried when one is refused)
  magpie search add searxng url=<address> [<key>]
  magpie search add you [<key>]              You.com: free with no key (100 searches a day)
  magpie search rm <api>                     take one away

  apis: tavily, brave, exa, firecrawl, you, searxng

  url= is the API's base URL, for an endpoint of your own that answers as that API
  does (e.g. url=https://search.example.com/tavily). Without it the vendor's own is
  used; a new key alone keeps the url set before, and url= with nothing after it puts
  the vendor's own back. The key is sent only to that url.

  A client's web search (Claude Code's WebSearch, Codex's web_search) is done by the
  model's provider when it can search, then by a provider that can (a Claude or ChatGPT
  account, OpenAI, Anthropic, OpenRouter…), and then by these, in the order they were
  added: what they find goes to the model as it is, and no model is asked to read it.
  Without any of them, the client's search is left out.

  e.g. magpie search add tavily tvly-…
       magpie search add searxng url=https://searx.example.com
       magpie search add you`

func searchCmd(args []string) error {
	if len(args) == 0 {
		as := provider.StoredSearchAPIs()
		if len(as) == 0 {
			fmt.Println(muted.Render("  no search API · magpie search add <tavily|brave|exa|firecrawl|you|searxng> <key>"))
			return nil
		}
		for i, a := range as {
			what := a.MaskedKey()
			if a.Key == "" {
				what = "no key"
			}
			if a.URL != "" {
				what += "  " + a.URL
			}
			line := fmt.Sprintf("  %d. %s  %s", i+1, pad(a.Name(), 14), muted.Render(what))
			if !a.Ready() {
				line += "  " + faint.Render("needs its key")
			}
			fmt.Println(line)
		}
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(searchUsage)
		return nil
	case "add", "set":
		if len(args) < 2 {
			return fmt.Errorf("magpie search add <api> <key> [url=<address>]")
		}
		a := provider.SearchAPI{Vendor: strings.ToLower(args[1])}
		// a new key alone keeps the address set before (#1580); url= with
		// nothing after it puts the vendor's own back
		urlGiven := false
		for _, v := range args[2:] {
			if u, ok := strings.CutPrefix(v, "url="); ok {
				a.URL, urlGiven = u, true
			} else if a.Key == "" {
				a.Key = v
			} else {
				return fmt.Errorf("magpie search add <api> <key> [url=<address>]")
			}
		}
		if i := slices.IndexFunc(provider.StoredSearchAPIs(), func(x provider.SearchAPI) bool { return x.Vendor == a.Vendor }); i >= 0 && !urlGiven {
			a.URL = provider.StoredSearchAPIs()[i].URL
		}
		if err := provider.SetSearchAPI(a); err != nil {
			return err
		}
		as := provider.StoredSearchAPIs()
		i := slices.IndexFunc(as, func(x provider.SearchAPI) bool { return x.Vendor == a.Vendor })
		fmt.Println(green.Render("✓"), as[i].Name(), muted.Render(fmt.Sprintf("search API %d of %d", i+1, len(as))))
		return nil
	case "rm", "remove":
		if len(args) != 2 {
			return fmt.Errorf("magpie search rm <api>")
		}
		if err := provider.RemoveSearchAPI(strings.ToLower(args[1])); err != nil {
			return err
		}
		fmt.Println(green.Render("✓"), "removed")
		return nil
	}
	return fmt.Errorf("magpie search [add|rm] (magpie search help)")
}
