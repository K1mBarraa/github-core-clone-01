package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// MCP: JSON-RPC 2.0 over Streamable HTTP at /mcp. Ports mcp.ts: the modern
// 2026-07-28 surface (server/discover, then tools/list and tools/call) and
// the legacy initialize handshake both work. The tools act on the same
// store as REST and GraphQL. A missing or wrong bearer token is 401; a body
// that is not JSON is HTTP 400 with a -32700 parse error.
//
// The server takes the store lock before calling HandleMcp (Lock for MCP —
// tools include mutations), so this file must never lock.

const mcpServerName = "github-core-clone-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = "A GitHub-compatible git forge. Use the tools to manage repositories, issues and pull requests."

// ---------------------------------------------------------------------------
// Argument helpers (port of pickArg / ownerRepo / toolRepo in mcp.ts)
// ---------------------------------------------------------------------------

// pickArg returns the first defined (present and non-null) argument among
// several names; the client maps synonyms.
func mcpPickArg(args map[string]any, names ...string) any {
	for _, name := range names {
		if v, ok := args[name]; ok && v != nil {
			return v
		}
	}
	return nil
}

func mcpPickString(args map[string]any, names ...string) (string, bool) {
	if v := mcpPickArg(args, names...); v != nil {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// mcpOwnerRepo splits an owner/repo pair out of the arguments (full_name
// or owner+repo).
func mcpOwnerRepo(args map[string]any) (owner, repo string) {
	if full, ok := mcpPickString(args, "full_name", "fullName", "repository"); ok {
		if i := strings.Index(full, "/"); i >= 0 {
			return full[:i], full[i+1:]
		}
	}
	owner, _ = mcpPickString(args, "owner", "owner_login", "ownerLogin")
	repo, _ = mcpPickString(args, "repo", "repository", "repo_name", "repoName")
	return owner, repo
}

// mcpToolRepo returns the repository a tool call names, or an error message.
func mcpToolRepo(ctx *Ctx, args map[string]any) (*Repo, error) {
	owner, repo := mcpOwnerRepo(args)
	if owner != ViewerLogin {
		return nil, fmt.Errorf("Repository %s/%s not found", owner, repo)
	}
	found := ctx.Store.GetRepo(owner, repo)
	if found == nil {
		return nil, fmt.Errorf("Repository %s/%s not found", owner, repo)
	}
	return found, nil
}

// mcpNumberArg reads a JSON number argument. JSON numbers decode as
// float64; a non-integral value can never match an issue/pull number.
func mcpNumberArg(args map[string]any, names ...string) (float64, bool) {
	if v := mcpPickArg(args, names...); v != nil {
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// mcpIntArg converts a number argument to an int, reporting whether it is
// a whole number. A fractional number is a real JSON number in the TS
// reference, so it proceeds to the lookup and misses instead of failing
// the "required" check.
func mcpIntArg(args map[string]any, names ...string) (n int, whole bool, present bool) {
	f, ok := mcpNumberArg(args, names...)
	if !ok {
		return 0, false, false
	}
	if f != math.Trunc(f) {
		return 0, false, true
	}
	return int(f), true, true
}

// mcpFormatNumber renders a JSON number the way JS String(number) does,
// for the not-found messages of fractional numbers.
func mcpFormatNumber(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// mcpLookupNumber resolves an issue/pull number argument the way the TS
// reference does: a missing or non-numeric value is "required", while a
// fractional number is a real JSON number, so it proceeds to a lookup that
// misses (notFoundFmt takes the JS rendering of the number, e.g. "1.5").
func mcpLookupNumber(args map[string]any, requiredMsg, notFoundFmt string, names ...string) (int, error) {
	number, whole, present := mcpIntArg(args, names...)
	if !present {
		return 0, errors.New(requiredMsg)
	}
	if !whole {
		f, _ := mcpNumberArg(args, names...)
		return 0, fmt.Errorf(notFoundFmt, mcpFormatNumber(f))
	}
	return number, nil
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

type mcpTool struct {
	name        string
	description string
	schema      mcpInputSchema
	call        func(ctx *Ctx, args map[string]any) (any, error)
}

type mcpSchemaProp struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type mcpInputSchema struct {
	Type       string                   `json:"type"`
	Properties map[string]mcpSchemaProp `json:"properties,omitempty"`
	Required   []string                 `json:"required,omitempty"`
}

func mcpStrProp(desc string) mcpSchemaProp { return mcpSchemaProp{Type: "string", Description: desc} }
func mcpBoolProp(desc string) mcpSchemaProp {
	return mcpSchemaProp{Type: "boolean", Description: desc}
}
func mcpIntProp(desc string) mcpSchemaProp { return mcpSchemaProp{Type: "integer", Description: desc} }

var mcpTools = []mcpTool{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"name":        mcpStrProp("The name of the repository."),
				"description": mcpStrProp("A short description of the repository."),
				"private":     mcpBoolProp("Whether the repository is private."),
			},
			Required: []string{"name"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			name, ok := mcpPickString(args, "name", "repo", "repo_name")
			if !ok || strings.TrimSpace(name) == "" {
				return nil, errors.New("name is required")
			}
			if ctx.Store.GetRepo(ViewerLogin, name) != nil {
				return nil, fmt.Errorf("Repository %s already exists", name)
			}
			desc, _ := mcpPickString(args, "description")
			priv, _ := mcpPickArg(args, "private").(bool)
			repo := ctx.Store.CreateRepo(ViewerLogin, name, desc, priv)
			if repo == nil {
				return nil, fmt.Errorf("Repository %s already exists", name)
			}
			return ctx.Store.RepoJSON(repo), nil
		},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner": mcpStrProp("The owner of the repository."),
				"repo":  mcpStrProp("The name of the repository."),
			},
			Required: []string{"owner", "repo"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			return ctx.Store.RepoJSON(repo), nil
		},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner": mcpStrProp("The owner of the repository."),
				"repo":  mcpStrProp("The name of the repository."),
				"title": mcpStrProp("The title of the issue."),
				"body":  mcpStrProp("The body of the issue."),
			},
			Required: []string{"owner", "repo", "title"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			title, ok := mcpPickString(args, "title")
			if !ok || strings.TrimSpace(title) == "" {
				return nil, errors.New("title is required")
			}
			body, _ := mcpPickString(args, "body")
			issue := ctx.Store.CreateIssue(repo, title, body, []string{})
			return ctx.Store.IssueJSON(repo, issue), nil
		},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner": mcpStrProp("The owner of the repository."),
				"repo":  mcpStrProp("The name of the repository."),
				"state": {Type: "string", Description: "Filter by state: open, closed, or all.", Enum: []string{"open", "closed", "all"}},
			},
			Required: []string{"owner", "repo"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			state, ok := mcpPickString(args, "state")
			if !ok {
				state = "open"
			}
			out := make([]map[string]any, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				if state != "all" && issue.State != state {
					continue
				}
				out = append(out, ctx.Store.IssueJSON(repo, issue))
			}
			return out, nil
		},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request in a repository.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner": mcpStrProp("The owner of the repository."),
				"repo":  mcpStrProp("The name of the repository."),
				"title": mcpStrProp("The title of the pull request."),
				"head":  mcpStrProp("The branch with the changes."),
				"base":  mcpStrProp("The branch to merge into."),
				"body":  mcpStrProp("The body of the pull request."),
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			title, ok := mcpPickString(args, "title")
			if !ok || strings.TrimSpace(title) == "" {
				return nil, errors.New("title is required")
			}
			head, _ := mcpPickString(args, "head", "head_ref", "headRef")
			base, _ := mcpPickString(args, "base", "base_ref", "baseRef")
			if head == "" || base == "" {
				return nil, errors.New("head and base are required")
			}
			headSha, okHead := ctx.Store.RefOID(repo, "refs/heads/"+head)
			baseSha, okBase := ctx.Store.RefOID(repo, "refs/heads/"+base)
			if !okHead || !okBase {
				return nil, errors.New("head or base branch does not exist")
			}
			body, _ := mcpPickString(args, "body")
			pr := ctx.Store.CreatePull(repo, title, body, head, headSha, base, baseSha)
			return ctx.Store.PullJSON(repo, pr), nil
		},
	},
	{
		name:        "merge_pull_request",
		description: "Merge a pull request with a squash commit.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner":       mcpStrProp("The owner of the repository."),
				"repo":        mcpStrProp("The name of the repository."),
				"pull_number": mcpIntProp("The number of the pull request."),
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			number, err := mcpLookupNumber(args, "pull_number is required", "Pull request #%s not found", "pull_number", "pullNumber", "pull_request_number", "number")
			if err != nil {
				return nil, err
			}
			pr := ctx.Store.FindPull(repo, number)
			if pr == nil {
				return nil, fmt.Errorf("Pull request #%d not found", number)
			}
			if pr.Merged {
				return nil, errors.New("Pull request already merged")
			}
			if pr.State != "open" {
				return nil, errors.New("Pull request is closed")
			}
			headSha, okHead := ctx.Store.RefOID(repo, "refs/heads/"+pr.HeadRef)
			baseSha, okBase := ctx.Store.RefOID(repo, "refs/heads/"+pr.BaseRef)
			if !okHead || !okBase {
				return nil, errors.New("head or base branch does not exist")
			}
			flat, conflict := ctx.Store.ThreeWayMerge(repo, baseSha, headSha)
			if conflict {
				return nil, errors.New("Pull request has conflicts")
			}
			treeOID := ctx.Store.BuildTreeFromFlat(repo, flat)
			mergeSha := ctx.Store.CreateCommit(repo, treeOID, []string{baseSha}, pr.Title, nil)
			ctx.Store.SetRef(repo, "refs/heads/"+pr.BaseRef, mergeSha)
			pr.State = "closed"
			pr.Merged = true
			mergedAt := ctx.Store.IsoSec(ctx.Store.NowSec())
			pr.MergedAt = &mergedAt
			pr.MergeCommitSha = &mergeSha
			pr.BaseSha = mergeSha
			return map[string]any{"merged": true, "sha": mergeSha, "message": "Pull request successfully merged"}, nil
		},
	},
	{
		name:        "list_repos",
		description: "List the repositories owned by the authenticated user.",
		schema:      mcpInputSchema{Type: "object", Properties: map[string]mcpSchemaProp{}},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repos := ctx.Store.ListViewerRepos()
			out := make([]map[string]any, 0, len(repos))
			for _, repo := range repos {
				out = append(out, ctx.Store.RepoJSON(repo))
			}
			return out, nil
		},
	},
	{
		name:        "get_issue",
		description: "Get an issue by number.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner":        mcpStrProp("The owner of the repository."),
				"repo":         mcpStrProp("The name of the repository."),
				"issue_number": mcpIntProp("The number of the issue."),
			},
			Required: []string{"owner", "repo", "issue_number"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			number, err := mcpLookupNumber(args, "issue_number is required", "Issue #%s not found", "issue_number", "issueNumber", "number")
			if err != nil {
				return nil, err
			}
			issue := ctx.Store.FindIssue(repo, number)
			if issue == nil {
				return nil, fmt.Errorf("Issue #%d not found", number)
			}
			return ctx.Store.IssueJSON(repo, issue), nil
		},
	},
	{
		name:        "create_comment",
		description: "Create a comment on an issue.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner":        mcpStrProp("The owner of the repository."),
				"repo":         mcpStrProp("The name of the repository."),
				"issue_number": mcpIntProp("The number of the issue."),
				"body":         mcpStrProp("The body of the comment."),
			},
			Required: []string{"owner", "repo", "issue_number", "body"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			number, err := mcpLookupNumber(args, "issue_number is required", "Issue #%s not found", "issue_number", "issueNumber", "number")
			if err != nil {
				return nil, err
			}
			issue := ctx.Store.FindIssue(repo, number)
			if issue == nil {
				return nil, fmt.Errorf("Issue #%d not found", number)
			}
			body, ok := mcpPickString(args, "body")
			if !ok {
				return nil, errors.New("body is required")
			}
			comment := ctx.Store.AddComment(repo, issue, body)
			return ctx.Store.CommentJSON(repo, issue, comment), nil
		},
	},
	{
		name:        "list_pull_requests",
		description: "List the pull requests of a repository.",
		schema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpSchemaProp{
				"owner": mcpStrProp("The owner of the repository."),
				"repo":  mcpStrProp("The name of the repository."),
				"state": {Type: "string", Description: "Filter by state: open, closed, or all.", Enum: []string{"open", "closed", "all"}},
			},
			Required: []string{"owner", "repo"},
		},
		call: func(ctx *Ctx, args map[string]any) (any, error) {
			repo, err := mcpToolRepo(ctx, args)
			if err != nil {
				return nil, err
			}
			state, ok := mcpPickString(args, "state")
			if !ok {
				state = "open"
			}
			out := make([]map[string]any, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if state != "all" && pr.State != state {
					continue
				}
				out = append(out, ctx.Store.PullJSON(repo, pr))
			}
			return out, nil
		},
	},
}

