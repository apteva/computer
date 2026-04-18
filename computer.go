// Package computer provides a factory for creating Computer implementations.
// The Computer interface is defined in github.com/apteva/core/pkg/computer.
// This package holds the concrete implementations (Browserbase, service, etc.)
package computer

import (
	"fmt"

	"github.com/apteva/core/pkg/computer"
	"github.com/apteva/computer/browserbase"
	"github.com/apteva/computer/local"
	"github.com/apteva/computer/service"
)

// Config holds the configuration for creating a Computer.
type Config struct {
	Type      string `json:"type"`                 // "browserbase", "service"
	URL       string `json:"url,omitempty"`        // for "service" type
	APIKey    string `json:"api_key,omitempty"`    // for "browserbase"
	ProjectID string `json:"project_id,omitempty"` // for "browserbase"
	Width     int    `json:"width,omitempty"`      // display width (default 2000)
	Height    int    `json:"height,omitempty"`     // display height (default 1000)

	// Browserbase-only extended options. Omit any field to use
	// Browserbase's server-side default.
	KeepAlive     bool           `json:"keep_alive,omitempty"`
	Region        string         `json:"region,omitempty"`
	Timeout       int            `json:"timeout,omitempty"`
	Proxies       any            `json:"proxies,omitempty"`
	Fingerprint   map[string]any `json:"fingerprint,omitempty"`
	ExtensionID   string         `json:"extension_id,omitempty"`
	SolveCaptchas bool           `json:"solve_captchas,omitempty"`
	UserMetadata  map[string]any `json:"user_metadata,omitempty"`
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
	case "service":
		return service.New(cfg.URL, display)
	case "local":
		return local.New(display)
	default:
		return nil, fmt.Errorf("unknown computer type: %s", cfg.Type)
	}
}
