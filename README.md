# Apteva Computer

Browser control implementations for [Apteva Core](https://github.com/apteva/core). Provides the `Computer` interface for screen-based environments — navigate, click, type, scroll, screenshot.

## Implementations

| Type | Description | Requires |
|------|-------------|----------|
| `local` | Auto-launches Chrome via CDP (headless if no display) | Chrome/Chromium installed |
| `browserbase` | Cloud browser via [Browserbase](https://browserbase.com) API + CDP | API key + project ID |
| `service` | Custom HTTP API (`POST /action`, `GET /screenshot`) | Running service URL |

## Usage

```go
import aptcomputer "github.com/apteva/computer"

comp, err := aptcomputer.New(aptcomputer.Config{
    Type:   "local",    // or "browserbase", "service"
    Width:  1280,
    Height: 800,
})
defer comp.Close()

// Navigate
screenshot, _ := comp.Execute(computer.Action{Type: "navigate", URL: "https://example.com"})

// Click
screenshot, _ = comp.Execute(computer.Action{Type: "click", X: 500, Y: 300})

// Screenshot
screenshot, _ = comp.Screenshot()
```

## Tool Definitions

The package exports tool definitions for [Apteva Core](https://github.com/apteva/core):

```go
import "github.com/apteva/core/pkg/computer"

// For non-Anthropic providers (regular function tool)
def := computer.GetComputerToolDef(display)   // computer_use — screen interaction
def := computer.GetSessionToolDef()           // browser_session — URL navigation

// For Anthropic (native computer use protocol)
spec := computer.GetAnthropicToolSpec(display, "20251124")
header := computer.AnthropicBetaHeader("20251124")

// Execute actions
text, screenshot, err := computer.HandleComputerAction(comp, args)
text, screenshot, err := computer.HandleSessionAction(comp, args)
```

### Two Tools

| Tool | Purpose | Returns images |
|------|---------|---------------|
| `browser_session` | Open URLs, close, resume, status | No |
| `computer_use` | Click, type, scroll, screenshot | Yes |

`browser_session` navigates. `computer_use` interacts with what's on screen. The agent calls `browser_session` to open a page, then `computer_use` to see and interact with it.

## Optional Interfaces

Implementations can optionally provide:

```go
// SessionInfo — report session details
type SessionInfo interface {
    SessionType() string  // "local", "browserbase", "service"
    SessionID() string    // empty for local
    CurrentURL() string   // current page URL
}

// Resumable — reconnect to existing sessions
type Resumable interface {
    Resume(sessionID string) error
}
```

Both `local` and `browserbase` implement `SessionInfo`.

## Testing

```bash
# Requires Chrome/Chromium installed
RUN_COMPUTER_TESTS=1 go test -run TestComputerUse_Local -v

# Requires Browserbase credentials
BROWSERBASE_API_KEY=... BROWSERBASE_PROJECT_ID=... RUN_COMPUTER_TESTS=1 go test -v
```

## License

MIT
