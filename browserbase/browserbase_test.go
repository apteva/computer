package browserbase

import (
	"os"
	"testing"

	"github.com/apteva/core/pkg/computer"
)

func getTestCreds(t *testing.T) (string, string) {
	t.Helper()
	key := os.Getenv("BROWSERBASE_API_KEY")
	project := os.Getenv("BROWSERBASE_PROJECT_ID")
	if key == "" || project == "" {
		t.Skip("BROWSERBASE_API_KEY or BROWSERBASE_PROJECT_ID not set")
	}
	return key, project
}

func TestSession_CreateAndClose(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Logf("session: %s", comp.sessionID)
	if comp.sessionID == "" {
		t.Fatal("no session ID")
	}

	d := comp.DisplaySize()
	if d.Width != 1280 || d.Height != 800 {
		t.Errorf("display: %dx%d", d.Width, d.Height)
	}

	if err := comp.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	t.Log("session closed")
}

func TestScreenshot_BlankPage(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	screenshot, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if len(screenshot) < 100 {
		t.Fatalf("screenshot too small: %d bytes", len(screenshot))
	}
	t.Logf("blank page screenshot: %d bytes", len(screenshot))
}

func TestNavigate_Example(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	screenshot, err := comp.Execute(computer.Action{
		Type: "navigate",
		URL:  "https://example.com",
	})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if len(screenshot) < 1000 {
		t.Fatalf("screenshot after navigate too small: %d bytes", len(screenshot))
	}
	t.Logf("navigate screenshot: %d bytes", len(screenshot))
}

func TestClick(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	// Navigate first
	_, err = comp.Execute(computer.Action{Type: "navigate", URL: "https://example.com"})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Click somewhere
	screenshot, err := comp.Execute(computer.Action{Type: "click", X: 640, Y: 300})
	if err != nil {
		t.Fatalf("click: %v", err)
	}
	t.Logf("click screenshot: %d bytes", len(screenshot))
}

func TestType(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	// Navigate to google
	_, err = comp.Execute(computer.Action{Type: "navigate", URL: "https://www.google.com"})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Click search box area
	_, err = comp.Execute(computer.Action{Type: "click", X: 640, Y: 340})
	if err != nil {
		t.Fatalf("click: %v", err)
	}

	// Type
	screenshot, err := comp.Execute(computer.Action{Type: "type", Text: "apteva ai"})
	if err != nil {
		t.Fatalf("type: %v", err)
	}
	t.Logf("type screenshot: %d bytes", len(screenshot))
}

func TestScroll(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	_, err = comp.Execute(computer.Action{Type: "navigate", URL: "https://en.wikipedia.org/wiki/Artificial_intelligence"})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	screenshot, err := comp.Execute(computer.Action{Type: "scroll", X: 640, Y: 400, Direction: "down", Amount: 5})
	if err != nil {
		t.Fatalf("scroll: %v", err)
	}
	t.Logf("scroll screenshot: %d bytes", len(screenshot))
}

func TestFullFlow(t *testing.T) {
	key, project := getTestCreds(t)

	comp, err := New(key, project, computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer comp.Close()

	// 1. Screenshot blank
	s1, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("1. screenshot: %v", err)
	}
	t.Logf("1. blank: %d bytes", len(s1))

	// 2. Navigate
	s2, err := comp.Execute(computer.Action{Type: "navigate", URL: "https://jsonplaceholder.typicode.com/todos/1"})
	if err != nil {
		t.Fatalf("2. navigate: %v", err)
	}
	t.Logf("2. navigate: %d bytes", len(s2))

	// 3. Wait
	s3, err := comp.Execute(computer.Action{Type: "wait", Duration: 1000})
	if err != nil {
		t.Fatalf("3. wait: %v", err)
	}
	t.Logf("3. wait: %d bytes", len(s3))

	// 4. Screenshot should differ from blank
	if len(s2) == len(s1) {
		t.Log("warning: navigate screenshot same size as blank — page may not have loaded")
	}

	t.Log("full flow passed")
}
