package acmelsp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"9fans.net/acme-lsp/internal/acmeutil"
	"9fans.net/acme-lsp/internal/lsp/text"
	"9fans.net/internal/go-lsp/lsp/protocol"
	"github.com/cptaffe/acme-styles/layer"
)

// semanticLegend extracts the SemanticTokensLegend from the server's
// initialization result.  SemanticTokensProvider is typed interface{} in the
// protocol library; re-marshalling through JSON is the standard way to obtain
// a typed value from the decoded map[string]interface{}.
//
// We unmarshal only the "legend" sub-object rather than the full
// SemanticTokensOptions so that servers like clangd that return scalar values
// for optional fields (e.g. "range": false) don't trip up the decoder — the
// protocol library's Or_SemanticTokensOptions_range type expects a JSON object,
// not a boolean, and json.Unmarshal returns an error on mismatch.
func semanticLegend(caps protocol.ServerCapabilities) (*protocol.SemanticTokensLegend, bool) {
	if caps.SemanticTokensProvider == nil {
		return nil, false
	}
	b, err := json.Marshal(caps.SemanticTokensProvider)
	if err != nil {
		return nil, false
	}
	var wrapper struct {
		Legend protocol.SemanticTokensLegend `json:"legend"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return nil, false
	}
	if len(wrapper.Legend.TokenTypes) == 0 {
		return nil, false
	}
	return &wrapper.Legend, true
}

// runeOffsets maps line numbers to rune offsets for translating LSP
// (line, character) positions into acme rune offsets.
type runeOffsets struct {
	lineStart     []int // lineStart[i] = rune offset of first rune on line i
	lineStartByte []int // lineStartByte[i] = byte offset of first byte on line i
	total         int
}

func buildRuneOffsets(body []byte) *runeOffsets {
	starts := []int{0}
	startBytes := []int{0}
	n, nb := 0, 0
	for len(body) > 0 {
		r, sz := utf8.DecodeRune(body)
		body = body[sz:]
		n++
		nb += sz
		if r == '\n' {
			starts = append(starts, n)
			startBytes = append(startBytes, nb)
		}
	}
	return &runeOffsets{lineStart: starts, lineStartByte: startBytes, total: n}
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

// blankLinesOnly returns true if every rune in body[gapStart:gapEnd] (rune
// offsets) is a newline — i.e. the gap consists only of blank lines.
func blankLinesOnly(body []byte, ro *runeOffsets, gapStart, gapEnd int) bool {
	if gapEnd <= gapStart {
		return true
	}
	// Locate the byte offset of gapStart via the line table.
	byteOff := 0
	for l := len(ro.lineStart) - 1; l >= 0; l-- {
		if ro.lineStart[l] <= gapStart {
			byteOff = ro.lineStartByte[l]
			skip := gapStart - ro.lineStart[l]
			b := body[byteOff:]
			for skip > 0 {
				_, sz := utf8.DecodeRune(b)
				b = b[sz:]
				byteOff += sz
				skip--
			}
			break
		}
	}
	b := body[byteOff:]
	for i := gapStart; i < gapEnd && len(b) > 0; i++ {
		r, sz := utf8.DecodeRune(b)
		b = b[sz:]
		if r != '\n' {
			return false
		}
	}
	return true
}

// lspPaletteNames maps LSP semantic token type names to the short palette
// entry names used by the acme-styles master palette.  Populated at startup
// from the embedded token_names.txt.  Token types not listed in the file are
// dropped so they do not occlude lower-priority layers with invisible spans.
var lspPaletteNames = parseTokenNames(tokenNamesData)

//go:embed token_names.txt
var tokenNamesData string

// parseTokenNames parses the "<palette-name> <source-name>" format used by
// token_names.txt and returns a source→palette map.
// Lines starting with '#' and blank lines are ignored.
func parseTokenNames(data string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			m[fields[1]] = fields[0] // source → palette
		}
	}
	return m
}

// buildStyleEntries converts encoded LSP semantic token data into a flat slice
// of layer.Entry values ready for writing to an acme-styles layer.
// LSP token type names are mapped to the short palette entry names defined in
// lspPaletteNames; types with no palette equivalent are dropped so they do not
// occlude runs from lower-priority layers.
func buildStyleEntries(data []uint32, legend *protocol.SemanticTokensLegend, body []byte) []layer.Entry {
	ro := buildRuneOffsets(body)

	// First pass: decode the relative encoding into absolute entries.
	entries := make([]layer.Entry, 0, len(data)/5)
	var line, col uint32
	for i := 0; i+4 < len(data); i += 5 {
		deltaLine := data[i]
		deltaCol := data[i+1]
		length := data[i+2]
		tokenType := data[i+3]
		// data[i+4] = tokenModifiers (unused)

		if deltaLine != 0 {
			line += deltaLine
			col = deltaCol
		} else {
			col += deltaCol
		}

		if int(tokenType) >= len(legend.TokenTypes) {
			continue
		}
		name := lspPaletteNames[legend.TokenTypes[tokenType]]
		if name == "" {
			continue
		}
		start := ro.toRuneOffset(int(line), int(col))
		entries = append(entries, layer.Entry{
			Name:  name,
			Start: start,
			End:   start + int(length),
		})
	}

	// Second pass: merge adjacent runs of the same name separated only by
	// blank lines.  gopls returns one token per line for multi-line constructs
	// (backtick strings, block comments) and emits nothing for blank lines
	// within them, even when multilineTokenSupport is advertised.
	merged := make([]layer.Entry, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		end := e.End
		for j := i + 1; j < len(entries); j++ {
			next := entries[j]
			if next.Name != e.Name {
				break
			}
			if !blankLinesOnly(body, ro, end, next.Start) {
				break
			}
			end = next.End
			i = j
		}
		merged = append(merged, layer.Entry{Name: e.Name, Start: e.Start, End: end})
	}

	return merged
}

// applyTokenEdits applies a slice of LSP SemanticTokensEdit operations to the
// previous flat token data array.  Edits are applied in reverse index order so
// that later splice positions are not invalidated by earlier ones.
func applyTokenEdits(prev []uint32, edits []protocol.SemanticTokensEdit) []uint32 {
	out := make([]uint32, len(prev))
	copy(out, prev)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		start := int(e.Start)
		end := start + int(e.DeleteCount)
		if start > len(out) {
			start = len(out)
		}
		if end > len(out) {
			end = len(out)
		}
		var next []uint32
		next = append(next, out[:start]...)
		next = append(next, e.Data...)
		next = append(next, out[end:]...)
		out = next
	}
	return out
}

// ApplySemanticTokens fetches semantic tokens for the named document from the
// LSP server and writes them to layer via the acme-styles compositor.
//
// ts is the per-file token cache.  When ts holds a non-empty resultID the
// function uses the LSP delta protocol (textDocument/semanticTokens/full/delta)
// for efficiency; a full request is used on first call or after failure.
func ApplySemanticTokens(ctx context.Context, c *Client, w *acmeutil.Win, name string, ts *tokenState, sl *layer.StyleLayer) error {
	if sl == nil {
		return nil
	}
	legend, ok := semanticLegend(c.initializeResult.Capabilities)
	if !ok {
		return nil
	}

	body, err := w.ReadAll("body")
	if err != nil {
		return fmt.Errorf("reading body: %v", err)
	}

	var prevResultID string
	var prevData []uint32
	if ts != nil {
		ts.mu.Lock()
		prevResultID = ts.resultID
		prevData = ts.data
		ts.mu.Unlock()
	}

	var data []uint32
	var newResultID string

	if prevResultID != "" {
		raw, err := c.SemanticTokensFullDelta(ctx, &protocol.SemanticTokensDeltaParams{
			TextDocument:     protocol.TextDocumentIdentifier{URI: text.ToURI(name)},
			PreviousResultID: prevResultID,
		})
		if err == nil && raw != nil {
			b, _ := json.Marshal(raw)
			var peek struct {
				Edits *json.RawMessage `json:"edits"`
			}
			json.Unmarshal(b, &peek) //nolint:errcheck
			if peek.Edits != nil {
				var delta protocol.SemanticTokensDelta
				if json.Unmarshal(b, &delta) == nil {
					data = applyTokenEdits(prevData, delta.Edits)
					newResultID = delta.ResultID
				}
			} else {
				var full protocol.SemanticTokens
				if json.Unmarshal(b, &full) == nil && len(full.Data) > 0 {
					data = full.Data
					newResultID = full.ResultID
				}
			}
		}
	}

	if data == nil {
		result, err := c.SemanticTokensFull(ctx, &protocol.SemanticTokensParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: text.ToURI(name)},
		})
		if err != nil {
			if Verbose {
				log.Printf("SemanticTokensFull %v: %v", name, err)
			}
			return nil // non-fatal: server may not be ready yet
		}
		if result == nil || len(result.Data) == 0 {
			sl.Clear()
			return nil
		}
		data = result.Data
		newResultID = result.ResultID
	}

	if ts != nil && newResultID != "" {
		ts.mu.Lock()
		ts.resultID = newResultID
		ts.data = data
		ts.mu.Unlock()
	}

	return sl.Apply(buildStyleEntries(data, legend, body))
}
