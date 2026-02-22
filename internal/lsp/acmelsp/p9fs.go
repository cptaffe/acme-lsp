package acmelsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"9fans.net/acme-lsp/internal/lsp/proxy"
	"9fans.net/go/plan9"
	"9fans.net/go/plan9/srv9p"
	"9fans.net/internal/go-lsp/lsp/protocol"
	"github.com/sourcegraph/jsonrpc2"
)

// proxyFileName is the name of the 9P file that proxies to the acme-lsp
// proxy server (i.e. the same server that L talks to).
const proxyFileName = "ls"

// ---- p9Session ----

// p9Session holds per-fid state for a server file opened for writing or RDWR.
//
// External protocol (JSONL-ish):
//
//   - Writes accumulate in a buffer; complete JSON objects are extracted with
//     json.Decoder (newlines are not required — 9p rdwr strips the trailing
//     \n via Brdstr before calling fswrite).  The session closes on Clunk.
//
//   - Reads use a simple rule:
//       • If a request is in-flight (awaiting), block until the response
//         arrives or the session ends.
//       • If unread response data is available (lastGen < gen), return it.
//       • Otherwise return 0 (9P EOF / no data).  This lets 9p rdwr, which
//         reads before writing, proceed to send its first request without
//         needing a pre-seeded buffer.
//
// Internally the session uses Content-Length-framed JSON-RPC over a net.Pipe
// connected to an in-process jsonrpc2 server.
type p9Session struct {
	wch       chan []byte // JSON-RPC bodies delivered to the writer goroutine
	closeOnce sync.Once

	mu       sync.Mutex
	cond     *sync.Cond
	curBuf   []byte // latest response body as a JSONL line (body + "\n")
	gen      uint64 // incremented whenever curBuf is replaced
	awaiting bool   // true while a request is in-flight
	closed   bool   // true after session is closed
	err      error  // terminal pipe error

	writeOnly bool // if true, reads return 0 immediately
}

// newP9Session starts an in-process jsonrpc2 server backed by handler and
// launches writer and reader goroutines.
func newP9Session(ctx context.Context, handler jsonrpc2.Handler, writeOnly bool) *p9Session {
	s := &p9Session{
		wch:       make(chan []byte, 16),
		writeOnly: writeOnly,
	}
	s.cond = sync.NewCond(&s.mu)

	pipeA, pipeB := net.Pipe()

	// jsonrpc2 server on pipeB processes requests and sends responses.
	streamB := jsonrpc2.NewBufferedStream(pipeB, jsonrpc2.VSCodeObjectCodec{})
	jsonrpc2.NewConn(ctx, streamB, handler)

	// Writer goroutine: drains wch, adds Content-Length framing, writes to pipeA.
	// Closing wch (via s.close) causes this goroutine to exit, closing pipeA,
	// which shuts down the reader goroutine and the jsonrpc2 server on pipeB.
	go func() {
		defer pipeA.Close()
		for body := range s.wch {
			hdr := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
			if _, err := io.WriteString(pipeA, hdr); err != nil {
				return
			}
			if _, err := pipeA.Write(body); err != nil {
				return
			}
		}
	}()

	// Reader goroutine: reads Content-Length-framed responses, strips headers,
	// and replaces curBuf with the raw JSON body as a JSONL line.
	go func() {
		r := bufio.NewReader(pipeA)
		for {
			var length int
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					s.setErr(err)
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if strings.HasPrefix(line, "Content-Length:") {
					var n int
					fmt.Sscanf(line, "Content-Length: %d", &n)
					if n > 0 {
						length = n
					}
				}
			}
			if length == 0 {
				continue
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(r, body); err != nil {
				s.setErr(err)
				return
			}
			s.mu.Lock()
			s.curBuf = append(body, '\n')
			s.gen++
			s.awaiting = false
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	}()

	return s
}

// close signals EOF.  Safe to call more than once.
func (s *p9Session) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cond.Broadcast()
		s.mu.Unlock()
		close(s.wch)
	})
}

