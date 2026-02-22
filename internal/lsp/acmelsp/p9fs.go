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
// It bridges between the 9P write/read handlers and an in-process jsonrpc2
// server connection.
//
// The client writes raw JSON-RPC objects via Write; the session frames them
// with Content-Length headers and forwards them to a jsonrpc2 server running
// in-process on the other end of a net.Pipe.  Responses from the server are
// stripped of their headers and accumulated in rbuf; Read calls block until
// data is available at the requested offset.
type p9Session struct {
	// wch carries raw JSON-RPC objects from the Write handler to the writer
	// goroutine.  Closed by Clunk to initiate orderly shutdown.
	wch chan []byte

	// rbuf accumulates all response bytes ever received so that offset-based
	// 9P reads can serve them correctly.  It is only appended to, never
	// truncated.
	mu        sync.Mutex
	cond      *sync.Cond
	rbuf      []byte
	err       error // terminal error; nil while running
	writeOnly bool  // when true, responses are discarded
}

// newP9Session creates an in-process jsonrpc2 server backed by handler, then
// starts goroutines to bridge between the session's channel/buffer and the
// in-process pipe.
func newP9Session(ctx context.Context, handler jsonrpc2.Handler, writeOnly bool) *p9Session {
	s := &p9Session{
		wch:       make(chan []byte, 16),
		writeOnly: writeOnly,
	}
	s.cond = sync.NewCond(&s.mu)

	// pipeA is our end; pipeB is the jsonrpc2 server end.
	pipeA, pipeB := net.Pipe()

	// Start the jsonrpc2 server on pipeB.
	streamB := jsonrpc2.NewBufferedStream(pipeB, jsonrpc2.VSCodeObjectCodec{})
	jsonrpc2.NewConn(ctx, streamB, handler)

	// Writer: drains wch, adds Content-Length framing, writes to pipeA.
	// Closing wch (in Clunk) causes this goroutine to exit and close pipeA,
	// which in turn causes the reader goroutine and the jsonrpc2 server to
	// terminate.
	go func() {
		defer pipeA.Close()
		for data := range s.wch {
			hdr := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
			if _, err := io.WriteString(pipeA, hdr); err != nil {
				return
			}
			if _, err := pipeA.Write(data); err != nil {
				return
			}
		}
	}()

	// Reader: reads Content-Length-framed responses from pipeA, strips headers,
	// appends raw JSON to rbuf, and wakes any blocked Read calls.
	go func() {
		r := bufio.NewReader(pipeA)
		for {
			// Parse header lines until blank line.
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
				var n int
				if _, err := fmt.Sscanf(line, "Content-Length: %d", &n); err == nil && n > 0 {
					length = n
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
			if !writeOnly {
				s.mu.Lock()
				s.rbuf = append(s.rbuf, body...)
				s.rbuf = append(s.rbuf, '\n')
				s.cond.Signal()
				s.mu.Unlock()
			}
		}
	}()

	return s
}

func (s *p9Session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// read blocks until response bytes are available at offset, then copies into
// data.  Returns 0, nil when writeOnly (immediate EOF in 9P).
func (s *p9Session) read(data []byte, offset int64) (int, error) {
	if s.writeOnly {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for int64(len(s.rbuf)) <= offset && s.err == nil {
		s.cond.Wait()
	}
	if int64(len(s.rbuf)) <= offset {
		return 0, io.EOF
	}
	return copy(data, s.rbuf[offset:]), nil
}

// ---- directServerProxy ----

// directServerProxy implements proxy.Server by forwarding all LSP methods
// directly to a specific language server's protocol.Server connection, and
// returning errors for acme-lsp-specific extension methods.
type directServerProxy struct {
	protocol.Server // forwards all LSP method calls
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
	wbuf    []byte     // partial JSON accumulation between Write calls
	rbuf    []byte     // pre-built directory listing (root only)
}

// p9FS implements the 9P virtual filesystem.
//
// The root directory lists one file per configured language server plus the
// special "ls" file that proxies to the acme-lsp proxy server.  Opening a
// file for writing (or RDWR) creates a p9Session; the client can then write
// raw JSON-RPC objects and read back raw JSON-RPC responses.
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

// serverQid returns a stable Qid for the given server file.
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
// "ls" maps to the proxy server; anything else maps directly to the
// named language server's protocol.Server connection.
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
		// Clone: copy position to newfid.
		ac := *a
		newfid.SetAux(&ac)
		return nil, nil
	}

	curFt := a.ft
	curName := a.srvName
	var qids []plan9.Qid

	for i, name := range names {
		if curFt != p9ftRoot {
			// Server files have no children; partial walk stops here.
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
		qid := fs.serverQid(name)
		qids = append(qids, qid)
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
			// Read-only: no session needed; reads will return EOF immediately.
			break
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
			return 0, nil // read-only: EOF
		}
		return a.sess.read(data, offset)
	}
	return 0, fmt.Errorf("read: unknown file type")
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

func (fs *p9FS) write(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	a := fid.Aux().(*p9FidAux)
	if a.sess == nil {
		return 0, fmt.Errorf("not writable")
	}
	a.wbuf = append(a.wbuf, data...)
	objs, rest := extractJSONObjects(a.wbuf)
	a.wbuf = rest
	for _, obj := range objs {
		select {
		case a.sess.wch <- []byte(obj):
		case <-ctx.Done():
			return len(data), ctx.Err()
		}
	}
	return len(data), nil
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
	// Closing wch causes the writer goroutine to drain and exit.
	// It then closes pipeA, which propagates EOF to the reader goroutine
	// and terminates the jsonrpc2 server on pipeB.
	close(a.sess.wch)
}

// ServeP9FS runs the 9P filesystem on conn until the connection is closed or
// ctx is cancelled.  It is intended to be called in a goroutine.
func ServeP9FS(ctx context.Context, conn io.ReadWriteCloser, ss *ServerSet, fm *FileManager) {
	defer conn.Close()
	fs := &p9FS{ctx: ctx, ss: ss, fm: fm}
	fs.Build().Serve(conn, conn)
}
