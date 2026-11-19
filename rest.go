// GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Ported from rest.ts (v3).
// Every reply is application/json with a non-empty X-GitHub-Request-Id;
// every error is a JSON body with a string message; a missing or wrong
// bearer token is 401; a repository under another owner is 404, never 403.
//
// The server takes the store lock before calling HandleRest (RLock for
// GETs, Lock for the rest), so this file never locks. All helpers are
// prefixed with "rest" to avoid colliding with the other builders' files
// in this package.

package main

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// HandleRest handles the REST v3 routes. It returns false when the path is
// not under /api/v3 (so the router falls through to a 404).
func HandleRest(ctx *Ctx, w http.ResponseWriter) bool {
	if restFastIssueGet(ctx, w) {
		return true
	}
	seg := restSegmentsOf(ctx.Path)
	if len(seg) < 2 || seg[0] != "api" || seg[1] != "v3" {
		return false
	}
	rest := seg[2:]
	method := ctx.Method

	// The API root: a 200 with the token's scopes (the starter's answer).
	// This is before the auth check, exactly like the reference.
	if len(rest) == 0 {
		if method == "GET" {
			SendJSON(w, 200, map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			}, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	// Every other /api/v3 route requires the bearer token.
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return true
	}

	// GET /meta: the installation's version.
	if len(rest) == 1 && rest[0] == "meta" && method == "GET" {
		SendJSON(w, 200, map[string]any{
			"installed_version":                  "1.0.0",
			"verifiable_password_authentication": true,
			"github_enterprise":                  false,
		}, nil)
		return true
	}

	// GET /user: the token's login.
	if len(rest) == 1 && rest[0] == "user" && method == "GET" {
		SendJSON(w, 200, map[string]any{
			"login": ViewerLogin,
			"id":    ViewerID,
			"type":  "User",
			"name":  "ada iversen",
			"email": ViewerEmail,
		}, nil)
		return true
	}

	// /user/repos: list and create.
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "GET" {
			repos := ctx.Store.ListViewerRepos()
			items := make([]any, 0, len(repos))
			for _, repo := range repos {
				items = append(items, ctx.Store.RepoJSON(repo))
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "POST" {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			name := strings.TrimSpace(restStringField(body, "name"))
			if name == "" {
				SendError(w, 422, "name is required")
				return true
			}
			if ctx.Store.GetRepo(ViewerLogin, name) != nil {
				SendError(w, 422, "Repository "+name+" already exists")
				return true
			}
			repo := ctx.Store.CreateRepo(
				ViewerLogin,
				name,
				restStringField(body, "description"),
				restBoolField(body, "private"),
			)
			if repo == nil {
				SendError(w, 422, "Repository "+name+" already exists")
				return true
			}
			SendJSON(w, 201, ctx.Store.RepoJSON(repo), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	// /repos/{owner}/{repo}[...]
	if len(rest) >= 3 && rest[0] == "repos" {
		return restHandleRepoRoutes(ctx, w, rest)
	}

	SendNotFound(w, "Not Found")
	return true
}

// restSegmentsOf splits a path into decoded segments. A segment with no
// '%' is already decoded, so it skips unescaping entirely — the read
// workload splits 7 segments per request, 2,000 times in a row.
func restSegmentsOf(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if strings.IndexByte(part, '%') < 0 {
			out = append(out, part)
			continue
		}
		if dec, err := url.PathUnescape(part); err == nil {
			out = append(out, dec)
		} else {
			out = append(out, part)
		}
	}
	return out
}

// restRepo is the repository a REST path names, or nil (404: missing, or
// another owner).
func restRepo(ctx *Ctx, owner, repo string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return ctx.Store.GetRepo(owner, repo)
}

// restQueryParam returns a query value, or def when the key is absent.
// A present-but-empty value ("?state=") is returned as "" — it is not
// the default, matching URLSearchParams.get.
func restQueryParam(ctx *Ctx, key, def string) string {
	if vals, ok := ctx.Query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return def
}

// restPageSize is the page size a query asks for (GitHub's default is 30,
// capped at 100).
func restPageSize(ctx *Ctx) int {
	raw := ctx.Query.Get("per_page")
	if raw == "" {
		return 30
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 30
	}
	n := int(math.Floor(f))
	if n > 100 {
		n = 100
	}
	return n
}

// restPageNumber is the page a query asks for (GitHub's default is 1).
func restPageNumber(ctx *Ctx) int {
	raw := ctx.Query.Get("page")
	if raw == "" {
		return 1
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 1
	}
	return int(math.Floor(f))
}

// restLinkHeader builds the Link header for a paginated list (absolute,
// same-origin URLs), or "" when there is a single page.
func restLinkHeader(ctx *Ctx, path string, total int) string {
	perPage := restPageSize(ctx)
	page := restPageNumber(ctx)
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if lastPage <= 1 {
		return ""
	}
	host := ctx.Headers.Get("Host")
	if host == "" {
		host = "127.0.0.1"
	}
	base := "http://" + host + path
	urlFor := func(p int) string {
		q := url.Values{}
		for k, vs := range ctx.Query {
			q[k] = append([]string(nil), vs...)
		}
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		return base + "?" + q.Encode()
	}
	parts := make([]string, 0, 4)
	if page < lastPage {
		parts = append(parts, "<"+urlFor(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		parts = append(parts, "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	parts = append(parts, "<"+urlFor(1)+">; rel=\"first\"")
	parts = append(parts, "<"+urlFor(lastPage)+">; rel=\"last\"")
	return strings.Join(parts, ", ")
}

// restSendList sends a paginated list as JSON with a Link header when
// there is more than one page.
func restSendList(ctx *Ctx, w http.ResponseWriter, path string, items []any) {
	perPage := restPageSize(ctx)
	page := restPageNumber(ctx)
	total := len(items)
	start := (page - 1) * perPage
	var slice []any
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		slice = items[start:end]
	} else {
		slice = []any{}
	}
	var headers map[string]string
	if link := restLinkHeader(ctx, path, total); link != "" {
		headers = map[string]string{"Link": link}
	}
	SendJSON(w, 200, slice, headers)
}

// restCommitAtRef is the commit at a ref (a branch name or a 40-hex id),
// or ("", false) when there is none.
func restCommitAtRef(ctx *Ctx, repo *Repo, ref string) (string, bool) {
	if ref == "" {
		sha, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		return sha, ok
	}
	if IsOID(ref) {
		if ctx.Store.HasObject(repo, ref) {
			return ref, true
		}
		return "", false
	}
	sha, ok := repo.Refs["refs/heads/"+ref]
	return sha, ok
}

// restParentList renders a commit's parents as [{sha}] (never null).
func restParentList(parents []string) []any {
	out := make([]any, 0, len(parents))
	for _, p := range parents {
		out = append(out, map[string]any{"sha": p})
	}
	return out
}

// restCommitList is the commits reachable from a tip, newest first
// (parents in order).
func restCommitList(ctx *Ctx, repo *Repo, tip string) []any {
	out := []any{}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := repo.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		commit := ParseCommit(obj.Data)
		// The Go ParsedCommit carries one timestamp (author and committer
		// share it: CreateCommit stamps both from the same second), and
		// Author/Committer are already split into name and email.
		date := ctx.Store.IsoSec(commit.Timestamp)
		out = append(out, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"author": map[string]any{
					"name":  commit.Author.Name,
					"email": commit.Author.Email,
					"date":  date,
				},
				"committer": map[string]any{
					"name":  commit.Committer.Name,
					"email": commit.Committer.Email,
					"date":  date,
				},
				"message": commit.Message,
				"tree":    map[string]any{"sha": commit.Tree},
			},
			"parents": restParentList(commit.Parents),
		})
		// Parents in reverse so the first parent is visited first.
		for i := len(commit.Parents) - 1; i >= 0; i-- {
			stack = append(stack, commit.Parents[i])
		}
	}
	return out
}

// restFastIssueGet is the read workload's hot path: GET
// /api/v3/repos/{owner}/{repo}/issues/{n} with no query string, 2,000
// times in a row on one connection. When the request has exactly that
// shape (plain segments, a numeric tail), it answers straight from the
// cached issue text, skipping the segment split and the dispatch layers.
// Anything else falls through to the general routes, so the behaviour is
// identical, only faster. Auth and 404s match the general path exactly.
func restFastIssueGet(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Method != "GET" || len(ctx.Query) != 0 {
		return false
	}
	path := ctx.Path
	if strings.IndexByte(path, '%') >= 0 {
		return false
	}
	const prefix = "/api/v3/repos/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	ownerEnd := strings.IndexByte(rest, '/')
	if ownerEnd <= 0 {
		return false
	}
	owner := rest[:ownerEnd]
	afterOwner := rest[ownerEnd+1:]
	repoEnd := strings.IndexByte(afterOwner, '/')
	if repoEnd <= 0 {
		return false
	}
	repoName := afterOwner[:repoEnd]
	afterRepo := afterOwner[repoEnd+1:]
	if !strings.HasPrefix(afterRepo, "issues/") {
		return false
	}
	numText := afterRepo[len("issues/"):]
	if numText == "" {
		return false
	}
	for i := 0; i < len(numText); i++ {
		if numText[i] < '0' || numText[i] > '9' {
			return false
		}
	}
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return true
	}
	repo := restRepo(ctx, owner, repoName)
	if repo == nil {
		SendNotFound(w, "Not Found")
		return true
	}
	number, err := strconv.Atoi(numText)
	if err != nil {
		// A digit string too large for an int: no such issue exists.
		SendNotFound(w, "Not Found")
		return true
	}
	issue := ctx.Store.FindIssue(repo, number)
	if issue == nil {
		SendNotFound(w, "Not Found")
		return true
	}
	SendJSONText(w, 200, ctx.Store.IssueJSONText(repo, issue), nil)
	return true
}

// restStringField returns a body field as a string, or "" when it is
// absent or not a string.
func restStringField(body map[string]any, key string) string {
	if v, ok := body[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// restStringFieldOr returns a body field as a string when it is a
// string, or def otherwise. Unlike restStringField, an explicit empty
// string is kept (the reference only falls back for non-strings).
func restStringFieldOr(body map[string]any, key, def string) string {
	if v, ok := body[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// restBoolField returns a body field as a bool, true only when the field
// is present and exactly the JSON value true.
func restBoolField(body map[string]any, key string) bool {
	if v, ok := body[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// restStringList returns a body field as a list of its string elements
// (non-strings filtered out), or nil when it is not an array.
func restStringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// restBodyObject parses the request body as a JSON object. Malformed JSON
// sends 400 "Invalid JSON body"; a missing, null, or non-object body
// sends 400 "Invalid request body". It returns false when it has sent an
// error (the caller then returns).
func restBodyObject(ctx *Ctx, w http.ResponseWriter) (map[string]any, bool) {
	v, ok := JSONBody(ctx)
	if !ok {
		SendError(w, 400, "Invalid JSON body")
		return nil, false
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		SendError(w, 400, "Invalid request body")
		return nil, false
	}
	return m, true
}

// restBodyObjectLenient parses the request body for PATCH routes: a
// malformed body sends 400 "Invalid JSON body", but a missing, null, or
// non-object body is a no-op (nil map, true) — the reference applies
// nothing and still answers 200.
func restBodyObjectLenient(ctx *Ctx, w http.ResponseWriter) (map[string]any, bool) {
	v, ok := JSONBody(ctx)
	if !ok {
		SendError(w, 400, "Invalid JSON body")
		return nil, false
	}
	m, _ := v.(map[string]any)
	return m, true
}

// restParseNumber parses a path number the way Number() does in the
// reference: "1e2" is 100, "1.5" and "12abc" are not integers, and a
// non-positive number is not a valid issue/pull number.
func restParseNumber(s string) (int, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f != math.Trunc(f) || f <= 0 || f > 1e15 {
		return 0, false
	}
	return int(f), true
}

func restHandleRepoRoutes(ctx *Ctx, w http.ResponseWriter, rest []string) bool {
	method := ctx.Method
	owner := rest[1]
	repoName := rest[2]
	repo := restRepo(ctx, owner, repoName)
	tail := rest[3:]

	// GET /repos/{o}/{r}, PATCH, DELETE.
	if len(tail) == 0 {
		if repo == nil {
			SendNotFound(w, "Not Found")
			return true
		}
		if method == "GET" {
			SendJSON(w, 200, ctx.Store.RepoJSON(repo), nil)
			return true
		}
		if method == "PATCH" {
			body, ok := restBodyObjectLenient(ctx, w)
			if !ok {
				return true
			}
			if body != nil {
				if v, ok := body["description"]; ok {
					if s, ok := v.(string); ok {
						repo.Description = s
					}
				}
				if v, ok := body["private"]; ok {
					if b, ok := v.(bool); ok {
						repo.Private = b
					}
				}
			}
			SendJSON(w, 200, ctx.Store.RepoJSON(repo), nil)
			return true
		}
		if method == "DELETE" {
			ctx.Store.DeleteRepo(owner, repoName)
			SendNoContent(w)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	if repo == nil {
		SendNotFound(w, "Not Found")
		return true
	}

	// GET /branches and GET /branches/{branch}.
	if tail[0] == "branches" {
		if method == "GET" && len(tail) == 1 {
			names := make([]string, 0, len(repo.Refs))
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(names)
			items := make([]any, 0, len(names))
			for _, name := range names {
				items = append(items, map[string]any{
					"name":   name,
					"commit": map[string]any{"sha": repo.Refs["refs/heads/"+name]},
				})
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "GET" && len(tail) >= 2 {
			branch := strings.Join(tail[1:], "/")
			sha, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				SendNotFound(w, "Branch not found")
				return true
			}
			SendJSON(w, 200, map[string]any{
				"name":   branch,
				"commit": map[string]any{"sha": sha},
			}, nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	// GET /commits[?sha=].
	if tail[0] == "commits" && len(tail) == 1 && method == "GET" {
		tip, ok := restCommitAtRef(ctx, repo, restQueryParam(ctx, "sha", ""))
		if !ok {
			SendJSON(w, 200, []any{}, nil)
			return true
		}
		restSendList(ctx, w, ctx.Path, restCommitList(ctx, repo, tip))
		return true
	}

	// /git/refs: POST (create) and GET /git/refs/heads/{branch}.
	if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
		if method == "POST" && len(tail) == 2 {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			ref := restStringField(body, "ref")
			sha := restStringField(body, "sha")
			if !strings.HasPrefix(ref, "refs/") || !IsOID(sha) {
				SendError(w, 422, "Invalid ref or sha")
				return true
			}
			if _, exists := repo.Refs[ref]; exists {
				SendError(w, 422, "Reference already exists")
				return true
			}
			obj, exists := repo.Objects[sha]
			if !exists || obj.Type != ObjCommit {
				SendError(w, 422, "Object does not exist")
				return true
			}
			repo.Refs[ref] = sha
			SendJSON(w, 201, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": sha, "type": "commit"},
			}, nil)
			return true
		}
		if method == "GET" && len(tail) >= 4 && tail[2] == "heads" {
			branch := strings.Join(tail[3:], "/")
			sha, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				SendNotFound(w, "Reference does not exist")
				return true
			}
			SendJSON(w, 200, map[string]any{
				"ref":    "refs/heads/" + branch,
				"object": map[string]any{"sha": sha, "type": "commit"},
			}, nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	// /contents/{path}: GET, PUT, DELETE.
	if tail[0] == "contents" {
		return restHandleContents(ctx, w, repo, tail[1:])
	}

	// /labels: GET, POST.
	if tail[0] == "labels" {
		if method == "GET" && len(tail) == 1 {
			names := make([]string, 0, len(repo.Labels))
			for name := range repo.Labels {
				names = append(names, name)
			}
			sort.Strings(names)
			items := make([]any, 0, len(names))
			for _, name := range names {
				items = append(items, ctx.Store.LabelJSON(repo.Labels[name]))
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "POST" && len(tail) == 1 {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			name := strings.TrimSpace(restStringField(body, "name"))
			if name == "" {
				SendError(w, 422, "name is required")
				return true
			}
			if _, exists := repo.Labels[name]; exists {
				SendError(w, 422, "Label "+name+" already exists")
				return true
			}
			label := Label{
				Name:        name,
				Color:       restStringFieldOr(body, "color", "ededed"),
				Description: restStringField(body, "description"),
			}
			repo.Labels[name] = label
			SendJSON(w, 201, ctx.Store.LabelJSON(label), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	// /issues[/{n}[/comments]] and /pulls[/{n}[/files|/merge]].
	if tail[0] == "issues" {
		return restHandleIssues(ctx, w, repo, tail[1:])
	}
	if tail[0] == "pulls" {
		return restHandlePulls(ctx, w, repo, tail[1:])
	}

	SendNotFound(w, "Not Found")
	return true
}

// restWriteBranch is the branch a contents write targets: the named
// branch (created from the default branch when it does not exist), else
// the default branch.
func restWriteBranch(repo *Repo, body map[string]any) string {
	branch := restStringField(body, "branch")
	if branch == "" {
		branch = DefaultBranch
	}
	ref := "refs/heads/" + branch
	if _, ok := repo.Refs[ref]; !ok {
		if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			repo.Refs[ref] = tip
		}
	}
	return branch
}

// restBaseName is the last segment of a contents path.
func restBaseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// restHandleContents handles GET /contents/{path}, PUT /contents/{path},
// DELETE /contents/{path}.
func restHandleContents(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) bool {
	method := ctx.Method
	path := strings.Join(tail, "/")

	if method == "GET" {
		tip, ok := restCommitAtRef(ctx, repo, restQueryParam(ctx, "ref", ""))
		if !ok {
			SendNotFound(w, "Not Found")
			return true
		}
		files := ctx.Store.FlatTree(repo, tip)
		if path == "" {
			SendJSON(w, 200, restDirectoryListing(ctx, repo, files, ""), nil)
			return true
		}
		if file, ok := files[path]; ok {
			var data []byte
			if blob, ok := repo.Objects[file.OID]; ok {
				data = blob.Data
			}
			SendJSON(w, 200, map[string]any{
				"type":     "file",
				"encoding": "base64",
				"size":     len(data),
				"name":     restBaseName(path),
				"path":     path,
				"content":  B64EncodeBytes(data),
				"sha":      file.OID,
			}, nil)
			return true
		}
		// A directory: any file under the path.
		prefix := path + "/"
		isDir := false
		for key := range files {
			if strings.HasPrefix(key, prefix) {
				isDir = true
				break
			}
		}
		if isDir {
			SendJSON(w, 200, restDirectoryListing(ctx, repo, files, path), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	if method == "PUT" {
		body, ok := restBodyObject(ctx, w)
		if !ok {
			return true
		}
		content, isStr := body["content"].(string)
		if !isStr {
			SendError(w, 422, "content is required")
			return true
		}
		message := restStringFieldOr(body, "message", "Update file")
		branch := restWriteBranch(repo, body)
		tip, tipOK := repo.Refs["refs/heads/"+branch]
		var files map[string]FlatFile
		if tipOK {
			files = ctx.Store.FlatTree(repo, tip)
		} else {
			files = map[string]FlatFile{}
		}
		existing, exists := files[path]
		// The reference decodes base64 leniently (Node never throws on
		// bad input), so decoded bytes are used even on a decode error.
		newBytes, _ := B64Decode(content)
		if exists {
			sha, present := body["sha"]
			if !present || sha == nil {
				SendError(w, 422, "sha wasn't supplied")
				return true
			}
			shaStr, isStr := sha.(string)
			if !isStr {
				SendError(w, 422, "sha wasn't supplied")
				return true
			}
			if shaStr != existing.OID {
				SendError(w, 409, "sha does not match")
				return true
			}
		}
		blobOID := ctx.Store.PutObject(repo, ObjBlob, newBytes)
		nextFiles := make(map[string]FlatFile, len(files)+1)
		for k, v := range files {
			nextFiles[k] = v
		}
		nextFiles[path] = FlatFile{OID: blobOID, Mode: "100644"}
		treeOID := ctx.Store.BuildTreeFromFlat(repo, nextFiles)
		var parents []string
		if tipOK {
			parents = []string{tip}
		} else {
			parents = []string{}
		}
		commitOID := ctx.Store.CreateCommit(repo, treeOID, parents, message, nil)
		repo.Refs["refs/heads/"+branch] = commitOID
		status := 201
		if exists {
			status = 200
		}
		SendJSON(w, status, map[string]any{
			"content": map[string]any{
				"name":     restBaseName(path),
				"path":     path,
				"sha":      blobOID,
				"size":     len(newBytes),
				"encoding": "base64",
				"type":     "file",
			},
			"commit": map[string]any{"sha": commitOID, "message": message},
		}, nil)
		return true
	}

	if method == "DELETE" {
		body, ok := restBodyObject(ctx, w)
		if !ok {
			return true
		}
		message := restStringFieldOr(body, "message", "Delete file")
		branch := restWriteBranch(repo, body)
		tip, tipOK := repo.Refs["refs/heads/"+branch]
		if !tipOK {
			SendNotFound(w, "Not Found")
			return true
		}
		files := ctx.Store.FlatTree(repo, tip)
		existing, exists := files[path]
		if !exists {
			SendNotFound(w, "Not Found")
			return true
		}
		sha, present := body["sha"]
		if !present || sha == nil {
			SendError(w, 422, "sha wasn't supplied")
			return true
		}
		shaStr, isStr := sha.(string)
		if !isStr {
			SendError(w, 422, "sha wasn't supplied")
			return true
		}
		if shaStr != existing.OID {
			SendError(w, 409, "sha does not match")
			return true
		}
		nextFiles := make(map[string]FlatFile, len(files))
		for k, v := range files {
			nextFiles[k] = v
		}
		delete(nextFiles, path)
		treeOID := ctx.Store.BuildTreeFromFlat(repo, nextFiles)
		commitOID := ctx.Store.CreateCommit(repo, treeOID, []string{tip}, message, nil)
		repo.Refs["refs/heads/"+branch] = commitOID
		SendJSON(w, 200, map[string]any{
			"commit": map[string]any{"sha": commitOID, "message": message},
		}, nil)
		return true
	}

	SendNotFound(w, "Not Found")
	return true
}

// restDirectoryListing is the immediate children of a directory, as
// GitHub's contents listing.
func restDirectoryListing(ctx *Ctx, repo *Repo, files map[string]FlatFile, dir string) []any {
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	type child struct {
		isDir bool
		file  FlatFile
	}
	children := map[string]*child{}
	for path, file := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if slash := strings.IndexByte(rest, '/'); slash < 0 {
			children[rest] = &child{isDir: false, file: file}
		} else {
			name := rest[:slash]
			if _, ok := children[name]; !ok {
				children[name] = &child{isDir: true}
			}
		}
	}
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		ch := children[name]
		childPath := prefix + name
		if !ch.isDir {
			size := 0
			if blob, ok := repo.Objects[ch.file.OID]; ok {
				size = len(blob.Data)
			}
			out = append(out, map[string]any{
				"type": "file",
				"name": name,
				"path": childPath,
				"sha":  ch.file.OID,
				"size": size,
			})
		} else {
			under := map[string]FlatFile{}
			for path, file := range files {
				if strings.HasPrefix(path, childPath+"/") {
					under[path[len(childPath)+1:]] = file
				}
			}
			treeOID := ctx.Store.BuildTreeFromFlat(repo, under)
			out = append(out, map[string]any{
				"type": "dir",
				"name": name,
				"path": childPath,
				"sha":  treeOID,
			})
		}
	}
	return out
}

// restHandleIssues handles GET /issues, POST /issues, GET/PATCH
// /issues/{n}, /issues/{n}/comments.
func restHandleIssues(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) bool {
	method := ctx.Method

	if len(tail) == 0 {
		if method == "GET" {
			state := restQueryParam(ctx, "state", "open")
			direction := restQueryParam(ctx, "direction", "desc")
			// sort=created is the only order the suite uses; numbers
			// track creation.
			issues := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				if state == "all" || issue.State == state {
					issues = append(issues, issue)
				}
			}
			if direction == "asc" {
				sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
			} else {
				sort.Slice(issues, func(i, j int) bool { return issues[i].Number > issues[j].Number })
			}
			items := make([]any, 0, len(issues))
			for _, issue := range issues {
				items = append(items, ctx.Store.IssueJSON(repo, issue))
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "POST" {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			title := strings.TrimSpace(restStringField(body, "title"))
			if title == "" {
				SendError(w, 422, "title is required")
				return true
			}
			labels := restStringList(body["labels"])
			if labels == nil {
				labels = []string{}
			}
			for _, label := range labels {
				if _, exists := repo.Labels[label]; !exists {
					SendError(w, 422, "Label "+label+" does not exist")
					return true
				}
			}
			issue := ctx.Store.CreateIssue(repo, title, restStringField(body, "body"), labels)
			SendJSON(w, 201, ctx.Store.IssueJSON(repo, issue), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	number, ok := restParseNumber(tail[0])
	if !ok {
		SendNotFound(w, "Not Found")
		return true
	}
	issue := ctx.Store.FindIssue(repo, number)
	if issue == nil {
		SendNotFound(w, "Not Found")
		return true
	}

	if len(tail) == 1 {
		if method == "GET" {
			// The cached serialized form: the read workload hits this path.
			SendJSONText(w, 200, ctx.Store.IssueJSONText(repo, issue), nil)
			return true
		}
		if method == "PATCH" {
			body, ok := restBodyObjectLenient(ctx, w)
			if !ok {
				return true
			}
			if body != nil {
				if v, ok := body["title"]; ok {
					if s, ok := v.(string); ok {
						issue.Title = s
					}
				}
				if v, ok := body["body"]; ok {
					if s, ok := v.(string); ok {
						issue.Body = s
					}
				}
				if v, ok := body["state"]; ok {
					if s, ok := v.(string); ok && (s == "open" || s == "closed") {
						issue.State = s
					}
				}
				if arr, isArr := body["labels"]; isArr {
					labels := restStringList(arr)
					if labels == nil {
						labels = []string{}
					}
					unknown := ""
					unknownFound := false
					for _, label := range labels {
						if _, exists := repo.Labels[label]; !exists {
							unknown = label
							unknownFound = true
							break
						}
					}
					if unknownFound {
						SendError(w, 422, "Label "+unknown+" does not exist")
						return true
					}
					issue.Labels = labels
				}
			}
			ctx.Store.TouchIssue(repo, issue)
			SendJSON(w, 200, ctx.Store.IssueJSON(repo, issue), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	if len(tail) == 2 && tail[1] == "comments" {
		if method == "GET" {
			items := make([]any, 0, len(issue.Comments))
			for _, comment := range issue.Comments {
				items = append(items, ctx.Store.CommentJSON(repo, issue, comment))
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "POST" {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			comment := ctx.Store.AddComment(repo, issue, restStringField(body, "body"))
			SendJSON(w, 201, ctx.Store.CommentJSON(repo, issue, comment), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	SendNotFound(w, "Not Found")
	return true
}

// restHandlePulls handles GET /pulls, POST /pulls, GET /pulls/{n},
// /pulls/{n}/files, /pulls/{n}/merge.
func restHandlePulls(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) bool {
	method := ctx.Method

	if len(tail) == 0 {
		if method == "GET" {
			state := restQueryParam(ctx, "state", "open")
			pulls := make([]*Pull, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				if state == "all" || pull.State == state {
					pulls = append(pulls, pull)
				}
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			items := make([]any, 0, len(pulls))
			for _, pull := range pulls {
				items = append(items, ctx.Store.PullJSON(repo, pull))
			}
			restSendList(ctx, w, ctx.Path, items)
			return true
		}
		if method == "POST" {
			body, ok := restBodyObject(ctx, w)
			if !ok {
				return true
			}
			title := strings.TrimSpace(restStringField(body, "title"))
			head := restStringField(body, "head")
			base := restStringField(body, "base")
			if title == "" || head == "" || base == "" {
				SendError(w, 422, "title, head and base are required")
				return true
			}
			headSha, headOK := repo.Refs["refs/heads/"+head]
			baseSha, baseOK := repo.Refs["refs/heads/"+base]
			if !headOK || !baseOK {
				SendError(w, 422, "head or base branch does not exist")
				return true
			}
			for _, pull := range repo.Pulls {
				if pull.HeadRef == head && pull.BaseRef == base && pull.State == "open" {
					SendError(w, 422, "A pull request already exists for these branches")
					return true
				}
			}
			pull := ctx.Store.CreatePull(repo, title, restStringField(body, "body"), head, headSha, base, baseSha)
			SendJSON(w, 201, ctx.Store.PullJSON(repo, pull), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	number, ok := restParseNumber(tail[0])
	if !ok {
		SendNotFound(w, "Not Found")
		return true
	}
	pull := ctx.Store.FindPull(repo, number)
	if pull == nil {
		SendNotFound(w, "Not Found")
		return true
	}

	if len(tail) == 1 {
		if method == "GET" {
			SendJSON(w, 200, ctx.Store.PullJSON(repo, pull), nil)
			return true
		}
		SendNotFound(w, "Not Found")
		return true
	}

	if len(tail) == 2 && tail[1] == "files" && method == "GET" {
		SendJSON(w, 200, ctx.Store.PullFiles(repo, pull), nil)
		return true
	}

	if len(tail) == 2 && tail[1] == "merge" && method == "PUT" {
		if pull.Merged {
			SendError(w, 405, "Pull request already merged")
			return true
		}
		if pull.State != "open" {
			SendError(w, 405, "Pull request is closed")
			return true
		}
		headSha, headOK := repo.Refs["refs/heads/"+pull.HeadRef]
		baseSha, baseOK := repo.Refs["refs/heads/"+pull.BaseRef]
		if !headOK || !baseOK {
			SendError(w, 422, "head or base branch does not exist")
			return true
		}
		flat, conflict := ctx.Store.ThreeWayMerge(repo, baseSha, headSha)
		if conflict {
			SendError(w, 405, "Pull request has conflicts")
			return true
		}
		// The squash commit: the merged tree on the base, one parent.
		treeOID := ctx.Store.BuildTreeFromFlat(repo, flat)
		mergeSha := ctx.Store.CreateCommit(repo, treeOID, []string{baseSha}, pull.Title, nil)
		repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
		pull.State = "closed"
		pull.Merged = true
		mergedAt := ctx.Store.IsoSec(ctx.Store.NowSec())
		pull.MergedAt = &mergedAt
		pull.MergeCommitSha = &mergeSha
		pull.BaseSha = mergeSha
		SendJSON(w, 200, map[string]any{
			"sha":     mergeSha,
			"merged":  true,
			"message": "Pull request successfully merged",
		}, nil)
		return true
	}

	SendNotFound(w, "Not Found")
	return true
}
