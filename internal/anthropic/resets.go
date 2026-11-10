package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// ResetCredit is a grant of limit resets Anthropic gave the account, such as one for a model launch:
// spending a reset clears the account's usage limits at once.
type ResetCredit struct {
	ID     string
	Left   int
	EndsAt time.Time // zero = never
}

// resetProgram names the reset credits in the usage answer (cedar_ember) and in a claim.
const resetProgram = "cedar_ember"

// resetCredits reads the cedar_ember block: the grants usable now, the one Anthropic spends next first;
// nil when the account is not eligible, has none, or the block names a next grant it does not hold.
func resetCredits(raw json.RawMessage) []ResetCredit {
	var ce struct {
		Eligible bool              `json:"eligible"`
		Grants   []json.RawMessage `json:"grants"`
		Next     string            `json:"next_grant_id"`
	}
	if json.Unmarshal(raw, &ce) != nil || !ce.Eligible {
		return nil
	}
	var out []ResetCredit
	for _, g := range ce.Grants {
		var r struct {
			ID     string     `json:"id"`
			Left   int        `json:"resets_left"`
			EndsAt *time.Time `json:"ends_at"`
			Paused bool       `json:"paused"`
			Usable bool       `json:"usable_now"`
		}
		if json.Unmarshal(g, &r) != nil || r.ID == "" || r.Left <= 0 || r.Paused || !r.Usable {
			continue
		}
		c := ResetCredit{ID: r.ID, Left: r.Left}
		if r.EndsAt != nil {
			c.EndsAt = *r.EndsAt
		}
		out = append(out, c)
	}
	i := slices.IndexFunc(out, func(c ResetCredit) bool { return c.ID == ce.Next })
	if i < 0 {
		return nil
	}
	next := out[i]
	copy(out[1:i+1], out[:i])
	out[0] = next
	return out
}

// resetBlock is credits as the usage endpoint answers them, for Body.
func resetBlock(credits []ResetCredit) map[string]any {
	type grant struct {
		ID     string     `json:"id"`
		Left   int        `json:"resets_left"`
		EndsAt *time.Time `json:"ends_at"`
		Usable bool       `json:"usable_now"`
	}
	gs := make([]grant, 0, len(credits))
	for _, c := range credits {
		g := grant{ID: c.ID, Left: c.Left, Usable: true}
		if !c.EndsAt.IsZero() {
			g.EndsAt = &c.EndsAt
		}
		gs = append(gs, g)
	}
	return map[string]any{"eligible": true, "grants": gs, "next_grant_id": credits[0].ID}
}

// Reset spends one reset of credit on the account the token belongs to, in its organization org. The
// claim lands once per requestID, so a retry with the same one is safe. result is Anthropic's word for
// a 2xx: reset, already_used, not_limited, cooldown, ineligible or unavailable.
func (c *Client) Reset(ctx context.Context, accessToken, org, credit, requestID string) (status int, result string, err error) {
	body, _ := json.Marshal(map[string]string{"program": resetProgram, "grant_id": credit, "request_id": requestID})
	status, _, b, err := c.do(ctx, http.MethodPost, c.APIBase+"/api/organizations/"+url.PathEscape(org)+"/reset_rate_limits", body, map[string]string{
		"Authorization":  "Bearer " + accessToken,
		"anthropic-beta": "oauth-2025-04-20",
		"Content-Type":   "application/json",
		"User-Agent":     c.claudeAgent(ctx),
	})
	if err != nil || status/100 != 2 {
		return status, "", err
	}
	var r struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Result == "" {
		return status, "", fmt.Errorf("reset: bad response: %.200s", b)
	}
	return status, r.Result, nil
}

// claudeAgent is the user agent of Claude Code's own version: Anthropic offers reset credits only to a
// recent enough Claude Code. Without a version it is the client's own.
func (c *Client) claudeAgent(ctx context.Context) string {
	if c.ClaudeVersion != nil {
		if v := c.ClaudeVersion(ctx); v != "" {
			return "claude-cli/" + v + " (external, cli)"
		}
	}
	return c.UserAgent
}
