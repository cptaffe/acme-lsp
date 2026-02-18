package acmelsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"unicode/utf8"

	"9fans.net/acme-lsp/internal/acmeutil"
	"9fans.net/acme-lsp/internal/lsp/text"
	"9fans.net/internal/go-lsp/lsp/protocol"
)

// StyleMap maps semantic token type name to a style index as defined in
// the acme style file (index 0 = default/unset; named entries start at 1).
type StyleMap map[string]int

// LoadStyleFile parses an acme style file and returns a name→index map.
// Index 0 is the "default" entry; subsequent named entries are 1, 2, …
// Lines beginning with '#' are ignored.
func LoadStyleFile(path string) (StyleMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sm := make(StyleMap)
	idx := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sm[fields[0]] = idx
		idx++
	}
	return sm, sc.Err()
}

// semanticLegend extracts the SemanticTokensLegend from the server's
// initialization result.  SemanticTokensProvider is typed interface{} in the
// protocol library; re-marshalling through JSON is the standard way to obtain
// a typed value from the decoded map[string]interface{}.
func semanticLegend(caps protocol.ServerCapabilities) (*protocol.SemanticTokensLegend, bool) {
	if caps.SemanticTokensProvider == nil {
		return nil, false
	}
	b, err := json.Marshal(caps.SemanticTokensProvider)
	if err != nil {
		return nil, false
	}
	var opts protocol.SemanticTokensOptions
	if err := json.Unmarshal(b, &opts); err != nil {
		return nil, false
	}
	if len(opts.Legend.TokenTypes) == 0 {
		return nil, false
	}
	return &opts.Legend, true
}

// runeOffsets maps line numbers to rune offsets for translating LSP
// (line, character) positions into acme rune offsets.
type runeOffsets struct {
	lineStart []int // lineStart[i] = rune offset of first rune on line i
	total     int
}

func buildRuneOffsets(body []byte) *runeOffsets {
	starts := []int{0}
	n := 0
	for len(body) > 0 {
		r, sz := utf8.DecodeRune(body)
		body = body[sz:]
		n++
		if r == '\n' {
			starts = append(starts, n)
		}
	}
	return &runeOffsets{lineStart: starts, total: n}
}

func (ro *runeOffsets) toRuneOffset(line, col int) int {
	if line >= len(ro.lineStart) {
		return ro.total
	}
	o := ro.lineStart[line] + col
	if o > ro.total {
		o = ro.total
	}
	return o
}

// buildStyleCmd converts the encoded LSP semantic token data into an acme
// style ctl command.
//
// The acme style command format used here is:
//
//	style <idx> <start> <len> [<idx> <start> <len> ...]
//
// Every entry carries an absolute start position so non-contiguous ranges
// require no gap-filling.  Returns "" if there are no mapped tokens.
func buildStyleCmd(data []uint32, legend *protocol.SemanticTokensLegend, sm StyleMap, body []byte) string {
	ro := buildRuneOffsets(body)

	var b bytes.Buffer
	first := true
	var line, col uint32

	for i := 0; i+4 < len(data); i += 5 {
		deltaLine := data[i]
		deltaCol := data[i+1]
		length := data[i+2]
		tokenType := data[i+3]
		// data[i+4] = tokenModifiers (unused for now)

		if deltaLine != 0 {
			line += deltaLine
			col = deltaCol
		} else {
			col += deltaCol
		}

		if int(tokenType) >= len(legend.TokenTypes) {
			continue
		}
		styleIdx, ok := sm[legend.TokenTypes[tokenType]]
		if !ok || styleIdx == 0 {
			continue // unmapped or default style — skip
		}

		start := ro.toRuneOffset(int(line), int(col))
		// LSP encodes length in UTF-16 code units; we approximate with rune count,
		// which is exact for BMP content (the common case for source code).
		if first {
			fmt.Fprintf(&b, "style %d %d %d", styleIdx, start, int(length))
			first = false
		} else {
			fmt.Fprintf(&b, " %d %d %d", styleIdx, start, int(length))
		}
	}
	return b.String()
}

// ApplySemanticTokens fetches semantic tokens for the named document from the
// LSP server and writes the corresponding style ctl commands to the acme window.
func ApplySemanticTokens(ctx context.Context, c *Client, w *acmeutil.Win, name string, sm StyleMap) error {
	if len(sm) == 0 {
		return nil
	}
	legend, ok := semanticLegend(c.initializeResult.Capabilities)
	if !ok {
		return nil
	}

	result, err := c.SemanticTokensFull(ctx, &protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: text.ToURI(name),
		},
	})
	if err != nil {
		if Verbose {
			log.Printf("SemanticTokensFull %v: %v", name, err)
		}
		return nil // non-fatal: server may not be ready yet
	}
	if result == nil || len(result.Data) == 0 {
		// No tokens — clear any existing highlights.
		return w.Ctl("style 0")
	}

	body, err := w.ReadAll("body")
	if err != nil {
		return fmt.Errorf("reading body: %v", err)
	}

	cmd := buildStyleCmd(result.Data, legend, sm, body)

	// Clear existing highlights first, then apply the new ones.
	if err := w.Ctl("style 0"); err != nil {
		return err
	}
	if cmd == "" {
		return nil // no mapped tokens; clear was enough
	}
	return w.Ctl("%s", cmd)
}
