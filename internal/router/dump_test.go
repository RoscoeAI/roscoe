package router

import (
	"os"
	"path/filepath"
	"testing"
)

// With DumpDir set, every request body lands in a numbered file, bytes intact
// and without headers, so two runs can be diffed.
func TestDumpDirWritesEachBody(t *testing.T) {
	dir := t.TempDir()
	rt := &Router{dumpDir: dir}
	rt.dump("/v1/messages", []byte(`{"model":"x","messages":[]}`))
	rt.dump("/v1/messages/count_tokens", []byte(`{"model":"y"}`))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	if entries[0].Name() != "001-v1_messages.json" || entries[1].Name() != "002-v1_messages_count_tokens.json" {
		t.Errorf("names = %s, %s", entries[0].Name(), entries[1].Name())
	}
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if string(b) != `{"model":"x","messages":[]}` {
		t.Errorf("body altered: %s", b)
	}
	// Off by default: nothing written, nothing fails.
	(&Router{}).dump("/v1/messages", []byte("x"))
}
