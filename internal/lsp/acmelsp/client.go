package acmelsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync"

	"9fans.net/internal/go-lsp/lsp/protocol"
	"github.com/sourcegraph/jsonrpc2"

	"9fans.net/acme-lsp/internal/lsp"
	"9fans.net/acme-lsp/internal/lsp/acmelsp/config"
	"9fans.net/acme-lsp/internal/lsp/proxy"
	"9fans.net/acme-lsp/internal/lsp/text"
)

var Verbose = false

type DiagnosticsWriter interface {
	WriteDiagnostics(params *protocol.PublishDiagnosticsParams)
}

// clientHandler handles JSON-RPC requests and notifications.
type clientHandler struct {
	cfg             *ClientConfig
	hideDiag        bool
	diagWriter      DiagnosticsWriter
	diag            map[protocol.DocumentURI][]protocol.Diagnostic
	mu              sync.Mutex
	tokensRefresher SemanticTokensRefresher
	proxy.NotImplementedClient
}

func (h *clientHandler) SemanticTokensRefresh(ctx context.Context) error {
	if h.tokensRefresher != nil {
		h.tokensRefresher.RefreshSemanticTokens()
	}
	return nil
}

func (h *clientHandler) ShowMessage(ctx context.Context, params *protocol.ShowMessageParams) error {
	log.Printf("LSP %v: %v\n", params.Type, params.Message)
	return nil
}

func (h *clientHandler) LogMessage(ctx context.Context, params *protocol.LogMessageParams) error {
	if h.cfg.Logger != nil {
		h.cfg.Logger.Printf("%v: %v\n", params.Type, params.Message)
		return nil
	}
	if params.Type == protocol.Error || params.Type == protocol.Warning || Verbose {
		log.Printf("log: LSP %v: %v\n", params.Type, params.Message)
	}
	return nil
}

func (h *clientHandler) Event(context.Context, *interface{}) error {
	return nil
}

func (h *clientHandler) PublishDiagnostics(ctx context.Context, params *protocol.PublishDiagnosticsParams) error {
	if h.hideDiag {
		return nil
	}

	h.diagWriter.WriteDiagnostics(params)
	return nil
}

func (h *clientHandler) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return nil, nil
}

func (h *clientHandler) Configuration(context.Context, *protocol.ParamConfiguration) ([]interface{}, error) {
	return nil, nil
}

func (h *clientHandler) RegisterCapability(context.Context, *protocol.RegistrationParams) error {
	return nil
}

func (h *clientHandler) UnregisterCapability(context.Context, *protocol.UnregistrationParams) error {
	return nil
}

func (h *clientHandler) ShowMessageRequest(context.Context, *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	return nil, nil
}

func (h *clientHandler) ApplyEdit(ctx context.Context, params *protocol.ApplyWorkspaceEditParams) (*protocol.ApplyWorkspaceEditResult, error) {
	err := editWorkspace(&params.Edit)
	if err != nil {
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: err.Error()}, nil
	}
	return &protocol.ApplyWorkspaceEditResult{Applied: true}, nil
}

// SemanticTokensRefresher is notified when the server sends
// workspace/semanticTokens/refresh.
type SemanticTokensRefresher interface {
	RefreshSemanticTokens()
}

// ServerResyncer is notified after a language server is restarted (following a
// crash or exit) and re-initialized.  A freshly started process has an empty
// document store, so the implementation must re-send textDocument/didOpen for
// the open files it tracks; otherwise every subsequent request fails with
// "document not found" until each file is manually reopened.
type ServerResyncer interface {
	ResyncFiles()
}

// ClientConfig contains LSP client configuration values.
type ClientConfig struct {
	*config.Server
	*config.FilenameHandler
	RootDirectory      string                     // used to compute RootURI in initialization
	HideDiag           bool                       // don't write diagnostics to DiagWriter
	RPCTrace           bool                       // print LSP rpc trace to stderr
	DiagWriter         DiagnosticsWriter          // notification handler writes diagnostics here
	Workspaces         []protocol.WorkspaceFolder // initial workspace folders
	Logger             *log.Logger
	TokensRefresher    SemanticTokensRefresher    // called on workspace/semanticTokens/refresh; may be nil
	Resyncer           ServerResyncer             // called after a server restart to replay didOpen; may be nil
}

// Client represents a LSP client connection.
type Client struct {
	protocol.Server
	initializeResult *protocol.InitializeResult
	cfg              *ClientConfig
	rpc              *jsonrpc2.Conn
}

func NewClient(conn net.Conn, cfg *ClientConfig) (*Client, error) {
	c := &Client{cfg: cfg}
	if err := c.init(conn, cfg); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error {
	return c.rpc.Close()
}

// initParams wraps ParamInitialize to serialize rootUri as JSON null when
// there is no workspace root.  The go-lsp RootURI field is a plain string with
// no omitempty, so it otherwise marshals as "rootUri":"", which the LSP spec
// disallows (rootUri must be null when no folder is open) and strict servers
// such as taplo reject outright.
type initParams struct {
	params  *protocol.ParamInitialize
	hasRoot bool
}

func (p *initParams) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(p.params)
	if err != nil {
		return nil, err
	}
	if p.hasRoot {
		return b, nil
	}
	// Rewrite the empty rootUri string to null.  The value is always "" here
	// (init only builds initParams with hasRoot=false when rootURI is unset).
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["rootUri"] = json.RawMessage("null")
	return json.Marshal(m)
}

