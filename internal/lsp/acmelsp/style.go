package acmelsp

import (
	"bufio"
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
	// lineStart[i] and lineStartByte[i] give us anchor points.
	// Find the line whose lineStart is <= gapStart.
	byteOff := 0
	for l := len(ro.lineStart) - 1; l >= 0; l-- {
		if ro.lineStart[l] <= gapStart {
			byteOff = ro.lineStartByte[l]
			// Advance byte-wise for any runes between lineStart[l] and gapStart.
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
	// Walk runes from gapStart to gapEnd checking each is '\n'.
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

// MaxStyleTriples is the maximum number of (index, start, length) triples
// that fit in a single style ctl write.  ctlstyleparse in acme declares a
// fixed buffer of 768 ulongs, which holds exactly 256 triples.
const MaxStyleTriples = 256

// styleToken holds a resolved token ready for emission.
type styleToken struct {
	styleIdx int
	start    int // rune offset
	length   int // runes
}

// buildStyleCmds converts the encoded LSP semantic token data into one or
// more acme style ctl commands, each containing at most MaxStyleTriples
// triples so that ctlstyleparse never overflows its fixed buffer.
//
// The acme style command format used here is:
//
//	style <idx> <start> <len> [<idx> <start> <len> ...]
//
// Every entry carries an absolute start position so non-contiguous ranges
// require no gap-filling.  Returns nil if there are no mapped tokens.
func buildStyleCmds(data []uint32, legend *protocol.SemanticTokensLegend, sm StyleMap, body []byte) []string {
	ro := buildRuneOffsets(body)

	// First pass: decode the relative encoding into absolute tokens.
	tokens := make([]styleToken, 0, len(data)/5)
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
		tokens = append(tokens, styleToken{
			styleIdx: styleIdx,
			start:    ro.toRuneOffset(int(line), int(col)),
			// LSP encodes length in UTF-16 code units; we approximate with rune
			// count, which is exact for BMP content (the common case).
			length: int(length),
		})
	}

	// Second pass: merge adjacent runs of the same style separated only by
	// blank lines.  gopls returns one token per line for multi-line constructs
	// (backtick strings, block comments) and emits nothing for blank lines
	// within them, even when multilineTokenSupport is advertised.
	merged := make([]styleToken, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		end := t.start + t.length
		for j := i + 1; j < len(tokens); j++ {
			next := tokens[j]
			if next.styleIdx != t.styleIdx {
				break
			}
			if !blankLinesOnly(body, ro, end, next.start) {
				break
			}
			end = next.start + next.length
			i = j
		}
		merged = append(merged, styleToken{t.styleIdx, t.start, end - t.start})
	}

	if len(merged) == 0 {
		return nil
	}

	// Third pass: split merged tokens into batches of at most MaxStyleTriples
	// so each ctl write fits within ctlstyleparse's fixed buffer.
	var cmds []string
	for start := 0; start < len(merged); start += MaxStyleTriples {
		end := start + MaxStyleTriples
		if end > len(merged) {
			end = len(merged)
		}
		batch := merged[start:end]

		var b strings.Builder
		fmt.Fprintf(&b, "style %d %d %d", batch[0].styleIdx, batch[0].start, batch[0].length)
		for _, tok := range batch[1:] {
			fmt.Fprintf(&b, " %d %d %d", tok.styleIdx, tok.start, tok.length)
		}
		cmds = append(cmds, b.String())
	}
	return cmds
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
// LSP server and writes the corresponding style ctl commands to the acme window.
//
// ts is the per-file token cache.  When ts holds a non-empty resultID from a
// previous response the function requests a delta update
// (textDocument/semanticTokens/full/delta) and applies the edit list to the
// cached data; this is more efficient than a full request for large files.
// A full request is used when ts is nil, when no resultID is cached, or when
// the delta request fails.
func ApplySemanticTokens(ctx context.Context, c *Client, w *acmeutil.Win, name string, sm StyleMap, ts *tokenState) error {
	if len(sm) == 0 {
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

	// Retrieve cached state under the token-state lock.
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
		// Attempt a delta request using the previous resultId.
		raw, err := c.SemanticTokensFullDelta(ctx, &protocol.SemanticTokensDeltaParams{
			TextDocument:     protocol.TextDocumentIdentifier{URI: text.ToURI(name)},
			PreviousResultID: prevResultID,
		})
		if err == nil && raw != nil {
			b, _ := json.Marshal(raw)
			// Distinguish a SemanticTokensDelta (has "edits") from a SemanticTokens
			// (has "data") by checking which key is present.
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
		// Fall back to a full token request.
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
			return w.Ctl("style 0")
		}
		data = result.Data
		newResultID = result.ResultID
	}

	// Persist the updated result for the next delta request.
	if ts != nil && newResultID != "" {
		ts.mu.Lock()
		ts.resultID = newResultID
		ts.data = data
		ts.mu.Unlock()
	}

	cmds := buildStyleCmds(data, legend, sm, body)

	// Clear existing highlights first, then apply the new ones in batches.
	// Each batch is a separate ctl write so ctlstyleparse's fixed 256-triple
	// buffer is never exceeded.  The occlusion check in ctlstyleparse skips
	// winframesync for batches whose segments are entirely off-screen.
	if err := w.Ctl("style 0"); err != nil {
		return err
	}
	for _, cmd := range cmds {
		if err := w.Ctl("%s", cmd); err != nil {
			return err
		}
	}
	return nil
}
