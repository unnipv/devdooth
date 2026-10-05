package webui

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The coordinator serves the embedded landing page and assets; the same files
// are published to GitHub Pages from docs/. These tests keep the copies in
// sync. Run `make sync-landing` after editing internal/webui/.
func TestLandingPageMatchesDocs(t *testing.T) {
	want, err := os.ReadFile("../../docs/index.html")
	if err != nil {
		t.Skipf("docs landing page not present: %v", err)
	}
	if string(indexHTML) != string(want) {
		t.Fatalf("docs/index.html is out of date with internal/webui/index.html; run `make sync-landing`")
	}
}

func TestDemoAssetsMatchDocs(t *testing.T) {
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded demo assets")
	}
	for _, e := range entries {
		embedded, err := fs.ReadFile(assets, "assets/"+e.Name())
		if err != nil {
			t.Fatalf("read embedded %s: %v", e.Name(), err)
		}
		onDisk, err := os.ReadFile(filepath.Join("../../docs/assets", e.Name()))
		if err != nil {
			t.Fatalf("docs/assets/%s is missing; run `make sync-landing`", e.Name())
		}
		if string(embedded) != string(onDisk) {
			t.Fatalf("docs/assets/%s differs from internal/webui/assets/%s; run `make sync-landing`", e.Name(), e.Name())
		}
	}
}