func (c *Client) init(conn net.Conn, cfg *ClientConfig) error {
	ctx := context.Background()
	stream := jsonrpc2.NewBufferedStream(conn, jsonrpc2.VSCodeObjectCodec{})
	handler := proxy.NewClientHandler(&clientHandler{
		cfg:             cfg,
		hideDiag:        cfg.HideDiag,
		diagWriter:      cfg.DiagWriter,
		diag:            make(map[protocol.DocumentURI][]protocol.Diagnostic),
		tokensRefresher: cfg.TokensRefresher,
	})
	var opts []jsonrpc2.ConnOpt
	if cfg.RPCTrace {
		opts = append(opts, lsp.LogMessages(log.Default()))
	}
	if c.rpc != nil {
		c.rpc.Close()
	}
	c.rpc = jsonrpc2.NewConn(ctx, stream, handler, opts...)
	server := protocol.NewServer(c.rpc)
	go func() {
		<-c.rpc.DisconnectNotify()
		log.Printf("jsonrpc2 client connection to LSP sever disconnected\n")
	}()

	d, err := filepath.Abs(cfg.RootDirectory)
	if err != nil {
		return err
	}
	// acme-lsp edits individual files and has no single workspace root, so
	// RootDirectory defaults to the filesystem root ("/" or `C:\`) as a
	// "no root" sentinel.  Per the LSP spec rootUri must be null when no
	// folder is open, and is anyway deprecated in favour of workspaceFolders,
	// which we always send.  Advertising the filesystem root as rootUri is a
	// lie every server would mis-scope on; terraform-ls makes it fatal by
	// eagerly walking the whole disk at init and crashing.  Only send rootUri
	// when a real root was explicitly configured (via -rootdir or config).
	//
	// hasRoot drives the null-vs-string decision at marshal time: the go-lsp
	// RootURI field has no omitempty and is a plain string, so an empty value
	// serializes as "rootUri":"" — which strict servers such as taplo reject
	// with -32602 ("expected relative URL without a base").  initParams below
	// rewrites it to JSON null when there is no root.
	var rootURI protocol.DocumentURI
	hasRoot := d != "/" && d != `C:\`
	if hasRoot {
		rootURI = text.ToURI(d)
	}
	params := &protocol.ParamInitialize{
		XInitializeParams: protocol.XInitializeParams{
			RootURI: rootURI,
			Capabilities: protocol.ClientCapabilities{
				TextDocument: protocol.TextDocumentClientCapabilities{
					CodeAction: protocol.CodeActionClientCapabilities{
						CodeActionLiteralSupport: protocol.ClientCodeActionLiteralOptions{
							CodeActionKind: protocol.ClientCodeActionKindOptions{
								ValueSet: []protocol.CodeActionKind{
									protocol.SourceOrganizeImports,
								},
							},
						},
					},
					DocumentSymbol: protocol.DocumentSymbolClientCapabilities{
						HierarchicalDocumentSymbolSupport: true,
					},
					Completion: protocol.CompletionClientCapabilities{
						CompletionItem: protocol.ClientCompletionItemOptions{
							TagSupport: &protocol.CompletionItemTagOptions{
								ValueSet: []protocol.CompletionItemTag{},
							},
						},
					},
					SemanticTokens: protocol.SemanticTokensClientCapabilities{
						Formats: []protocol.TokenFormat{"relative"},
						Requests: protocol.ClientSemanticTokensRequestOptions{
							Full: &protocol.Or_ClientSemanticTokensRequestOptions_full{
								Value: true,
							},
						},
						MultilineTokenSupport:   true,
						OverlappingTokenSupport: true,
						TokenTypes: []string{
							"namespace", "type", "class", "enum", "interface",
							"struct", "typeParameter", "parameter", "variable",
							"property", "enumMember", "event", "function",
							"method", "macro", "keyword", "modifier", "comment",
							"string", "number", "regexp", "operator", "decorator",
						},
						TokenModifiers: []string{
							"declaration", "definition", "readonly", "static",
							"deprecated", "abstract", "async", "modification",
							"documentation", "defaultLibrary",
						},
					},
				},
				Workspace: protocol.WorkspaceClientCapabilities{
					WorkspaceFolders: true,
					ApplyEdit:        true,
				},
			},
			InitializationOptions: cfg.Options,
		},
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: cfg.Workspaces,
		},
	}

	var result *protocol.InitializeResult
	if err := c.rpc.Call(ctx, "initialize", &initParams{params, hasRoot}, &result); err != nil {
		return fmt.Errorf("initialize failed: %v", err)
	}
	if err := server.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
		return fmt.Errorf("initialized failed: %v", err)
	}

	c.Server = server
	c.initializeResult = result
	return nil
}

// InitializeResult implements proxy.Server.
func (c *Client) InitializeResult(context.Context, *protocol.TextDocumentIdentifier) (*protocol.InitializeResult, error) {
	return c.initializeResult, nil
}

// Version exists only to implement proxy.Server.
func (c *Client) Version(context.Context) (int, error) {
	panic("intentionally not implemented")
}

// WorkspaceFolders exists only to implement proxy.Server.
func (c *Client) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	panic("intentionally not implemented")
}

// ExecuteCommandOnDocument implements proxy.Server.
func (s *Client) ExecuteCommandOnDocument(ctx context.Context, params *proxy.ExecuteCommandOnDocumentParams) (interface{}, error) {
	return s.Server.ExecuteCommand(ctx, &params.ExecuteCommandParams)
}

// ExecuteCommandOnServer implements proxy.Server.
func (s *Client) ExecuteCommandOnServer(ctx context.Context, params *proxy.ExecuteCommandOnServerParams) (interface{}, error) {
	return s.Server.ExecuteCommand(ctx, &params.ExecuteCommandParams)
}
