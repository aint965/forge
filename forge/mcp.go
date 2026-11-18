// MCP: JSON-RPC 2.0 over streamable HTTP at /mcp, both protocol eras. The
// modern era answers server/discover with its supported revisions; the legacy
// era answers initialize plus notifications/initialized. Six tools share the
// store with REST, GraphQL and git, and tools/list is byte-identical on every
// call because it is built once.

package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const mcpServerName = "forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ---------------------------------------------------------------------------
// Tool definitions (built once, served byte-identical every time)

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp() map[string]any { return map[string]any{"type": "string"} }

func schemaOf(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the token owner.",
		InputSchema: schemaOf(map[string]any{
			"owner":       strProp(),
			"name":        strProp(),
			"description": strProp(),
			"private":     map[string]any{"type": "boolean"},
		}, []string{"name"}),
	},
	{
		Name:        "get_repo",
		Description: "Read one repository by owner and name.",
		InputSchema: schemaOf(map[string]any{
			"owner": strProp(),
			"repo":  strProp(),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: schemaOf(map[string]any{
			"owner": strProp(),
			"repo":  strProp(),
			"title": strProp(),
			"body":  strProp(),
		}, []string{"owner", "repo", "title"}),
	},
	{
		Name:        "list_issues",
		Description: "List a repository's issues, optionally filtered by state.",
		InputSchema: schemaOf(map[string]any{
			"owner": strProp(),
			"repo":  strProp(),
			"state": strProp(),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: schemaOf(map[string]any{
			"owner": strProp(),
			"repo":  strProp(),
			"title": strProp(),
			"body":  strProp(),
			"head":  strProp(),
			"base":  strProp(),
		}, []string{"owner", "repo", "title", "head", "base"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request into its base branch.",
		InputSchema: schemaOf(map[string]any{
			"owner":       strProp(),
			"repo":        strProp(),
			"pull_number": map[string]any{"type": "integer"},
		}, []string{"owner", "repo", "pull_number"}),
	},
}

// toolsListResult is the tools/list result, marshalled once.
var toolsListResult json.RawMessage

func init() {
	toolsListResult = mustJSON(map[string]any{"tools": mcpTools})
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) response {
	envelope := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result}
	return jsonResponse(200, mustJSON(envelope))
}

func rpcError(id json.RawMessage, status, code int, message string) response {
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(nullID(id)),
		"error":   map[string]any{"code": code, "message": message},
	}
	return jsonResponse(status, mustJSON(envelope))
}

func nullID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// handleMCP answers POST /mcp. The caller holds the store lock and has
// already checked the bearer token.
func (s *server) handleMCP(q *request) response {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(q.body, &raw); err != nil {
		return rpcError(nil, 400, -32700, "Parse error")
	}
	var req rpcRequest
	if err := json.Unmarshal(q.body, &req); err != nil {
		return rpcError(nil, 400, -32700, "Parse error")
	}
	_, hasID := raw["id"]
	if !hasID {
		// A notification: no response body, just an acceptance.
		return response{status: 202, contentType: "application/json", body: []byte{}}
	}
	if req.Method == "" {
		return rpcError(req.ID, 200, -32600, "Invalid Request: no method")
	}
	switch req.Method {
	case "server/discover":
		return rpcResult(req.ID, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			},
			"instructions": "A GitHub-compatible forge: repositories, issues and pull requests over MCP tools.",
		})
	case "initialize":
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)
		version := "2025-11-25"
		if v, ok := toString(params["protocolVersion"]); ok && v != "" {
			version = v
		}
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
	case "tools/list":
		return rpcResult(req.ID, json.RawMessage(toolsListResult))
	case "tools/call":
		return s.mcpToolsCall(req)
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return response{status: 202, contentType: "application/json", body: []byte{}}
		}
		return rpcError(req.ID, 200, -32601, "Method not found: "+req.Method)
	}
}

// ---------------------------------------------------------------------------
// tools/call