func (s *p9Session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// read delivers response data to the 9P client.  lastGen is per-fid and
// tracks which response generation has already been delivered.
//
// Behaviour:
//   - Block while a request is in-flight (awaiting).
//   - Return curBuf[offset:] when there is new content (lastGen < gen).
//     Advance lastGen only after the last byte of curBuf is delivered so
//     that callers reading in chunks work correctly.
//   - Return 0 in all other cases (no pending request, no new content).
//     Returning 0 is what lets 9p rdwr unblock and proceed to write its
//     first request.
func (s *p9Session) read(data []byte, offset int64, lastGen *uint64) (int, error) {
	if s.writeOnly {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Block while a request is in-flight.
	for s.awaiting && s.err == nil && !s.closed {
		s.cond.Wait()
	}

	// Return new content if available.
	if *lastGen < s.gen {
		n := copy(data, s.curBuf[offset:])
		if offset+int64(n) >= int64(len(s.curBuf)) {
			*lastGen = s.gen
		}
		return n, nil
	}

	// No pending request, no new content → 0 (lets 9p rdwr write next).
	return 0, nil
}

// ---- directServerProxy ----

// directServerProxy implements proxy.Server by forwarding all standard LSP
// methods directly to a specific language server's protocol.Server connection.
// acme-lsp extension methods return errors.
type directServerProxy struct {
	protocol.Server
}

func (p *directServerProxy) Version(context.Context) (int, error) {
	return 0, fmt.Errorf("not supported on direct server connection")
}

func (p *directServerProxy) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return nil, fmt.Errorf("not supported on direct server connection")
}

func (p *directServerProxy) InitializeResult(_ context.Context, _ *protocol.TextDocumentIdentifier) (*protocol.InitializeResult, error) {
	return nil, fmt.Errorf("not supported on direct server connection")
}

func (p *directServerProxy) ExecuteCommandOnDocument(ctx context.Context, params *proxy.ExecuteCommandOnDocumentParams) (interface{}, error) {
	return p.Server.ExecuteCommand(ctx, &params.ExecuteCommandParams)
}

func (p *directServerProxy) ExecuteCommandOnServer(ctx context.Context, params *proxy.ExecuteCommandOnServerParams) (interface{}, error) {
	return p.Server.ExecuteCommand(ctx, &params.ExecuteCommandParams)
}

// ---- 9P filesystem ----

const (
	p9ftRoot   = 0
	p9ftServer = 1
)

// p9FidAux is the per-fid state stored in a srv9p.Fid.
type p9FidAux struct {
	ft      int        // p9ftRoot or p9ftServer
	srvName string     // server name (only meaningful for p9ftServer)
	sess    *p9Session // nil for root or read-only fids
	wbuf    []byte     // incomplete data accumulation between Write calls
	rbuf    []byte     // pre-built directory listing (root only)
	lastGen uint64     // last p9Session.gen delivered to this fid
}

// p9FS implements the 9P virtual filesystem.
//
// Root directory lists one file per configured language server plus the
// special "ls" file that proxies to the acme-lsp proxy server.
type p9FS struct {
	ctx context.Context
	ss  *ServerSet
	fm  *FileManager
}

func (fs *p9FS) serverNames() []string {
	seen := map[string]bool{proxyFileName: true}
	names := []string{proxyFileName}
	for _, info := range fs.ss.Data {
		if !seen[info.ID] {
			seen[info.ID] = true
			names = append(names, info.ID)
		}
	}
	return names
}

func (fs *p9FS) serverQid(name string) plan9.Qid {
	for i, n := range fs.serverNames() {
		if n == name {
			return plan9.Qid{Type: plan9.QTFILE, Path: uint64(i + 1)}
		}
	}
	return plan9.Qid{Type: plan9.QTFILE, Path: 1}
}

func (fs *p9FS) buildRootListing() []byte {
	now := uint32(time.Now().Unix())
	var buf []byte
	for _, name := range fs.serverNames() {
		d := plan9.Dir{
			Qid:   fs.serverQid(name),
			Mode:  0666,
			Atime: now, Mtime: now,
			Name: name,
			Uid:  "none", Gid: "none", Muid: "none",
		}
		if b, err := d.Bytes(); err == nil {
			buf = append(buf, b...)
		}
	}
	return buf
}

// handlerFor returns the jsonrpc2.Handler for the named server file.
func (fs *p9FS) handlerFor(srvName string) (jsonrpc2.Handler, error) {
	if srvName == proxyFileName {
		return proxy.NewServerHandler(&proxyServer{ss: fs.ss, fm: fs.fm}), nil
	}
	srv, found, err := fs.ss.StartForID(srvName)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("unknown server %q", srvName)
	}
	return proxy.NewServerHandler(&directServerProxy{Server: srv.Client.Server}), nil
}

// Build constructs and returns the srv9p.Server.
func (fs *p9FS) Build() *srv9p.Server {
	return &srv9p.Server{
		Attach: fs.attach,
		Walk:   fs.walk,
		Open:   fs.open,
		Read:   fs.read,
		Write:  fs.write,
		Stat:   fs.stat,
		Clunk:  fs.clunk,
	}
}

func (fs *p9FS) attach(ctx context.Context, fid, _ *srv9p.Fid, _, _ string) (plan9.Qid, error) {
	qid := plan9.Qid{Type: plan9.QTDIR, Path: 0}
	fid.SetAux(&p9FidAux{ft: p9ftRoot})
	fid.SetQid(qid)
	return qid, nil
}

