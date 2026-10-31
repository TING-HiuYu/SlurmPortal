package eventbus

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLocalTokensRemainPrivateStableAndSeparateFromMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event-tokens.json")
	const count = 16
	results := make(chan LocalTokens, count)
	errors := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens, err := LoadOrCreateLocalTokens(path)
			if err != nil {
				errors <- err
				return
			}
			results <- tokens
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first LocalTokens
	for tokens := range results {
		if first.ReadOnly == "" {
			first = tokens
		}
		if tokens != first {
			t.Fatal("racing initializers used different credentials")
		}
	}
	if !first.CanRead(first.ReadOnly) || first.CanWrite(first.ReadOnly) || !first.CanRead(first.ReadWrite) || !first.CanWrite(first.ReadWrite) || first.CanRead("") {
		t.Fatal("token scopes not enforced")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials mode %o", info.Mode().Perm())
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOrCreateLocalTokens(path); err == nil {
		t.Fatal("world-readable credentials accepted")
	}
}
