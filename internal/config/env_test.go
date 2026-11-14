package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadARICredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ari.env")
	content := "# コメント\nARI_USER=drunk_zundamon\nexport ARI_PASS=\"pass word\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := LoadARICredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if creds.User != "drunk_zundamon" || creds.Password != "pass word" {
		t.Fatalf("creds=%+v", creds)
	}
}
