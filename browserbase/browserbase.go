// Package browserbase implements the Computer interface using the Browserbase API.
package browserbase

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/apteva/core/pkg/computer"
)

type Computer struct {
	apiKey    string
	projectID string
	sessionID string
	display   computer.DisplaySize
	client    *http.Client
}

// New creates a Browserbase-backed Computer and starts a session.
func New(apiKey, projectID string, display computer.DisplaySize) (*Computer, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("browserbase: api_key is required")
	}
	c := &Computer{
		apiKey:    apiKey,
		projectID: projectID,
		display:   display,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
	if err := c.createSession(); err != nil {
		return nil, fmt.Errorf("browserbase: failed to create session: %w", err)
	}
	return c, nil
}

func (c *Computer) createSession() error {
	body := map[string]any{
		"projectId": c.projectID,
		"browserSettings": map[string]any{
			"viewport": map[string]int{
				"width":  c.display.Width,
				"height": c.display.Height,
			},
		},
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.browserbase.com/v1/sessions", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bb-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	c.sessionID = result.ID
	return nil
}

func (c *Computer) Execute(action computer.Action) ([]byte, error) {
	switch action.Type {
	case "screenshot":
		return c.Screenshot()
	case "wait":
		dur := action.Duration
		if dur <= 0 {
			dur = 1000
		}
		time.Sleep(time.Duration(dur) * time.Millisecond)
		return c.Screenshot()
	}

	body := map[string]any{"action": action.Type}
	switch action.Type {
	case "click", "double_click":
		body["x"] = action.X
		body["y"] = action.Y
	case "type":
		body["text"] = action.Text
	case "key":
		body["key"] = action.Key
	case "scroll":
		body["x"] = action.X
		body["y"] = action.Y
		body["direction"] = action.Direction
		body["amount"] = action.Amount
	case "navigate":
		body["url"] = action.URL
	}

	data, _ := json.Marshal(body)
	url := fmt.Sprintf("https://api.browserbase.com/v1/sessions/%s/actions", c.sessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bb-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("action %s failed: %w", action.Type, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("action %s: HTTP %d: %s", action.Type, resp.StatusCode, string(respBody))
	}

	time.Sleep(200 * time.Millisecond)
	return c.Screenshot()
}

func (c *Computer) Screenshot() ([]byte, error) {
	url := fmt.Sprintf("https://api.browserbase.com/v1/sessions/%s/screenshot", c.sessionID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-bb-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("screenshot failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("screenshot: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if decoded, err := base64.StdEncoding.DecodeString(string(respBody)); err == nil {
		return decoded, nil
	}
	return respBody, nil
}

func (c *Computer) DisplaySize() computer.DisplaySize { return c.display }

func (c *Computer) Close() error {
	if c.sessionID == "" {
		return nil
	}
	url := fmt.Sprintf("https://api.browserbase.com/v1/sessions/%s", c.sessionID)
	req, _ := http.NewRequest("DELETE", url, nil)
	req.Header.Set("x-bb-api-key", c.apiKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.sessionID = ""
	return nil
}
