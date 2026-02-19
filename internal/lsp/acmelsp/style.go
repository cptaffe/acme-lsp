package acmelsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"9fans.net/acme-lsp/internal/acmeutil"
	"9fans.net/acme-lsp/internal/lsp/text"
	"9fans.net/internal/go-lsp/lsp/protocol"
	"9fans.net/go/plan9"
	"9fans.net/go/plan9/client"
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

// StyleEntry is a resolved (style-index, start-rune-offset, rune-length)
// triple ready for writing to an acme-styles layer.
type StyleEntry struct {
	StyleIdx int
	Start    int
	Length   int
}

// StyleLayer represents a single named layer owned by acme-lsp on the
// acme-styles compositor for one acme window.  It holds no persistent
// connection — each operation mounts acme-styles fresh so the layer
// survives acme-styles restarts transparently.
type StyleLayer struct {
	winID   int
	layerID int
}

func (sl *StyleLayer) mount() (*client.Fsys, error) {
	return client.MountService("acme-styles")
}

// allocLayer opens <winid>/layers/new and reads back the assigned ID,
// updating sl.layerID in place.
func (sl *StyleLayer) allocLayer(fs *client.Fsys) error {
	fid, err := fs.Open(fmt.Sprintf("%d/layers/new", sl.winID), plan9.OREAD)
	if err != nil {
		return err
	}
	var buf [32]byte
	n, err := fid.Read(buf[:])
	fid.Close()
	if err != nil && n == 0 {
		return fmt.Errorf("reading layer id: %v", err)
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return fmt.Errorf("parsing layer id %q: %v", string(buf[:n]), err)
	}
	sl.layerID = id
	return nil
}

// Clear zeroes the layer.  Best-effort: if acme-styles is not running the
// layer is already gone, so errors are silently ignored.
func (sl *StyleLayer) Clear() error {
	fs, err := sl.mount()
	if err != nil {
		return nil
	}
	defer fs.Close()
	fid, err := fs.Open(fmt.Sprintf("%d/layers/%d/clear", sl.winID, sl.layerID), plan9.OWRITE)
	if err != nil {
		return nil
	}
	fid.Close()
	return nil
}

// Apply clears the layer and writes new entries.  If the layer no longer
// exists (acme-styles restarted) it is re-allocated before writing.
func (sl *StyleLayer) Apply(entries []StyleEntry) error {
	fs, err := sl.mount()
	if err != nil {
		return err
	}
	defer fs.Close()

	clearPath := fmt.Sprintf("%d/layers/%d/clear", sl.winID, sl.layerID)
	clearFid, err := fs.Open(clearPath, plan9.OWRITE)
	if err != nil {
		// Layer is gone — acme-styles restarted.  Re-allocate and retry.
		if err2 := sl.allocLayer(fs); err2 != nil {
			return fmt.Errorf("clear: %v; re-alloc: %v", err, err2)
		}
		clearFid, err = fs.Open(fmt.Sprintf("%d/layers/%d/clear", sl.winID, sl.layerID), plan9.OWRITE)
		if err != nil {
			return err
		}
	}
	clearFid.Close()

	if len(entries) == 0 {
		return nil
	}
	fid, err := fs.Open(fmt.Sprintf("%d/layers/%d/style", sl.winID, sl.layerID), plan9.OWRITE)
	if err != nil {
		return err
	}
	defer fid.Close()
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%d %d %d\n", e.StyleIdx, e.Start, e.Length)
	}
	_, err = fid.Write([]byte(sb.String()))
	return err
}

// buildStyleEntries converts encoded LSP semantic token data into a flat slice
// of StyleEntry values ready for writing to an acme-styles layer.
// Returns nil if there are no mapped tokens.
func buildStyleEntries(data []uint32, legend *protocol.SemanticTokensLegend, sm StyleMap, body []byte) []StyleEntry {
	ro := buildRuneOffsets(body)

	// First pass: decode the relative encoding into absolute entries.
	entries := make([]StyleEntry, 0, len(data)/5)
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
		styleIdx, ok := sm[legend.TokenTypes[tokenType]]
		if !ok || styleIdx == 0 {
			continue
		}
		entries = append(entries, StyleEntry{
			StyleIdx: styleIdx,
			Start:    ro.toRuneOffset(int(line), int(col)),
			Length:   int(length),
		})
	}

	// Second pass: merge adjacent runs of the same style separated only by
	// blank lines.  gopls returns one token per line for multi-line constructs
	// (backtick strings, block comments) and emits nothing for blank lines
	// within them, even when multilineTokenSupport is advertised.
	merged := make([]StyleEntry, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		end := e.Start + e.Length
		for j := i + 1; j < len(entries); j++ {
			next := entries[j]
			if next.StyleIdx != e.StyleIdx {
				break
			}
			if !blankLinesOnly(body, ro, end, next.Start) {
				break
			}
			end = next.Start + next.Length
			i = j
		}
		merged = append(merged, StyleEntry{e.StyleIdx, e.Start, end - e.Start})
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
func ApplySemanticTokens(ctx context.Context, c *Client, w *acmeutil.Win, name string, sm StyleMap, ts *tokenState, layer *StyleLayer) error {
	if len(sm) == 0 || layer == nil {
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
			return layer.Clear()
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

	return layer.Apply(buildStyleEntries(data, legend, sm, body))
}
