// Package service implements the Computer interface using a custom browser-service HTTP API.
package service

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
	url     string
	display computer.DisplaySize
	client  *http.Client
}

// New creates a Computer backed by a browser-service HTTP API.
func New(url string, display computer.DisplaySize) (*Computer, error) {
	if url == "" {
		return nil, fmt.Errorf("service: url is required")
	}
	return &Computer{
		url:     url,
		display: display,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
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

	data, _ := json.Marshal(action)
	req, err := http.NewRequest("POST", c.url+"/action", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

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
	req, err := http.NewRequest("GET", c.url+"/screenshot", nil)
	if err != nil {
		return nil, err
	}
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

func (c *Computer) Close() error { return nil }