func (s *server) mcpToolsCall(req rpcRequest) response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	// A full_name argument ("owner/repo") fills owner and repo when the
	// caller addressed the repository that way.
	if _, hasOwner := params.Arguments["owner"]; !hasOwner {
		if full, ok := toString(params.Arguments["full_name"]); ok && strings.Contains(full, "/") {
			parts := strings.SplitN(full, "/", 2)
			params.Arguments["owner"] = parts[0]
			params.Arguments["repo"] = parts[1]
		}
	}
	args := params.Arguments
	need := func(keys ...string) (string, bool) {
		for _, k := range keys {
			v, ok := args[k]
			if !ok || v == nil {
				return k, false
			}
			if str, isStr := v.(string); isStr && str == "" {
				return k, false
			}
		}
		return "", true
	}
	fail := func(message string) response {
		return rpcResult(req.ID, map[string]any{
			"content": []map[string]string{{"type": "text", "text": message}},
			"isError": true,
		})
	}
	okResult := func(value any) response {
		text := string(mustJSON(value))
		return rpcResult(req.ID, map[string]any{
			"content":           []map[string]string{{"type": "text", "text": text}},
			"structuredContent": value,
		})
	}
	ownerOf := func() string {
		if o, ok := toString(args["owner"]); ok {
			return o
		}
		return s.st.viewerLogin
	}
	switch params.Name {
	case "create_repo":
		if missing, ok := need("name"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		name, _ := toString(args["name"])
		description, hasDesc := toString(args["description"])
		private, _ := args["private"].(bool)
		owner := ownerOf()
		r := s.st.createRepo(owner, name, description, hasDesc, private)
		if r == nil {
			return fail("repository " + owner + "/" + name + " already exists")
		}
		return okResult(s.repoView(r))
	case "get_repo":
		if missing, ok := need("owner", "repo"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		owner, _ := toString(args["owner"])
		name, _ := toString(args["repo"])
		r := s.st.repo(owner, name)
		if r == nil {
			return fail("repository " + owner + "/" + name + " not found")
		}
		return okResult(s.repoView(r))
	case "create_issue":
		if missing, ok := need("owner", "repo", "title"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		owner, _ := toString(args["owner"])
		name, _ := toString(args["repo"])
		title, _ := toString(args["title"])
		bodyText, _ := toString(args["body"])
		r := s.st.repo(owner, name)
		if r == nil {
			return fail("repository " + owner + "/" + name + " not found")
		}
		it := s.st.createIssue(r, title, bodyText, nil)
		return okResult(s.issueView(r, it))
	case "list_issues":
		if missing, ok := need("owner", "repo"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		owner, _ := toString(args["owner"])
		name, _ := toString(args["repo"])
		state, _ := toString(args["state"])
		state = strings.ToLower(state)
		r := s.st.repo(owner, name)
		if r == nil {
			return fail("repository " + owner + "/" + name + " not found")
		}
		views := []issueJSON{}
		for _, it := range r.issues {
			if state != "" && state != "all" && it.state != state {
				continue
			}
			views = append(views, s.issueView(r, it))
		}
		return okResult(views)
	case "create_pull_request":
		if missing, ok := need("owner", "repo", "title", "head", "base"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		owner, _ := toString(args["owner"])
		name, _ := toString(args["repo"])
		title, _ := toString(args["title"])
		bodyText, _ := toString(args["body"])
		head, _ := toString(args["head"])
		base, _ := toString(args["base"])
		head = stripOwnerPrefix(head)
		base = stripOwnerPrefix(base)
		r := s.st.repo(owner, name)
		if r == nil {
			return fail("repository " + owner + "/" + name + " not found")
		}
		headSha, okHead := r.refs["refs/heads/"+head]
		baseSha, okBase := r.refs["refs/heads/"+base]
		if !okHead {
			return fail("the head branch " + head + " does not exist")
		}
		if !okBase {
			return fail("the base branch " + base + " does not exist")
		}
		pr := s.st.createPull(r, title, bodyText, head, base, headSha, baseSha)
		return okResult(s.pullView(r, pr))
	case "merge_pull_request":
		if missing, ok := need("owner", "repo", "pull_number"); !ok {
			return rpcError(req.ID, 200, -32602, "missing required argument: "+missing)
		}
		owner, _ := toString(args["owner"])
		name, _ := toString(args["repo"])
		number, okNum := toInt(args["pull_number"])
		if !okNum {
			if str, okStr := toString(args["pull_number"]); okStr {
				if n, err := strconv.Atoi(str); err == nil {
					number, okNum = n, true
				}
			}
		}
		if !okNum {
			return rpcError(req.ID, 200, -32602, "pull_number must be an integer")
		}
		r := s.st.repo(owner, name)
		if r == nil {
			return fail("repository " + owner + "/" + name + " not found")
		}
		pr := r.pullsByNum[number]
		if pr == nil {
			return fail(fmt.Sprintf("pull request #%d not found", number))
		}
		sha, conflict, already := s.mergePullRequest(r, pr)
		if already {
			return fail(fmt.Sprintf("pull request #%d is already closed", number))
		}
		if conflict {
			return fail(fmt.Sprintf("pull request #%d cannot be merged: conflicting changes", number))
		}
		return okResult(map[string]any{
			"number": number, "merged": true, "state": "closed", "sha": sha.String(),
		})
	default:
		return rpcError(req.ID, 200, -32602, "unknown tool: "+params.Name)
	}
}
