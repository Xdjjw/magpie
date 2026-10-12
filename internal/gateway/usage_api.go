package gateway

import (
	"net/http"
	"strconv"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usageapi"
)

// The usage log, as the Usage page reads it (HoGee_xxl on X: an app of
// their own reading magpie's usage over its gateway, as they can't over
// the GUI's /api/usage): GET /v1/magpie/usage is the page's summary of a
// ?period=, the same JSON as /api/usage, and GET /v1/magpie/usage/requests
// its requests a page at a time, with /api/usage/requests' filters. Both
// are read-only, and what was said in a call is not told here.
//
// They name accounts, keys and sessions, so, as /v1/magpie/quotas, they
// answer this machine, and another only with an enabled gateway key while
// magpie is shared. A key held to a budget, to some models or to some
// accounts is someone else's to use: it is told its own calls alone, from
// /v1/magpie/usage/requests, and not the summary of everyone's.

const usageRefused = "magpie's usage is told to another machine only when magpie is shared on the local network (Settings → Share on local network) and the request carries its API key (Authorization: Bearer <key> or x-api-key: <key>)"

// heldKey: the request's gateway key is held to a budget, models or
// accounts, and so sees only its own usage.
func heldKey(who access.Identity) bool {
	return who.KeyID != "" && (who.Limit.Limited() || len(who.Models) > 0 || len(who.Accounts) > 0)
}

func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request) {
	if !local(r) && !sharedWith(r) {
		writeError(w, provider.Chat, http.StatusForbidden, usageRefused)
		return
	}
	if heldKey(access.Caller(r.Context())) {
		writeError(w, provider.Chat, http.StatusForbidden, "this gateway key is held to a budget, models or accounts, so it is told its own calls alone: GET /v1/magpie/usage/requests")
		return
	}
	out := usageapi.State(usageapi.PeriodOf(r.URL.Query().Get("period")))
	if !local(r) {
		out.Path = "" // where the log is on this machine
	}
	writeJSON(w, 200, out)
}

func (s *Server) usageRequests(w http.ResponseWriter, r *http.Request) {
	if !local(r) && !sharedWith(r) {
		writeError(w, provider.Chat, http.StatusForbidden, usageRefused)
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := usageapi.FilterOf(q)
	who := access.Caller(r.Context())
	if !heldKey(who) {
		writeJSON(w, 200, usageapi.LedgerPage(usageapi.PeriodOf(q.Get("period")), f, offset, limit))
		return
	}
	f.CallerKey = who.KeyID
	writeJSON(w, 200, usageapi.OwnPage(usageapi.LedgerPage(usageapi.PeriodOf(q.Get("period")), f, offset, limit), who.KeyID))
}
