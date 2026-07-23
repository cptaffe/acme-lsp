package acmelsp

import (
	"io/ioutil"
	"testing"

	"9fans.net/acme-lsp/internal/lsp/acmelsp/config"
)

// TestDocStateReset verifies that reset clears the cached document state and
// that the version counter restarts from 1.  A restarted language server has an
// empty document store, so the next didOpen/didChange must begin at version 1;
// terraform-ls (and others) silently drop a didChange whose version is not
// greater than the last one they saw, which would starve semantic tokens.
func TestDocStateReset(t *testing.T) {
	d := &docState{
		version:  7,
		resultID: "abc",
		data:     []uint32{1, 2, 3},
	}

	d.reset()

	if d.version != 0 {
		t.Errorf("after reset, version is %d; want 0", d.version)
	}
	if d.resultID != "" {
		t.Errorf("after reset, resultID is %q; want empty", d.resultID)
	}
	if d.data != nil {
		t.Errorf("after reset, data is %v; want nil", d.data)
	}
	if got := d.nextVersion(); got != 1 {
		t.Errorf("first version after reset is %d; want 1", got)
	}
}

// fakeResyncer records whether ResyncFiles was called.
type fakeResyncer struct {
	called bool
}

func (f *fakeResyncer) ResyncFiles() { f.called = true }

// TestClientConfigPropagatesResyncer verifies that ClientConfig carries the
// ServerSet's resyncer through to the client.  The restart goroutine reads
// ClientConfig.Resyncer to replay didOpen after a server restart; if it is not
// propagated, restarted servers keep an empty document store and every request
// fails with "document not found".
func TestClientConfigPropagatesResyncer(t *testing.T) {
	cfg := &config.Config{
		File: config.File{
			RootDirectory: "/",
			Servers: map[string]*config.Server{
				"gopls": {Command: []string{"gopls"}},
			},
			FilenameHandlers: []config.FilenameHandler{
				{Pattern: `\.go$`, ServerKey: "gopls"},
			},
		},
	}
	ss, err := NewServerSet(cfg, &mockDiagosticsWriter{ioutil.Discard})
	if err != nil {
		t.Fatalf("NewServerSet: %v", err)
	}
	defer ss.CloseAll()

	fr := &fakeResyncer{}
	ss.resyncer = fr

	if len(ss.Data) == 0 {
		t.Fatalf("ServerSet has no server info")
	}
	cc := ss.ClientConfig(ss.Data[0])
	if cc.Resyncer == nil {
		t.Fatal("ClientConfig.Resyncer is nil; resyncer was not propagated")
	}
	cc.Resyncer.ResyncFiles()
	if !fr.called {
		t.Error("ClientConfig.Resyncer does not point at the ServerSet resyncer")
	}
}

// TestFileManagerImplementsResyncer verifies at compile time that FileManager
// satisfies the ServerResyncer interface, the contract the restart goroutine
// depends on.
func TestFileManagerImplementsResyncer(t *testing.T) {
	var _ ServerResyncer = (*FileManager)(nil)
}
