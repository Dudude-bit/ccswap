package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type Window struct {
	Utilization float64   // 0-100
	ResetsAt    time.Time // zero = unknown
}

type ModelWindow struct {
	Name string
	Window
}

type Usage struct {
	FiveHour, SevenDay *Window
	Models             []ModelWindow
}

type UsageResult struct {
	Status     int
	Body       []byte        // raw upstream body
	Usage      Usage         // parsed only for 2xx
	RetryAfter time.Duration // from a 429's Retry-After (seconds); 0 when absent
}

type rawWindow struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

func (r *rawWindow) win() *Window {
	if r == nil {
		return nil
	}
	w := &Window{Utilization: r.Utilization}
	if r.ResetsAt != nil {
		w.ResetsAt = *r.ResetsAt
	}
	return w
}

// window parses one optional window; a malformed one counts as absent.
func window(raw json.RawMessage) *Window {
	var r *rawWindow
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return r.win()
}

// ParseUsage reads the usage payload part by part: a malformed window or limits[] entry is
// skipped, only a payload that is not a JSON object is an error.
func ParseUsage(b []byte) (Usage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return Usage{}, fmt.Errorf("usage: bad response: %w", err)
	}
	u := Usage{FiveHour: window(top["five_hour"]), SevenDay: window(top["seven_day"])}
	for _, m := range []struct{ name, key string }{{"opus", "seven_day_opus"}, {"sonnet", "seven_day_sonnet"}} {
		if w := window(top[m.key]); w != nil {
			u.Models = append(u.Models, ModelWindow{m.name, *w})
		}
	}
	var limits []json.RawMessage
	_ = json.Unmarshal(top["limits"], &limits)
	for _, raw := range limits {
		var l struct {
			Kind     string     `json:"kind"`
			Percent  *float64   `json:"percent"`
			ResetsAt *time.Time `json:"resets_at"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		name := l.Scope.Model.DisplayName
		if l.Kind != "weekly_scoped" || name == "" || l.Percent == nil {
			continue
		}
		w := Window{Utilization: *l.Percent}
		if l.ResetsAt != nil {
			w.ResetsAt = *l.ResetsAt
		}
		u.Models = append(u.Models, ModelWindow{name, w})
	}
	return u, nil
}

func (c *Client) Usage(ctx context.Context, accessToken string) (UsageResult, error) {
	status, hdr, b, err := c.do(ctx, http.MethodGet, c.APIBase+"/api/oauth/usage", nil, map[string]string{
		"Authorization":  "Bearer " + accessToken,
		"anthropic-beta": "oauth-2025-04-20",
	})
	if err != nil {
		return UsageResult{}, err
	}
	res := UsageResult{Status: status, Body: b}
	if n, err := strconv.Atoi(hdr.Get("Retry-After")); err == nil && n > 0 {
		res.RetryAfter = time.Duration(n) * time.Second
	}
	if status/100 == 2 {
		if res.Usage, err = ParseUsage(b); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Body is u as the usage endpoint answers it, for clients that want the raw body: ParseUsage reads
// it back to u.
func Body(u Usage) []byte {
	type window struct {
		Utilization float64    `json:"utilization"`
		ResetsAt    *time.Time `json:"resets_at"`
	}
	win := func(w Window) *window {
		r := &window{Utilization: w.Utilization}
		if !w.ResetsAt.IsZero() {
			r.ResetsAt = &w.ResetsAt
		}
		return r
	}
	type limit struct {
		Kind     string     `json:"kind"`
		Percent  float64    `json:"percent"`
		ResetsAt *time.Time `json:"resets_at"`
		Scope    struct {
			Model struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	top := map[string]any{}
	if u.FiveHour != nil {
		top["five_hour"] = win(*u.FiveHour)
	}
	if u.SevenDay != nil {
		top["seven_day"] = win(*u.SevenDay)
	}
	var limits []limit
	for _, m := range u.Models {
		if m.Name == "opus" || m.Name == "sonnet" {
			top["seven_day_"+m.Name] = win(m.Window)
			continue
		}
		l := limit{Kind: "weekly_scoped", Percent: m.Utilization, ResetsAt: win(m.Window).ResetsAt}
		l.Scope.Model.DisplayName = m.Name
		limits = append(limits, l)
	}
	if limits != nil {
		top["limits"] = limits
	}
	b, _ := json.Marshal(top)
	return b
}