// ---------------------------------------------------------------------------
// JSON-RPC envelopes
// ---------------------------------------------------------------------------

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools map[string]any `json:"tools"`
}

func mcpEmptyCapabilities() mcpCapabilities {
	return mcpCapabilities{Tools: map[string]any{}}
}

type mcpErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpOKResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result"`
}

type mcpErrorResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Error   mcpErrorBody `json:"error"`
}

func mcpResponse(id any, result any) mcpOKResponse {
	return mcpOKResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpError(id any, code int, message string) mcpErrorResponse {
	return mcpErrorResponse{JSONRPC: "2.0", ID: id, Error: mcpErrorBody{Code: code, Message: message}}
}

type mcpToolListEntry struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpContentPart `json:"content"`
	IsError bool             `json:"isError"`
}

// mcpRequestedVersion returns the protocol version the client asked for,
// or the newest we speak.
func mcpRequestedVersion(request map[string]any) string {
	if meta, ok := request["_meta"].(map[string]any); ok {
		if v, ok := meta["io.modelcontextprotocol/protocolVersion"].(string); ok && v != "" {
			return v
		}
	}
	if params, ok := request["params"].(map[string]any); ok {
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			return v
		}
	}
	return mcpSupportedVersions[0]
}

// mcpHandleRpc handles one JSON-RPC request. It returns nil for a
// notification (no reply).
func mcpHandleRpc(ctx *Ctx, request map[string]any) any {
	id, hasID := request["id"]
	isNotification := !hasID || id == nil
	method, methodOK := request["method"].(string)
	if !methodOK {
		if isNotification {
			return nil
		}
		return mcpError(id, -32600, "Invalid Request: method is required")
	}
	// Notifications get no reply (a 202 with an empty body from the handler).
	if isNotification {
		return nil
	}

	switch method {
	case "server/discover":
		return mcpResponse(id, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"serverInfo":        mcpServerInfo{Name: mcpServerName, Version: mcpServerVersion},
			"capabilities":      mcpEmptyCapabilities(),
		})
	case "initialize":
		return mcpResponse(id, struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    mcpCapabilities `json:"capabilities"`
			ServerInfo      mcpServerInfo   `json:"serverInfo"`
			Instructions    string          `json:"instructions"`
		}{
			ProtocolVersion: mcpRequestedVersion(request),
			Capabilities:    mcpEmptyCapabilities(),
			ServerInfo:      mcpServerInfo{Name: mcpServerName, Version: mcpServerVersion},
			Instructions:    mcpInstructions,
		})
	case "ping":
		return mcpResponse(id, map[string]any{})
	case "tools/list":
		tools := make([]mcpToolListEntry, 0, len(mcpTools))
		for _, tool := range mcpTools {
			tools = append(tools, mcpToolListEntry{Name: tool.name, Description: tool.description, InputSchema: tool.schema})
		}
		return mcpResponse(id, map[string]any{"tools": tools})
	case "tools/call":
		params, _ := request["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		name, _ := params["name"].(string)
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].name == name {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return mcpError(id, -32601, fmt.Sprintf("Unknown tool: %s", name))
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, err := tool.call(ctx, args)
		if err != nil {
			return mcpResponse(id, mcpCallResult{
				Content: []mcpContentPart{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
		}
		return mcpResponse(id, mcpCallResult{
			Content: []mcpContentPart{{Type: "text", Text: JSONText(result)}},
			IsError: false,
		})
	default:
		return mcpError(id, -32601, fmt.Sprintf("Method not found: %s", method))
	}
}

// ---------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------

// HandleMcp handles POST /mcp. It returns false when the path is not
// ours, so the router falls through to a 404.
func HandleMcp(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Path != "/mcp" {
		return false
	}
	if ctx.Method != "POST" {
		SendNotFound(w, "Not Found")
		return true
	}
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return true
	}
	// A body that is not JSON is HTTP 400 with a -32700 parse error.
	var request any
	if err := json.Unmarshal(ctx.Body, &request); err != nil {
		SendJSON(w, 400, mcpError(nil, -32700, "Parse error"), nil)
		return true
	}
	obj, ok := request.(map[string]any)
	if !ok {
		SendJSON(w, 400, mcpError(nil, -32600, "Invalid Request"), nil)
		return true
	}
	result := mcpHandleRpc(ctx, obj)
	if result == nil {
		// A notification: 202 with no body (base headers only, as in
		// respond.ts: writeHead(202, baseHeaders())).
		w.Header().Set("X-GitHub-Request-Id", NextRequestID())
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	SendJSON(w, 200, result, nil)
	return true
}