func (fs *p9FS) walk(ctx context.Context, fid, newfid *srv9p.Fid, names []string) ([]plan9.Qid, error) {
	a := fid.Aux().(*p9FidAux)

	if len(names) == 0 {
		ac := *a
		newfid.SetAux(&ac)
		return nil, nil
	}

	curFt := a.ft
	curName := a.srvName
	var qids []plan9.Qid

	for i, name := range names {
		if curFt != p9ftRoot {
			break
		}
		var found bool
		for _, n := range fs.serverNames() {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			if i == 0 {
				return nil, fmt.Errorf("no such file")
			}
			break
		}
		qids = append(qids, fs.serverQid(name))
		curFt = p9ftServer
		curName = name
	}

	newfid.SetAux(&p9FidAux{ft: curFt, srvName: curName})
	if curFt == p9ftServer {
		newfid.SetQid(fs.serverQid(curName))
	} else {
		newfid.SetQid(plan9.Qid{Type: plan9.QTDIR, Path: 0})
	}
	return qids, nil
}

func (fs *p9FS) open(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
	a := fid.Aux().(*p9FidAux)
	m := mode & 3

	switch a.ft {
	case p9ftRoot:
		if m != plan9.OREAD {
			return fmt.Errorf("permission denied")
		}
		a.rbuf = fs.buildRootListing()

	case p9ftServer:
		if m == plan9.OREAD {
			break // read-only: no session; reads return 0
		}
		handler, err := fs.handlerFor(a.srvName)
		if err != nil {
			return err
		}
		a.sess = newP9Session(fs.ctx, handler, m == plan9.OWRITE)
	}
	return nil
}

func (fs *p9FS) read(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	a := fid.Aux().(*p9FidAux)
	switch a.ft {
	case p9ftRoot:
		return fid.ReadBytes(data, offset, a.rbuf)
	case p9ftServer:
		if a.sess == nil {
			return 0, nil
		}
		return a.sess.read(data, offset, &a.lastGen)
	}
	return 0, fmt.Errorf("read: unknown file type")
}

// write appends data to an accumulation buffer and extracts complete JSON
// objects via json.Decoder.  Newlines are not required: 9p rdwr strips the
// trailing \n via Brdstr before calling fswrite, so the objects arrive as
// plain JSON text.  The session closes when the fid is clunked.
func (fs *p9FS) write(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	a := fid.Aux().(*p9FidAux)
	if a.sess == nil {
		return 0, fmt.Errorf("not writable")
	}

	a.wbuf = append(a.wbuf, data...)

	objs, rest := extractJSONObjects(a.wbuf)
	a.wbuf = rest

	for _, obj := range objs {
		a.sess.mu.Lock()
		if a.sess.closed {
			a.sess.mu.Unlock()
			break
		}
		if !a.sess.writeOnly {
			a.sess.awaiting = true
		}
		a.sess.mu.Unlock()

		select {
		case a.sess.wch <- []byte(obj):
		case <-ctx.Done():
			return len(data), ctx.Err()
		}
	}

	return len(data), nil
}

// extractJSONObjects scans data for complete top-level JSON values,
// returning them and any unparsed trailing bytes.
func extractJSONObjects(data []byte) ([]json.RawMessage, []byte) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var objs []json.RawMessage
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		objs = append(objs, raw)
	}
	return objs, data[dec.InputOffset():]
}

func (fs *p9FS) stat(ctx context.Context, fid *srv9p.Fid) (*plan9.Dir, error) {
	a := fid.Aux().(*p9FidAux)
	now := uint32(time.Now().Unix())
	switch a.ft {
	case p9ftRoot:
		return &plan9.Dir{
			Qid:   plan9.Qid{Type: plan9.QTDIR, Path: 0},
			Mode:  plan9.DMDIR | 0555,
			Atime: now, Mtime: now,
			Name:  "/",
			Uid:   "none", Gid: "none", Muid: "none",
		}, nil
	case p9ftServer:
		return &plan9.Dir{
			Qid:   fs.serverQid(a.srvName),
			Mode:  0666,
			Atime: now, Mtime: now,
			Name:  a.srvName,
			Uid:   "none", Gid: "none", Muid: "none",
		}, nil
	}
	return nil, fmt.Errorf("stat: unknown file type")
}

func (fs *p9FS) clunk(fid *srv9p.Fid) {
	a, ok := fid.Aux().(*p9FidAux)
	if !ok || a == nil || a.sess == nil {
		return
	}
	a.sess.close()
}

// ServeP9FS accepts 9P client connections from ln and serves each in its own
// goroutine.  Used on non-Plan 9 systems.  Blocks until ln is closed.
func ServeP9FS(ctx context.Context, ln net.Listener, ss *ServerSet, fm *FileManager) {
	defer ln.Close()
	srv := (&p9FS{ctx: ctx, ss: ss, fm: fm}).Build()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go srv.Serve(conn, conn)
	}
}

// ServeP9FSConn runs the 9P filesystem on a single bidirectional connection.
// Used on Plan 9, where srv9p.Post returns a kernel-muxed pipe.
func ServeP9FSConn(ctx context.Context, conn io.ReadWriteCloser, ss *ServerSet, fm *FileManager) {
	defer conn.Close()
	(&p9FS{ctx: ctx, ss: ss, fm: fm}).Build().Serve(conn, conn)
}
