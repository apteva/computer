// Package computer provides a factory for creating Computer implementations.
// The Computer interface is defined in github.com/apteva/core/pkg/computer.
// This package holds the concrete implementations (Browserbase, service, etc.)
package computer

import (
	"fmt"

	"github.com/apteva/core/pkg/computer"
	"github.com/apteva/computer/browserbase"
	"github.com/apteva/computer/service"
)

// Config holds the configuration for creating a Computer.
type Config struct {
	Type      string `json:"type"`                 // "browserbase", "service"
	URL       string `json:"url,omitempty"`        // for "service" type
	APIKey    string `json:"api_key,omitempty"`    // for "browserbase"
	ProjectID string `json:"project_id,omitempty"` // for "browserbase"
	Width     int    `json:"width,omitempty"`      // display width (default 1280)
	Height    int    `json:"height,omitempty"`     // display height (default 800)
}

// New creates a Computer from config. Returns nil if type is empty.
func New(cfg Config) (computer.Computer, error) {
	if cfg.Type == "" {
		return nil, nil
	}
	width := cfg.Width
	if width == 0 {
		width = 1280
	}
	height := cfg.Height
	if height == 0 {
		height = 800
	}
	display := computer.DisplaySize{Width: width, Height: height}

	switch cfg.Type {
	case "browserbase":
		return browserbase.New(cfg.APIKey, cfg.ProjectID, display)
	case "service":
		return service.New(cfg.URL, display)
	default:
		return nil, fmt.Errorf("unknown computer type: %s", cfg.Type)
	}
}
