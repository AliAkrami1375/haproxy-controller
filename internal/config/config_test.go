package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The session secret must be generated once and then persisted, so a restart
// reuses it. If it changed on every boot, sessions and the encrypted assistant
// key would break across restarts.
func TestSessionSecretIsStableAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "controller.json")

	// Minimal file with no session_secret, like the container entrypoint writes.
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(cfgPath, []byte(`{"data_dir":"`+dir+`","db_path":"`+dir+`/c.db"}`), 0o600))

	first, err := Load([]string{"-config", cfgPath})
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.SessionSecret == "" {
		t.Fatal("no session secret was generated")
	}

	second, err := Load([]string{"-config", cfgPath})
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.SessionSecret != first.SessionSecret {
		t.Errorf("session secret changed across loads:\n first  = %s\n second = %s",
			first.SessionSecret, second.SessionSecret)
	}

	// And it must actually be on disk.
	data, _ := os.ReadFile(cfgPath)
	if !contains(string(data), first.SessionSecret) {
		t.Error("the generated secret was not written to the config file")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
