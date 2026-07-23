package acmelsp

import (
	"encoding/json"
	"strings"
	"testing"

	"9fans.net/internal/go-lsp/lsp/protocol"
)

func TestInitParamsRootURINull(t *testing.T) {
	// no root → rootUri must be null, never ""
	p := &protocol.ParamInitialize{}
	b, err := json.Marshal(&initParams{p, false})
	if err != nil { t.Fatal(err) }
	s := string(b)
	if !strings.Contains(s, `"rootUri":null`) {
		t.Errorf("no-root: expected rootUri null; got %s", s)
	}
	if strings.Contains(s, `"rootUri":""`) {
		t.Errorf("no-root: rootUri must not be empty string; got %s", s)
	}

	// with root → rootUri preserved
	p2 := &protocol.ParamInitialize{}
	p2.RootURI = protocol.DocumentURI("file:///home/x")
	b2, err := json.Marshal(&initParams{p2, true})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(string(b2), `"rootUri":"file:///home/x"`) {
		t.Errorf("with-root: expected rootUri preserved; got %s", string(b2))
	}
}
