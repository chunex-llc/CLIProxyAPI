package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	paceClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	paceCodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	paceProbeTimeout   = 20 * time.Second
)

// paceProbe polls the usage endpoint of every Claude and Codex credential and
// records the windows it reports in the pace ledger. Responses relayed by the
// proxy carry only account-wide windows; model-scoped weekly limits are only
// visible here.
type paceProbe struct {
	manager        *coreauth.Manager
	ledger         *coreauth.PaceLedger
	client         *http.Client
	claudeUsageURL string
	codexUsageURL  string
	interval       time.Duration
	afterCycle     func()
}

func newPaceProbe(cfg *config.Config, manager *coreauth.Manager, ledger *coreauth.PaceLedger, interval time.Duration) *paceProbe {
	client := &http.Client{Timeout: paceProbeTimeout}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &paceProbe{
		manager:        manager,
		ledger:         ledger,
		client:         client,
		claudeUsageURL: paceClaudeUsageURL,
		codexUsageURL:  paceCodexUsageURL,
		interval:       interval,
	}
}

// Run polls once immediately and then every interval until ctx is done; each
// cycle probes the credentials one at a time. A non-positive interval
// disables polling.
func (p *paceProbe) Run(ctx context.Context) {
	if p == nil || p.interval <= 0 {
		return
	}
	p.cycle(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cycle(ctx)
		}
	}
}

