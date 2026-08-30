package selftest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Every tick the battery lands messages on the probe agent: one real inbound
// SMTP delivery, one real outbound send, and two loopback self-sends — and a
// loopback lands BOTH an outbound and an inbound copy. Before this sweep only
// scenarioWebSocketRoundTrip cleaned up after itself, and only the one inbound
// copy it knew the id of; everything else stayed live forever.
//
// On staging that reached 268k live messages on the single probe agent (plus
// 92k on each sdk-monitor agent), which is enough table churn that an unrelated
// agent delete can exceed HAProxy's 60s server timeout and come back as a 504 —
// with HAProxy's HTML error page, which then fails the conformance suite's
// response-schema gate because the spec documents application/json.
//
// The sweep is a whole-inbox pass rather than per-scenario id tracking on
// purpose: a loopback self-send lands a copy the sending scenario never learns
// an id for, and a scenario that fails partway leaves residue no deferred
// cleanup would catch. Listing what is actually there covers both cases with
// one mechanism.
const (
	// sweepBudget bounds one hygiene pass per phase. The battery creates well
	// under this per tick, so the surplus also drains backlog left by earlier
	// runs — gradually, without turning the prober into a bulk-delete client.
	sweepBudget = 50
	// listPageMax is the server's per-page ceiling on listMessages.
	listPageMax = 100
	// sweepTimeout bounds the pass on its OWN context: the battery's ctx may
	// already be done by the time hygiene runs.
	sweepTimeout = 30 * time.Second
)

// SweepResult reports what one hygiene pass actually did.
type SweepResult struct {
	Trashed int `json:"trashed"`
	Purged  int `json:"purged"`
}

// SweepMessages trashes live probe messages and then purges the trash.
//
// The two steps are ordered, not alternatives: "delete forever" only accepts a
// message that is ALREADY in the trash (the store returns ErrNotInTrash, surfaced
// as 409 not_in_trash), so a live message has to be trashed first. Trashing is
// idempotent, so re-trashing a row another pass already moved is harmless.
//
// Best-effort by construction: it runs AFTER every scenario has reported, takes
// its own context, and returns no error. A failed cleanup is hygiene debt, never
// a red battery — the same contract trashMessage and the sdk-monitor's _cleanup
// already follow, and for the same reason: a webhook redelivery or a flaky
// delete must not be able to turn a healthy stack red.
func (p *Probe) SweepMessages() SweepResult {
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()

	var res SweepResult
	// Live → trash. direction=all and read_status=all are both load-bearing:
	// the endpoint defaults to inbound-only, and for inbound to unread-only,
	// so the defaults would walk straight past every outbound copy and every
	// inbound message a scenario had already read.
	for _, id := range p.listMessageIDs(ctx, "direction=all&read_status=all") {
		if p.deleteMessage(ctx, id, false) {
			res.Trashed++
		}
	}
	// Trash → gone. Picks up what was just trashed plus anything an earlier
	// run trashed and left sitting for the 30-day janitor.
	for _, id := range p.listMessageIDs(ctx, "deleted=true&direction=all&read_status=all") {
		if p.deleteMessage(ctx, id, true) {
			res.Purged++
		}
	}
	return res
}

// listMessageIDs returns up to sweepBudget message ids for one filter. A list
// failure yields nothing rather than an error: the next tick retries, and a
// sweep that cannot list is not a reason to report anything about the stack.
func (p *Probe) listMessageIDs(ctx context.Context, query string) []string {
	limit := sweepBudget
	if limit > listPageMax {
		limit = listPageMax
	}
	u := fmt.Sprintf("%s/v1/agents/%s/messages?%s&limit=%d",
		p.HTTPBaseURL, url.PathEscape(p.AgentEmail), query, limit)
	st, body, err := p.do(ctx, http.MethodGet, u, nil)
	if err != nil || st != http.StatusOK {
		return nil
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil
	}
	ids := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		if it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// deleteMessage trashes (permanent=false) or purges (permanent=true) one
// message, reporting whether the row actually moved. A 409 — a message held for
// review, or one whose provider submission is still in flight — counts as
// skipped and is retried next tick, rather than being booked as done.
func (p *Probe) deleteMessage(ctx context.Context, id string, permanent bool) bool {
	u := p.HTTPBaseURL + "/v1/agents/" + url.PathEscape(p.AgentEmail) +
		"/messages/" + url.PathEscape(id)
	if permanent {
		u += "?permanent=true&confirm=DELETE"
	}
	st, _, err := p.do(ctx, http.MethodDelete, u, nil)
	return err == nil && st == http.StatusOK
}
