// Package computer provides a factory for creating Computer implementations.
// The Computer interface is defined in github.com/apteva/core/pkg/computer.
// This package holds the concrete implementations (Browserbase, service, etc.)
package computer

import (
	"fmt"

	"github.com/apteva/core/pkg/computer"
	"github.com/apteva/computer/browserbase"
	"github.com/apteva/computer/browserengine"
	"github.com/apteva/computer/local"
	"github.com/apteva/computer/service"
	"github.com/apteva/computer/steel"
)

// Config holds the configuration for creating a Computer. Context
// binding and session-id attaches are NOT here on purpose — those are
// agent-runtime decisions, made via browser_session.open's context_id /
// session_id parameters at tool-call time, not at instance boot.
type Config struct {
	Type      string `json:"type"`                 // "browserbase", "steel", "browser-engine", "service", "local"
	URL       string `json:"url,omitempty"`        // for "service" type; also BaseURL override for "browser-engine"
	APIKey    string `json:"api_key,omitempty"`    // for "browserbase", "steel", "browser-engine"
	ProjectID string `json:"project_id,omitempty"` // for "browserbase"
	Width     int    `json:"width,omitempty"`      // display width (default 2000)
	Height    int    `json:"height,omitempty"`     // display height (default 1000)

	// Browserbase-only extended options. Omit any field to use
	// Browserbase's server-side default.
	KeepAlive bool   `json:"keep_alive,omitempty"`
	Region    string `json:"region,omitempty"`

	// Timeout is the max server-side session lifetime, in SECONDS,
	// applied at creation. All three cloud backends honor this (the
	// factory converts to milliseconds internally for Steel, whose
	// API expects ms). Browserbase / Steel cannot extend post-create
	// — set it generously up front. Browser Engine also supports
	// extension via Timeoutable.ExtendTimeout for mid-session bumps.
	Timeout int `json:"timeout,omitempty"`
	Proxies       any            `json:"proxies,omitempty"`
	Fingerprint   map[string]any `json:"fingerprint,omitempty"`
	ExtensionID   string         `json:"extension_id,omitempty"`
	SolveCaptchas bool           `json:"solve_captchas,omitempty"`
	UserMetadata  map[string]any `json:"user_metadata,omitempty"`

	// Steel-only extended options. Region/Timeout are shared with
	// Browserbase above.
	BlockAds bool `json:"block_ads,omitempty"`
	// ProxyURL: explicit upstream proxy URL.
	//   - Steel: passed through as a custom proxy override.
	//   - Local: the proxy the agent gets when it calls
	//     browser_session(open, proxy=true). Schemes http, https,
	//     socks5; inline credentials honored via CDP auth handler.
	//   - Cloud backends with their own managed proxy
	//     (browser-engine, browserbase) ignore this field.
	ProxyURL string `json:"proxy_url,omitempty"`
	UseProxy bool   `json:"use_proxy,omitempty"`
	SolveCaptcha bool   `json:"solve_captcha,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`

	// Browser Engine–only extended options. InitialURL and Metadata
	// are also accepted (InitialURL seeds first nav server-side,
	// Metadata attaches to the session record).
	InitialURL       string         `json:"initial_url,omitempty"`
	BrowserProjectID int            `json:"browser_project_id,omitempty"`
	ProxyEnabled     bool           `json:"proxy_enabled,omitempty"`
	ProxyCountry     string         `json:"proxy_country,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// New creates a Computer from config. Returns nil if type is empty.
func New(cfg Config) (computer.Computer, error) {
	if cfg.Type == "" {
		return nil, nil
	}
	// Fallback viewport when the caller didn't pass one. Callers that
	// know the target provider (core/api.go, server/instances.go) pick
	// a smarter default: 1024×768 for Anthropic's native computer-use
	// tool (trained on that size), 1600×800 (2:1 widescreen) for
	// everyone else. This factory only sees it when a caller forgets —
	// keep the widescreen default to match the common case.
	width := cfg.Width
	if width == 0 {
		width = 1600
	}
	height := cfg.Height
	if height == 0 {
		height = 800
	}
	display := computer.DisplaySize{Width: width, Height: height}

	switch cfg.Type {
	case "browserbase":
		return browserbase.NewWithOptions(cfg.APIKey, cfg.ProjectID, display, browserbase.Options{
			KeepAlive:     cfg.KeepAlive,
			Region:        cfg.Region,
			Timeout:       cfg.Timeout,
			Proxies:       cfg.Proxies,
			Fingerprint:   cfg.Fingerprint,
			ExtensionID:   cfg.ExtensionID,
			SolveCaptchas: cfg.SolveCaptchas,
			UserMetadata:  cfg.UserMetadata,
		})
	case "steel":
		// Steel's API takes timeout in milliseconds; Config.Timeout is
		// seconds (consistent with Browserbase / Browser Engine).
		// Convert here so callers don't have to remember per-provider
		// units.
		steelTimeoutMs := cfg.Timeout * 1000
		return steel.NewWithOptions(cfg.APIKey, display, steel.Options{
			BlockAds:     cfg.BlockAds,
			ProxyURL:     cfg.ProxyURL,
			UseProxy:     cfg.UseProxy,
			Region:       cfg.Region,
			Timeout:      steelTimeoutMs,
			SolveCaptcha: cfg.SolveCaptcha,
			UserAgent:    cfg.UserAgent,
		})
	case "browser-engine":
		var proxy *browserengine.ProxyConfig
		if cfg.ProxyEnabled || cfg.ProxyCountry != "" {
			proxy = &browserengine.ProxyConfig{
				Enabled: cfg.ProxyEnabled,
				Country: cfg.ProxyCountry,
			}
		}
		return browserengine.NewWithOptions(cfg.APIKey, display, browserengine.Options{
			BaseURL:    cfg.URL,
			InitialURL: cfg.InitialURL,
			UserAgent:  cfg.UserAgent,
			Timeout:    cfg.Timeout,
			Proxy:      proxy,
			ProjectID:  cfg.BrowserProjectID,
			Metadata:   cfg.Metadata,
		})
	case "service":
		return service.New(cfg.URL, display)
	case "local":
		return local.NewWithOptions(display, local.Options{ProxyURL: cfg.ProxyURL})
	default:
		return nil, fmt.Errorf("unknown computer type: %s", cfg.Type)
	}
}