// cycle probes each enabled Claude and Codex credential with an access token,
// one at a time. Failures are logged and leave the ledger untouched.
func (p *paceProbe) cycle(ctx context.Context) {
	if p.manager != nil {
		for _, auth := range p.manager.List() {
			if ctx.Err() != nil {
				break
			}
			if auth == nil || auth.Disabled || paceAccessToken(auth) == "" {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if provider != "claude" && provider != "codex" {
				continue
			}
			if errProbe := p.probe(ctx, auth); errProbe != nil {
				log.WithError(errProbe).Debugf("pace probe failed for auth %s", auth.ID)
			}
		}
	}
	if p.afterCycle != nil {
		p.afterCycle()
	}
}

func (p *paceProbe) probe(ctx context.Context, auth *coreauth.Auth) error {
	token := paceAccessToken(auth)
	if token == "" {
		return fmt.Errorf("auth %s has no access token", auth.ID)
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	url := p.claudeUsageURL
	if provider == "codex" {
		url = p.codexUsageURL
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	switch provider {
	case "claude":
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	case "codex":
		req.Header.Set("OpenAI-Beta", "codex-1")
		req.Header.Set("Originator", "Codex Desktop")
		if accountID, _ := auth.Metadata["account_id"].(string); strings.TrimSpace(accountID) != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	default:
		return fmt.Errorf("provider %q has no usage endpoint", auth.Provider)
	}

	resp, errDo := p.client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("pace probe: close usage response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}

	at := time.Now()
	var obs map[coreauth.PaceWindowKey]coreauth.PaceObservation
	var errParse error
	if provider == "codex" {
		obs, errParse = parseCodexUsage(body, at)
	} else {
		obs, errParse = parseClaudeUsage(body, at)
	}
	if errParse != nil {
		return errParse
	}
	p.ledger.Observe(auth.ID, obs)
	return nil
}

func paceAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeUsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// parseClaudeUsage reads the account-wide five-hour and seven-day windows and
// every weekly model-scoped limit from a Claude OAuth usage response.
func parseClaudeUsage(body []byte, at time.Time) (map[coreauth.PaceWindowKey]coreauth.PaceObservation, error) {
	var payload struct {
		FiveHour *claudeUsageWindow `json:"five_hour"`
		SevenDay *claudeUsageWindow `json:"seven_day"`
		Limits   []json.RawMessage  `json:"limits"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	obs := make(map[coreauth.PaceWindowKey]coreauth.PaceObservation)
	addWindow := func(key coreauth.PaceWindowKey, window *claudeUsageWindow, duration time.Duration) {
		if window == nil || window.Utilization == nil {
			return
		}
		obs[key] = coreauth.PaceObservation{
			UsedPercent: *window.Utilization,
			ResetAt:     paceParseRFC3339(window.ResetsAt),
			Duration:    duration,
			ObservedAt:  at,
		}
	}
	addWindow("5h", payload.FiveHour, 5*time.Hour)
	addWindow("7d", payload.SevenDay, 7*24*time.Hour)
	for _, raw := range payload.Limits {
		// A limit of an unexpected shape is skipped rather than failing the
		// account-wide windows read above.
		var limit claudeUsageLimit
		if json.Unmarshal(raw, &limit) != nil {
			continue
		}
		if limit.Kind != "weekly_scoped" || limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
		if name == "" {
			continue
		}
		obs[coreauth.PaceWindowKey("model:"+name)] = coreauth.PaceObservation{
			UsedPercent: *limit.Percent,
			ResetAt:     paceParseRFC3339(limit.ResetsAt),
			Duration:    7 * 24 * time.Hour,
			ObservedAt:  at,
		}
	}
	return obs, nil
}

func paceParseRFC3339(value *string) time.Time {
	if value == nil {
		return time.Time{}
	}
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(*value))
	if errParse != nil {
		return time.Time{}
	}
	return parsed
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            *float64 `json:"reset_at"`
	ResetAfterSeconds  *float64 `json:"reset_after_seconds"`
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
}

type codexUsageRateLimit struct {
	PrimaryWindow        *codexUsageWindow `json:"primary_window"`
	SecondaryWindow      *codexUsageWindow `json:"secondary_window"`
	AdditionalRateLimits json.RawMessage   `json:"additional_rate_limits"`
}

type codexUsageAdditional struct {
	LimitName       string               `json:"limit_name"`
	LimitNameCamel  string               `json:"limitName"`
	Name            string               `json:"name"`
	RateLimit       *codexUsageRateLimit `json:"rate_limit"`
	RateLimitCamel  *codexUsageRateLimit `json:"rateLimit"`
	PrimaryWindow   *codexUsageWindow    `json:"primary_window"`
	SecondaryWindow *codexUsageWindow    `json:"secondary_window"`
}

// parseCodexUsage reads the primary and secondary windows and the primary
// window of every additional (model-scoped) limit from a Codex usage response.
func parseCodexUsage(body []byte, at time.Time) (map[coreauth.PaceWindowKey]coreauth.PaceObservation, error) {
	var payload struct {
		RateLimit            *codexUsageRateLimit `json:"rate_limit"`
		AdditionalRateLimits json.RawMessage      `json:"additional_rate_limits"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	obs := make(map[coreauth.PaceWindowKey]coreauth.PaceObservation)
	addWindow := func(key coreauth.PaceWindowKey, window *codexUsageWindow) {
		if window == nil || window.UsedPercent == nil {
			return
		}
		observation := coreauth.PaceObservation{UsedPercent: *window.UsedPercent, ObservedAt: at}
		switch {
		case window.ResetAt != nil:
			observation.ResetAt = time.Unix(int64(*window.ResetAt), 0)
		case window.ResetAfterSeconds != nil:
			observation.ResetAt = at.Add(time.Duration(*window.ResetAfterSeconds * float64(time.Second)))
		}
		if window.LimitWindowSeconds != nil {
			observation.Duration = time.Duration(*window.LimitWindowSeconds * float64(time.Second))
		}
		obs[key] = observation
	}

	additional := payload.AdditionalRateLimits
	if payload.RateLimit != nil {
		addWindow("primary", payload.RateLimit.PrimaryWindow)
		addWindow("secondary", payload.RateLimit.SecondaryWindow)
		if paceJSONAbsent(additional) {
			additional = payload.RateLimit.AdditionalRateLimits
		}
	}
	for name, primary := range codexAdditionalPrimaryWindows(additional) {
		addWindow(coreauth.PaceWindowKey("model:"+name), primary)
	}
	return obs, nil
}

// codexAdditionalPrimaryWindows maps each lowercased additional limit name to
// its primary window. raw is either an object keyed by limit name or an array
// of named items; anything else yields nothing.
func codexAdditionalPrimaryWindows(raw json.RawMessage) map[string]*codexUsageWindow {
	if paceJSONAbsent(raw) {
		return nil
	}
	named := make(map[string]codexUsageAdditional)
	var byName map[string]codexUsageAdditional
	var items []codexUsageAdditional
	if json.Unmarshal(raw, &byName) == nil {
		named = byName
	} else if json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			name := item.LimitName
			if name == "" {
				name = item.LimitNameCamel
			}
			if name == "" {
				name = item.Name
			}
			named[name] = item
		}
	}
	windows := make(map[string]*codexUsageWindow, len(named))
	for name, item := range named {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		rateLimit := item.RateLimit
		if rateLimit == nil {
			rateLimit = item.RateLimitCamel
		}
		primary := item.PrimaryWindow
		if rateLimit != nil {
			primary = rateLimit.PrimaryWindow
		}
		if primary != nil {
			windows[name] = primary
		}
	}
	return windows
}

func paceJSONAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
