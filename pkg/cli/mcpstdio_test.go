package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/pkg/mcptools"
	"github.com/nexustar/usher/pkg/router"
)

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestValidateImage_SuccessReturnsDimsJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chart.png")
	writeTestPNG(t, p, 640, 480)

	text, isErr := validateImage(p)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}
	var got struct {
		Message string `json:"message"`
		W       int    `json:"w"`
		H       int    `json:"h"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result is not JSON: %q (%v)", text, err)
	}
	if got.W != 640 || got.H != 480 {
		t.Errorf("dims = %dx%d, want 640x480", got.W, got.H)
	}
	if got.Message == "" {
		t.Error("expected a human-facing message field")
	}
}

func TestValidateImage_Errors(t *testing.T) {
	cases := []struct{ name, path string }{
		{"empty", ""},
		{"not an image", filepath.Join(t.TempDir(), "notes.txt")},
		{"missing", filepath.Join(t.TempDir(), "nope.png")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, isErr := validateImage(c.path)
			if !isErr {
				t.Errorf("expected isError for %q", c.path)
			}
		})
	}
}

func TestMCPRouterToolsAreOptIn(t *testing.T) {
	t.Setenv("USHER_HOOK_SOCK", filepath.Join(t.TempDir(), "hook.sock"))
	if got := len(mcpTools(nil)); got != 1 {
		t.Fatalf("default tool count = %d, want show_image only", got)
	}
	tools := mcpTools(&mcptools.Router{})
	if want := 1 + len(mcptools.RouterTools()); len(tools) != want {
		t.Fatalf("router tool count = %d, want %d", len(tools), want)
	}
	if tools[0]["name"] != "show_image" || tools[1]["name"] != "list_sessions" {
		t.Fatalf("tool order = %q, %q", tools[0]["name"], tools[1]["name"])
	}
}

// A session that usher did not spawn gets no tools at all, router flag or not.
func TestMCPUnmanagedSessionGetsNoTools(t *testing.T) {
	t.Setenv("USHER_HOOK_SOCK", "")
	if got := len(mcpTools(&mcptools.Router{})); got != 0 {
		t.Fatalf("unmanaged tool count = %d, want 0", got)
	}
}

func TestMCPToolsListDefaultsToImageOnly(t *testing.T) {
	t.Setenv("USHER_HOOK_SOCK", filepath.Join(t.TempDir(), "hook.sock"))
	var raw bytes.Buffer
	out := bufio.NewWriter(&raw)
	err := handleMCPLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), out, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Flush(); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0]["name"] != "show_image" {
		t.Fatalf("tools = %#v", resp.Result.Tools)
	}
}

// The pi router's allowlist names the routing tools one by one; a tool added
// to the catalog must be added there too, or pi's router never sees it.
func TestPiRouterAllowlistCoversTheCatalog(t *testing.T) {
	allowed := "," + router.PiRouterTools() + ","
	for _, tool := range mcptools.RouterTools() {
		if !strings.Contains(allowed, ",mcp__usher__"+tool.Name+",") {
			t.Errorf("pi allowlist is missing %s", tool.Name)
		}
	}
}
