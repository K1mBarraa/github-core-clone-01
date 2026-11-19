// GraphQL v4: a small document parser and executor for the subset the suite
// sends (the documents gh and github-mcp-server emit). Port of graphql.ts
// (v3 reference). Behaviour-identical: same documents, fields, mutations,
// response shapes and error cases.
//
// Port checklist (TS -> Go):
//   tokenize -> tokenizeGraphQL
//   Parser (parseDocument/parseVariableDefinitions/parseType/
//     parseSelectionSet/parseSelection/parseDirectives/parseArguments/
//     parseValue) -> gqlParser methods
//   resolveValue -> resolveGqlValue; directivesAllow -> gqlDirectivesAllow
//   SCHEMA -> gqlSchemaFields; SEARCH_TYPE_VALUES -> gqlSearchTypeValues
//   viewerValue/repoValue/issueValue/pullValue -> gqlViewerValue etc.
//   cursorOf -> gqlCursorOf; pageInfoOf -> gqlPageInfoOf
//   issueConnection/pullConnection/commentConnection/labelConnection/
//     filesConnection -> gqlIssueConnection etc.
//   repoUrl -> gqlRepoURL; doSquashMerge -> gqlDoSquashMerge
//   RESOLVERS -> gqlResolvers; executeSelections -> gqlExecuteSelections
//   executeOperation -> gqlExecuteOperation; executeBody -> gqlExecuteBody;
//     executeOne -> gqlExecuteOne; handleGraphql -> HandleGraphql
//
// Concurrency: the server holds the store lock before calling HandleGraphql
// (Lock for GraphQL — documents include mutations). This file never locks.

package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------- tokenizer

type gqlTokenKind int

const (
	gqlTokName gqlTokenKind = iota
	gqlTokString
	gqlTokInt
	gqlTokFloat
	gqlTokPunct
	gqlTokSpread
)

type gqlToken struct {
	kind  gqlTokenKind
	value string
}

func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func gqlIsNameChar(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlIsDigit(c byte) bool { return c >= '0' && c <= '9' }

func tokenizeGraphQL(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(source) {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
			tokens = append(tokens, gqlToken{gqlTokSpread, "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@", c) >= 0 {
			tokens = append(tokens, gqlToken{gqlTokPunct, string(c)})
			i++
			continue
		}
		if c == '"' {
			var sb strings.Builder
			i++
			for i < len(source) && source[i] != '"' {
				ch := source[i]
				if ch == '\\' {
					if i+1 >= len(source) {
						break
					}
					esc := source[i+1]
					i += 2
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'u':
						hex := ""
						if i+4 <= len(source) {
							hex = source[i : i+4]
						}
						n, _ := strconv.ParseInt(hex, 16, 32)
						sb.WriteRune(rune(n))
						i += 4
					default:
						sb.WriteByte(esc)
					}
				} else {
					sb.WriteByte(ch)
					i++
				}
			}
			i++
			tokens = append(tokens, gqlToken{gqlTokString, sb.String()})
			continue
		}
		if c == '-' || gqlIsDigit(c) {
			j := i
			if j < len(source) && source[j] == '-' {
				j++
			}
			for j < len(source) && gqlIsDigit(source[j]) {
				j++
			}
			isFloat := false
			if j < len(source) && source[j] == '.' {
				isFloat = true
				j++
				for j < len(source) && gqlIsDigit(source[j]) {
					j++
				}
			}
			if j < len(source) && (source[j] == 'e' || source[j] == 'E') {
				isFloat = true
				j++
				if j < len(source) && (source[j] == '+' || source[j] == '-') {
					j++
				}
				for j < len(source) && gqlIsDigit(source[j]) {
					j++
				}
			}
			kind := gqlTokInt
			if isFloat {
				kind = gqlTokFloat
			}
			tokens = append(tokens, gqlToken{kind, source[i:j]})
			i = j
			continue
		}
		if gqlIsNameStart(c) {
			j := i
			for j < len(source) && gqlIsNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{gqlTokName, source[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %s", strconv.Quote(string(c)))
	}
	return tokens, nil
}

// ---------------------------------------------------------- parser

type gqlValueKind int

const (
	gqlValVariable gqlValueKind = iota
	gqlValString
	gqlValInt
	gqlValFloat
	gqlValBool
	gqlValNull
	gqlValEnum
	gqlValList
	gqlValObject
)

type gqlValueNode struct {
	kind   gqlValueKind
	str    string // string / enum / variable name
	num    float64
	b      bool
	list   []*gqlValueNode
	fields map[string]*gqlValueNode
}

type gqlDirective struct {
	name string
	args map[string]*gqlValueNode
}

type gqlSelKind int

const (
	gqlSelField gqlSelKind = iota
	gqlSelFragmentSpread
	gqlSelInlineFragment
)

type gqlSelection struct {
	kind             gqlSelKind
	name             string // field name, or fragment name for spreads
	alias            string
	hasAlias         bool
	args             map[string]*gqlValueNode
	directives       []gqlDirective
	selections       []*gqlSelection
	typeCondition    string
	hasTypeCondition bool
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValueNode // nil = no default
}

type gqlOperation struct {
	typ        string // "query" | "mutation"
	name       string
	hasName    bool
	variables  []gqlVarDef
	selections []*gqlSelection
}

type gqlFragment struct {
	typeCondition string
	selections    []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() (gqlToken, bool) {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos], true
	}
	return gqlToken{}, false
}

func (p *gqlParser) next() (gqlToken, error) {
	t, ok := p.peek()
	if !ok {
		return gqlToken{}, errors.New("unexpected end of document")
	}
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != gqlTokPunct || t.value != value {
		return fmt.Errorf("expected %s, got %s", value, t.value)
	}
	return nil
}

func (p *gqlParser) expectName(want ...string) (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != gqlTokName {
		return "", fmt.Errorf("expected a name, got %s", t.value)
	}
	if len(want) > 0 && t.value != want[0] {
		return "", fmt.Errorf("expected %s, got %s", want[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) isPunct(value string) bool {
	t, ok := p.peek()
	return ok && t.kind == gqlTokPunct && t.value == value
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*gqlFragment{}}
	for {
		t, ok := p.peek()
		if !ok {
			break
		}
		if t.kind == gqlTokName && t.value == "fragment" {
			p.pos++
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, err
			}
			typeCondition, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = &gqlFragment{typeCondition: typeCondition, selections: sels}
		} else if t.kind == gqlTokPunct && t.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, &gqlOperation{typ: "query", selections: sels})
		} else {
			typ, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if typ != "query" && typ != "mutation" {
				return nil, fmt.Errorf("unsupported operation %s", typ)
			}
			op := &gqlOperation{typ: typ}
			if nt, ok := p.peek(); ok && nt.kind == gqlTokName {
				op.name = nt.value
				op.hasName = true
				p.pos++
			}
			if p.isPunct("(") {
				vars, err := p.parseVariableDefinitions()
				if err != nil {
					return nil, err
				}
				op.variables = vars
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.selections = sels
			doc.operations = append(doc.operations, op)
		}
	}
	if len(doc.operations) == 0 {
		return nil, errors.New("document has no operations")
	}
	return doc, nil
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var out []gqlVarDef
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var def *gqlValueNode
		if p.isPunct("=") {
			p.pos++
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			def = v
		}
		out = append(out, gqlVarDef{name: name, defaultValue: def})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return out, nil
}

// parseType skips a type reference (named, list, non-null).
func (p *gqlParser) parseType() error {
	if p.isPunct("[") {
		p.pos++
		if err := p.parseType(); err != nil {
			return err
		}
		return p.expectPunct("]")
	}
	if _, err := p.expectName(); err != nil {
		return err
	}
	if p.isPunct("!") {
		p.pos++
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*gqlSelection
	for !p.isPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	t, ok := p.peek()
	if !ok {
		return nil, errors.New("unexpected end of document")
	}
	if t.kind == gqlTokSpread {
		p.pos++
		nt, ok := p.peek()
		if ok && nt.kind == gqlTokName && nt.value == "on" {
			p.pos++
			tc, err := p.expectName()
			if err != nil {
				return nil, err
			}
			dirs, err := p.parseDirectives()
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: gqlSelInlineFragment, typeCondition: tc, hasTypeCondition: true, directives: dirs, selections: sels}, nil
		}
		if ok && nt.kind == gqlTokName {
			name := nt.value
			p.pos++
			dirs, err := p.parseDirectives()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: gqlSelFragmentSpread, name: name, directives: dirs}, nil
		}
		dirs, err := p.parseDirectives()
		if err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: gqlSelInlineFragment, directives: dirs, selections: sels}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	name := first
	alias := ""
	hasAlias := false
	if p.isPunct(":") {
		p.pos++
		alias = first
		hasAlias = true
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args := map[string]*gqlValueNode{}
	if p.isPunct("(") {
		args, err = p.parseArguments()
		if err != nil {
			return nil, err
		}
	}
	dirs, err := p.parseDirectives()
	if err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	if p.isPunct("{") {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{kind: gqlSelField, name: name, alias: alias, hasAlias: hasAlias, args: args, directives: dirs, selections: sels}, nil
}

func (p *gqlParser) parseDirectives() ([]gqlDirective, error) {
	var out []gqlDirective
	for p.isPunct("@") {
		p.pos++
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		args := map[string]*gqlValueNode{}
		if p.isPunct("(") {
			args, err = p.parseArguments()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, gqlDirective{name: name, args: args})
	}
	return out, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValueNode, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	out := map[string]*gqlValueNode{}
	for !p.isPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseValue() (*gqlValueNode, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == gqlTokPunct && t.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValVariable, str: name}, nil
	}
	if t.kind == gqlTokString {
		return &gqlValueNode{kind: gqlValString, str: t.value}, nil
	}
	if t.kind == gqlTokInt || t.kind == gqlTokFloat {
		n, _ := strconv.ParseFloat(t.value, 64)
		kind := gqlValInt
		if t.kind == gqlTokFloat {
			kind = gqlValFloat
		}
		return &gqlValueNode{kind: kind, num: n}, nil
	}
	if t.kind == gqlTokName {
		switch t.value {
		case "true":
			return &gqlValueNode{kind: gqlValBool, b: true}, nil
		case "false":
			return &gqlValueNode{kind: gqlValBool, b: false}, nil
		case "null":
			return &gqlValueNode{kind: gqlValNull}, nil
		default:
			return &gqlValueNode{kind: gqlValEnum, str: t.value}, nil
		}
	}
	if t.kind == gqlTokPunct && t.value == "[" {
		var values []*gqlValueNode
		for {
			nt, ok := p.peek()
			if ok && nt.kind == gqlTokPunct && nt.value == "]" {
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValList, list: values}, nil
	}
	if t.kind == gqlTokPunct && t.value == "{" {
		fields := map[string]*gqlValueNode{}
		for {
			nt, ok := p.peek()
			if ok && nt.kind == gqlTokPunct && nt.value == "}" {
				break
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields[name] = v
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValObject, fields: fields}, nil
	}
	return nil, fmt.Errorf("unexpected value token %s", t.value)
}

// ---------------------------------------------------------- value resolution

// resolveGqlValue resolves a value node against the operation's variables.
func resolveGqlValue(node *gqlValueNode, vars map[string]any) any {
	switch node.kind {
	case gqlValVariable:
		if v, ok := vars[node.str]; ok && v != nil {
			return v
		}
		return nil
	case gqlValString, gqlValEnum:
		return node.str
	case gqlValInt, gqlValFloat:
		return node.num
	case gqlValBool:
		return node.b
	case gqlValNull:
		return nil
	case gqlValList:
		out := make([]any, 0, len(node.list))
		for _, v := range node.list {
			out = append(out, resolveGqlValue(v, vars))
		}
		return out
	case gqlValObject:
		out := make(map[string]any, len(node.fields))
		for k, v := range node.fields {
			out[k] = resolveGqlValue(v, vars)
		}
		return out
	}
	return nil
}

// gqlDirectivesAllow reports whether a selection survives @skip/@include.
func gqlDirectivesAllow(dirs []gqlDirective, vars map[string]any) bool {
	for _, d := range dirs {
		value := any(true)
		if raw, ok := d.args["if"]; ok {
			value = resolveGqlValue(raw, vars)
		}
		if d.name == "skip" {
			if b, ok := value.(bool); ok && b {
				return false
			}
		}
		if d.name == "include" {
			if b, ok := value.(bool); !ok || !b {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------- values and schema

// gqlValue is a value the executor can resolve fields against.
type gqlValue map[string]any

type gqlExecCtx struct {
	store  *Store
	errors []string
}

func (c *gqlExecCtx) addError(msg string) { c.errors = append(c.errors, msg) }

// gqlSchemaFields lists, in order, the fields each type answers.
var gqlSchemaFields = map[string][]string{
	"Query":                            {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "createdAt", "updatedAt", "description", "url", "isPrivate", "fork"},
	"Issue":                            {"id", "databaseId", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url", "resourcePath", "repository"},
	"PullRequest":                      {"id", "databaseId", "number", "title", "state", "body", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "createdAt", "updatedAt", "author", "files", "url", "resourcePath", "repository", "mergedAt", "mergeCommitSha"},
	"User":                             {"id", "login", "name", "email", "url"},
	"Ref":                              {"name", "target"},
	"Label":                            {"id", "name", "color", "description"},
	"LabelConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueCommentConnection":           {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"IssueComment":                     {"id", "databaseId", "body", "author", "createdAt", "updatedAt", "url"},
	"IssueCommentEdge":                 {"node", "cursor"},
	"LabelEdge":                        {"node", "cursor"},
	"IssueEdge":                        {"node", "cursor"},
	"PullRequestEdge":                  {"node", "cursor"},
	"PullRequestChangedFileEdge":       {"node", "cursor"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"__Type":                           {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                          {"name", "description", "type"},
	"__EnumValue":                      {"name", "description"},
	"__Schema":                         {"types", "queryType", "mutationType"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "clientMutationId"},
}

var gqlSearchTypeValues = []string{"CODE", "COMMIT", "DISCUSSION", "ISSUE", "LABEL", "MILESTONE", "PULL_REQUEST", "REPOSITORY", "USER"}

func gqlFieldAllowed(typ, field string) bool {
	for _, f := range gqlSchemaFields[typ] {
		if f == field {
			return true
		}
	}
	return false
}

func gqlTypename(v gqlValue) string {
	s, _ := v["__typename"].(string)
	return s
}

func gqlStore(v gqlValue) *Store {
	s, _ := v["store"].(*Store)
	return s
}

func gqlRepo(v gqlValue) *Repo {
	r, _ := v["repo"].(*Repo)
	return r
}

func gqlIssue(v gqlValue) *Issue {
	i, _ := v["issue"].(*Issue)
	return i
}

func gqlPull(v gqlValue) *Pull {
	p, _ := v["pr"].(*Pull)
	return p
}

func gqlComment(v gqlValue) (Comment, bool) {
	c, ok := v["comment"].(Comment)
	return c, ok
}

func gqlViewerValue() gqlValue {
	return gqlValue{"__typename": "User", "login": ViewerLogin, "id": ViewerID}
}

func gqlRepoValue(store *Store, repo *Repo) gqlValue {
	return gqlValue{"__typename": "Repository", "store": store, "repo": repo}
}

func gqlIssueValue(store *Store, repo *Repo, issue *Issue) gqlValue {
	return gqlValue{"__typename": "Issue", "store": store, "repo": repo, "issue": issue}
}

func gqlPullValue(store *Store, repo *Repo, pr *Pull) gqlValue {
	return gqlValue{"__typename": "PullRequest", "store": store, "repo": repo, "pr": pr}
}

func gqlCursorOf(kind string, number int) string {
	return B64Encode(kind + ":" + strconv.Itoa(number))
}

func gqlPageInfoOf(nodes []any, cursorOfNode func(any) string, hasMore bool) gqlValue {
	var start, end any
	if len(nodes) > 0 {
		start = cursorOfNode(nodes[0])
		end = cursorOfNode(nodes[len(nodes)-1])
	}
	return gqlValue{
		"__typename":      "PageInfo",
		"hasNextPage":     hasMore,
		"hasPreviousPage": false,
		"startCursor":     start,
		"endCursor":       end,
	}
}

// gqlAsFloat converts a JSON number (or int) argument to float64.
func gqlAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// gqlAsInt converts a JSON number argument to int.
func gqlAsInt(v any) (int, bool) {
	if f, ok := gqlAsFloat(v); ok {
		return int(f), true
	}
	return 0, false
}

// gqlParseCursorNumber parses a decoded "<kind>:<digits>" cursor.
func gqlParseCursorNumber(text, kind string) (int, bool) {
	prefix := kind + ":"
	if !strings.HasPrefix(text, prefix) {
		return 0, false
	}
	rest := text[len(prefix):]
	if rest == "" {
		return 0, false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// gqlFirstArg reads a Relay `first` argument: max(0, floor(n)), else total.
func gqlFirstArg(args map[string]any, total int) int {
	if f, ok := gqlAsFloat(args["first"]); ok {
		n := int(math.Floor(f))
		if n < 0 {
			n = 0
		}
		return n
	}
	return total
}

// gqlIssueConnection orders issues by an orderBy argument, then paginates
// by first/after.
func gqlIssueConnection(store *Store, repo *Repo, args map[string]any) gqlValue {
	issues := make([]*Issue, 0, len(repo.Issues))
	issues = append(issues, repo.Issues...)
	if rawStates, ok := args["states"].([]any); ok && len(rawStates) > 0 {
		allowed := make(map[string]bool, len(rawStates))
		for _, s := range rawStates {
			if str, ok := s.(string); ok {
				allowed[str] = true
			}
		}
		filtered := make([]*Issue, 0, len(issues))
		for _, issue := range issues {
			if allowed[strings.ToUpper(issue.State)] {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	field := ""
	direction := ""
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		field, _ = orderBy["field"].(string)
		direction, _ = orderBy["direction"].(string)
	}
	dir := -1
	if direction == "ASC" {
		dir = 1
	}
	keyOf := func(issue *Issue) int {
		switch field {
		case "UPDATED_AT":
			return issue.UpdatedSeq
		case "COMMENTS":
			return len(issue.Comments)
		default:
			return issue.CreatedSeq
		}
	}
	sort.SliceStable(issues, func(i, j int) bool {
		diff := keyOf(issues[i]) - keyOf(issues[j])
		if diff != 0 {
			return diff*dir < 0
		}
		return (issues[i].Number-issues[j].Number)*dir < 0
	})
	first := gqlFirstArg(args, len(issues))
	after, _ := args["after"].(string)
	start := 0
	if after != "" {
		text := ""
		if decoded, err := B64Decode(after); err == nil {
			text = string(decoded)
		}
		if num, ok := gqlParseCursorNumber(text, "I"); ok {
			for idx, issue := range issues {
				if issue.Number == num {
					start = idx + 1
					break
				}
			}
		}
	}
	end := start + first
	if end > len(issues) {
		end = len(issues)
	}
	page := issues[start:end]
	nodes := make([]any, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, gqlIssueValue(store, repo, issue))
	}
	return gqlValue{
		"__typename": "IssueConnection",
		"nodes":      nodes,
		"totalCount": len(issues),
		"pageInfo": gqlPageInfoOf(nodes, func(n any) string {
			return gqlCursorOf("I", gqlIssue(n.(gqlValue)).Number)
		}, start+first < len(issues)),
	}
}

func gqlPullConnection(store *Store, repo *Repo, args map[string]any) gqlValue {
	pulls := make([]*Pull, 0, len(repo.Pulls))
	pulls = append(pulls, repo.Pulls...)
	sort.SliceStable(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	first := gqlFirstArg(args, len(pulls))
	if first > len(pulls) {
		first = len(pulls)
	}
	page := pulls[:first]
	nodes := make([]any, 0, len(page))
	for _, pr := range page {
		nodes = append(nodes, gqlPullValue(store, repo, pr))
	}
	return gqlValue{
		"__typename": "PullRequestConnection",
		"nodes":      nodes,
		"totalCount": len(pulls),
		"pageInfo": gqlPageInfoOf(nodes, func(n any) string {
			return gqlCursorOf("P", gqlPull(n.(gqlValue)).Number)
		}, first < len(pulls)),
	}
}

func gqlCommentConnection(store *Store, repo *Repo, issue *Issue) gqlValue {
	nodes := make([]any, 0, len(issue.Comments))
	for _, comment := range issue.Comments {
		c := comment
		nodes = append(nodes, gqlValue{
			"__typename": "IssueComment",
			"store":      store,
			"repo":       repo,
			"issue":      issue,
			"comment":    c,
			"id":         store.CommentGlobalID(repo, issue, c),
		})
	}
	return gqlValue{
		"__typename": "IssueCommentConnection",
		"nodes":      nodes,
		"totalCount": len(nodes),
		"pageInfo": gqlPageInfoOf(nodes, func(n any) string {
			c, _ := gqlComment(n.(gqlValue))
			return gqlCursorOf("C", c.ID)
		}, false),
	}
}

func gqlLabelConnection(store *Store, repo *Repo, issue *Issue) gqlValue {
	nodes := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		label, ok := repo.Labels[name]
		if !ok {
			continue
		}
		nodes = append(nodes, gqlValue{
			"__typename":  "Label",
			"id":          B64Encode("L:" + repo.Owner + "/" + repo.Name + ":" + label.Name),
			"name":        label.Name,
			"color":       label.Color,
			"description": label.Description,
		})
	}
	return gqlValue{
		"__typename": "LabelConnection",
		"nodes":      nodes,
		"totalCount": len(nodes),
		"pageInfo": gqlPageInfoOf(nodes, func(n any) string {
			return B64Encode("L:" + n.(gqlValue)["name"].(string))
		}, false),
	}
}

func gqlFilesConnection(store *Store, repo *Repo, pr *Pull, args map[string]any) gqlValue {
	files := store.PullFiles(repo, pr)
	first := gqlFirstArg(args, len(files))
	if first > len(files) {
		first = len(files)
	}
	page := files[:first]
	nodes := make([]any, 0, len(page))
	for _, file := range page {
		nodes = append(nodes, gqlValue{
			"__typename": "PullRequestChangedFile",
			"path":       file.Filename,
			"additions":  file.Additions,
			"deletions":  file.Deletions,
			"changeType": strings.ToUpper(file.Status),
		})
	}
	return gqlValue{
		"__typename": "PullRequestChangedFileConnection",
		"nodes":      nodes,
		"totalCount": len(files),
		"pageInfo": gqlPageInfoOf(nodes, func(n any) string {
			return B64Encode("F:" + n.(gqlValue)["path"].(string))
		}, first < len(files)),
	}
}

// ---------------------------------------------------------- resolvers

type gqlResolver func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error)

func gqlRepoURL(repo *Repo) string {
	return "https://github.com/" + repo.Owner + "/" + repo.Name
}

func gqlDoSquashMerge(ectx *gqlExecCtx, repo *Repo, pr *Pull) bool {
	store := ectx.store
	headSha, okHead := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSha, okBase := repo.Refs["refs/heads/"+pr.BaseRef]
	if !okHead || !okBase {
		return false
	}
	merged, conflict := store.ThreeWayMerge(repo, baseSha, headSha)
	if conflict {
		return false
	}
	treeOID := store.BuildTreeFromFlat(repo, merged)
	mergeSha := store.CreateCommit(repo, treeOID, []string{baseSha}, pr.Title, nil)
	repo.Refs["refs/heads/"+pr.BaseRef] = mergeSha
	pr.State = "closed"
	pr.Merged = true
	mergedAt := store.IsoSec(store.NowSec())
	pr.MergedAt = &mergedAt
	sha := mergeSha
	pr.MergeCommitSha = &sha
	pr.BaseSha = mergeSha
	return true
}

// gqlInput extracts a mutation's `input` object argument ({} when absent).
func gqlInput(args map[string]any) map[string]any {
	if input, ok := args["input"].(map[string]any); ok {
		return input
	}
	return map[string]any{}
}

func gqlClientMutationID(input map[string]any) any {
	if s, ok := input["clientMutationId"].(string); ok {
		return s
	}
	return nil
}

var gqlResolvers = map[string]map[string]gqlResolver{
	"Query": {
		"viewer": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlViewerValue(), nil
		},
		"repository": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			owner, _ := args["owner"].(string)
			name, ok := args["name"].(string)
			if owner != ViewerLogin || !ok {
				return nil, nil
			}
			repo := store.GetRepo(owner, name)
			if repo == nil {
				return nil, nil
			}
			return gqlRepoValue(store, repo), nil
		},
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			id, ok := args["id"].(string)
			if !ok {
				return nil, nil
			}
			kind, repo, issue, pr := store.ResolveGlobalID(id)
			switch kind {
			case "":
				return nil, nil
			case "repo":
				return gqlRepoValue(store, repo), nil
			case "issue":
				return gqlIssueValue(store, repo, issue), nil
			default:
				return gqlPullValue(store, repo, pr), nil
			}
		},
		"__type": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			name, hasName := args["name"].(string)
			if hasName && name == "SearchType" {
				enumValues := make([]any, 0, len(gqlSearchTypeValues))
				for _, value := range gqlSearchTypeValues {
					enumValues = append(enumValues, gqlValue{"__typename": "__EnumValue", "name": value, "description": nil})
				}
				return gqlValue{"__typename": "__Type", "name": "SearchType", "kind": "ENUM", "fields": nil, "enumValues": enumValues}, nil
			}
			if hasName {
				if fields, ok := gqlSchemaFields[name]; ok {
					list := make([]any, 0, len(fields))
					for _, field := range fields {
						list = append(list, gqlValue{"__typename": "__Field", "name": field, "description": nil})
					}
					return gqlValue{"__typename": "__Type", "name": name, "kind": "OBJECT", "fields": list, "enumValues": nil}, nil
				}
			}
			var nameVal any
			if hasName {
				nameVal = name
			}
			return gqlValue{"__typename": "__Type", "name": nameVal, "kind": "OBJECT", "fields": []any{}, "enumValues": nil}, nil
		},
		"__schema": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlValue{
				"__typename":   "__Schema",
				"types":        []any{},
				"queryType":    gqlValue{"__typename": "__Type", "name": "Query"},
				"mutationType": gqlValue{"__typename": "__Type", "name": "Mutation"},
			}, nil
		},
	},
	"Mutation": {
		"createIssue": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			input := gqlInput(args)
			repoID, ok := input["repositoryId"].(string)
			var kind string
			var repo *Repo
			if ok {
				kind, repo, _, _ = store.ResolveGlobalID(repoID)
			}
			if kind != "repo" {
				return nil, errors.New("Repository not found")
			}
			title, _ := input["title"].(string)
			if title == "" {
				return nil, errors.New("title is required")
			}
			body, _ := input["body"].(string)
			issue := store.CreateIssue(repo, title, body, []string{})
			return gqlValue{"__typename": "CreateIssuePayload", "issue": gqlIssueValue(store, repo, issue), "clientMutationId": gqlClientMutationID(input)}, nil
		},
		"closeIssue": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			input := gqlInput(args)
			issueID, ok := input["issueId"].(string)
			var kind string
			var repo *Repo
			var issue *Issue
			if ok {
				kind, repo, issue, _ = store.ResolveGlobalID(issueID)
			}
			if kind != "issue" {
				return nil, errors.New("Issue not found")
			}
			issue.State = "closed"
			store.TouchIssue(repo, issue)
			return gqlValue{"__typename": "CloseIssuePayload", "issue": gqlIssueValue(store, repo, issue), "clientMutationId": gqlClientMutationID(input)}, nil
		},
		"createPullRequest": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			input := gqlInput(args)
			repoID, ok := input["repositoryId"].(string)
			var kind string
			var repo *Repo
			if ok {
				kind, repo, _, _ = store.ResolveGlobalID(repoID)
			}
			if kind != "repo" {
				return nil, errors.New("Repository not found")
			}
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			headSha, okHead := repo.Refs["refs/heads/"+head]
			baseSha, okBase := repo.Refs["refs/heads/"+base]
			if !okHead || !okBase {
				return nil, errors.New("head or base branch does not exist")
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			pr := store.CreatePull(repo, title, body, head, headSha, base, baseSha)
			return gqlValue{"__typename": "CreatePullRequestPayload", "pullRequest": gqlPullValue(store, repo, pr), "clientMutationId": gqlClientMutationID(input)}, nil
		},
		"mergePullRequest": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			input := gqlInput(args)
			prID, ok := input["pullRequestId"].(string)
			var kind string
			var repo *Repo
			var pr *Pull
			if ok {
				kind, repo, _, pr = store.ResolveGlobalID(prID)
			}
			if kind != "pull" {
				return nil, errors.New("Pull request not found")
			}
			if pr.Merged {
				return nil, errors.New("Pull request already merged")
			}
			if pr.State != "open" {
				return nil, errors.New("Pull request is closed")
			}
			if !gqlDoSquashMerge(ectx, repo, pr) {
				return nil, errors.New("Pull request has conflicts")
			}
			return gqlValue{"__typename": "MergePullRequestPayload", "pullRequest": gqlPullValue(store, repo, pr), "clientMutationId": gqlClientMutationID(input)}, nil
		},
		"addComment": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			input := gqlInput(args)
			subjectID, ok := input["subjectId"].(string)
			var kind string
			var repo *Repo
			var issue *Issue
			if ok {
				kind, repo, issue, _ = store.ResolveGlobalID(subjectID)
			}
			if kind != "issue" {
				return nil, errors.New("Subject not found")
			}
			body, _ := input["body"].(string)
			comment := store.AddComment(repo, issue, body)
			node := gqlValue{
				"__typename": "IssueComment",
				"store":      store,
				"repo":       repo,
				"issue":      issue,
				"comment":    comment,
				"id":         store.CommentGlobalID(repo, issue, comment),
			}
			return gqlValue{
				"__typename":       "AddCommentPayload",
				"commentEdge":      gqlValue{"__typename": "IssueCommentEdge", "node": node, "cursor": gqlCursorOf("C", comment.ID)},
				"clientMutationId": gqlClientMutationID(input),
			}, nil
		},
	},
	"Repository": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).RepoGlobalID(gqlRepo(parent)), nil
		},
		"databaseId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepo(parent).ID, nil
		},
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepo(parent).Name, nil
		},
		"nameWithOwner": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			repo := gqlRepo(parent)
			return repo.Owner + "/" + repo.Name, nil
		},
		"owner": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlViewerValue(), nil
		},
		"defaultBranchRef": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			repo := gqlRepo(parent)
			head, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				return nil, nil
			}
			return gqlValue{"__typename": "Ref", "name": repo.DefaultBranch, "target": head}, nil
		},
		"viewerPermission": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return "ADMIN", nil
		},
		"hasIssuesEnabled": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return true, nil
		},
		"issue": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			repo := gqlRepo(parent)
			number, ok := gqlAsInt(args["number"])
			if !ok {
				return nil, nil
			}
			issue := store.FindIssue(repo, number)
			if issue == nil {
				return nil, nil
			}
			return gqlIssueValue(store, repo, issue), nil
		},
		"issues": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlIssueConnection(gqlStore(parent), gqlRepo(parent), args), nil
		},
		"pullRequests": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPullConnection(gqlStore(parent), gqlRepo(parent), args), nil
		},
		"pullRequest": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			repo := gqlRepo(parent)
			number, ok := gqlAsInt(args["number"])
			if !ok {
				return nil, nil
			}
			pr := store.FindPull(repo, number)
			if pr == nil {
				return nil, nil
			}
			return gqlPullValue(store, repo, pr), nil
		},
		"createdAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlRepo(parent).CreatedSeq), nil
		},
		"updatedAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlRepo(parent).CreatedSeq), nil
		},
		"description": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepo(parent).Description, nil
		},
		"url": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepoURL(gqlRepo(parent)), nil
		},
		"isPrivate": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepo(parent).Private, nil
		},
		"fork": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return false, nil
		},
	},
	"Issue": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IssueGlobalID(gqlRepo(parent), gqlIssue(parent)), nil
		},
		"databaseId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlIssue(parent).ID, nil
		},
		"number": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlIssue(parent).Number, nil
		},
		"title": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlIssue(parent).Title, nil
		},
		"state": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return strings.ToUpper(gqlIssue(parent).State), nil
		},
		"body": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlIssue(parent).Body, nil
		},
		"createdAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlIssue(parent).CreatedSeq), nil
		},
		"updatedAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlIssue(parent).UpdatedSeq), nil
		},
		"author": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlViewerValue(), nil
		},
		"labels": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlLabelConnection(gqlStore(parent), gqlRepo(parent), gqlIssue(parent)), nil
		},
		"comments": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlCommentConnection(gqlStore(parent), gqlRepo(parent), gqlIssue(parent)), nil
		},
		"url": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepoURL(gqlRepo(parent)) + "/issues/" + strconv.Itoa(gqlIssue(parent).Number), nil
		},
		"resourcePath": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			repo := gqlRepo(parent)
			return "/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(gqlIssue(parent).Number), nil
		},
		"repository": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepoValue(gqlStore(parent), gqlRepo(parent)), nil
		},
	},
	"PullRequest": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).PullGlobalID(gqlRepo(parent), gqlPull(parent)), nil
		},
		"databaseId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).ID, nil
		},
		"number": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).Number, nil
		},
		"title": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).Title, nil
		},
		"state": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			pr := gqlPull(parent)
			if pr.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pr.State), nil
		},
		"body": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).Body, nil
		},
		"merged": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).Merged, nil
		},
		"mergeable": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			store := gqlStore(parent)
			if store.Mergeable(gqlRepo(parent), gqlPull(parent)) {
				return "MERGEABLE", nil
			}
			return "CONFLICTING", nil
		},
		"headRefName": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).HeadRef, nil
		},
		"baseRefName": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlPull(parent).BaseRef, nil
		},
		"headRef": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			pr := gqlPull(parent)
			return gqlValue{"__typename": "Ref", "name": pr.HeadRef, "target": pr.HeadSha}, nil
		},
		"baseRef": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			pr := gqlPull(parent)
			return gqlValue{"__typename": "Ref", "name": pr.BaseRef, "target": pr.BaseSha}, nil
		},
		"createdAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlPull(parent).CreatedSeq), nil
		},
		"updatedAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlStore(parent).IsoFor(gqlPull(parent).CreatedSeq), nil
		},
		"author": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlViewerValue(), nil
		},
		"files": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlFilesConnection(gqlStore(parent), gqlRepo(parent), gqlPull(parent), args), nil
		},
		"url": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepoURL(gqlRepo(parent)) + "/pull/" + strconv.Itoa(gqlPull(parent).Number), nil
		},
		"resourcePath": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			repo := gqlRepo(parent)
			return "/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(gqlPull(parent).Number), nil
		},
		"repository": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlRepoValue(gqlStore(parent), gqlRepo(parent)), nil
		},
		"mergedAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			if pr := gqlPull(parent); pr.MergedAt != nil {
				return *pr.MergedAt, nil
			}
			return nil, nil
		},
		"mergeCommitSha": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			if pr := gqlPull(parent); pr.MergeCommitSha != nil {
				return *pr.MergeCommitSha, nil
			}
			return nil, nil
		},
	},
	"User": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["id"], nil
		},
		"login": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			login, _ := parent["login"].(string)
			return login, nil
		},
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return "ada iversen", nil
		},
		"email": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return ViewerEmail, nil
		},
		"url": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			login, _ := parent["login"].(string)
			return "https://github.com/" + login, nil
		},
	},
	"Ref": {
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			name, _ := parent["name"].(string)
			return name, nil
		},
		"target": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["target"], nil
		},
	},
	"Label": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["id"], nil
		},
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			name, _ := parent["name"].(string)
			return name, nil
		},
		"color": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			color, _ := parent["color"].(string)
			return color, nil
		},
		"description": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			desc, _ := parent["description"].(string)
			return desc, nil
		},
	},
	"IssueConnection": {
		"nodes": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["nodes"], nil
		},
		"edges": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			nodes, _ := parent["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, gqlValue{"__typename": "IssueEdge", "node": n, "cursor": gqlCursorOf("I", gqlIssue(n.(gqlValue)).Number)})
			}
			return edges, nil
		},
		"pageInfo": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pageInfo"], nil
		},
		"totalCount": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["totalCount"], nil
		},
	},
	"PullRequestConnection": {
		"nodes": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["nodes"], nil
		},
		"edges": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			nodes, _ := parent["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, gqlValue{"__typename": "PullRequestEdge", "node": n, "cursor": gqlCursorOf("P", gqlPull(n.(gqlValue)).Number)})
			}
			return edges, nil
		},
		"pageInfo": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pageInfo"], nil
		},
		"totalCount": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["totalCount"], nil
		},
	},
	"PullRequestChangedFileConnection": {
		"nodes": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["nodes"], nil
		},
		"edges": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			nodes, _ := parent["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, gqlValue{"__typename": "PullRequestChangedFileEdge", "node": n, "cursor": B64Encode("F:" + n.(gqlValue)["path"].(string))})
			}
			return edges, nil
		},
		"pageInfo": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pageInfo"], nil
		},
		"totalCount": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["totalCount"], nil
		},
	},
	"IssueCommentConnection": {
		"nodes": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["nodes"], nil
		},
		"edges": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			nodes, _ := parent["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				c, _ := gqlComment(n.(gqlValue))
				edges = append(edges, gqlValue{"__typename": "IssueCommentEdge", "node": n, "cursor": gqlCursorOf("C", c.ID)})
			}
			return edges, nil
		},
		"pageInfo": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pageInfo"], nil
		},
		"totalCount": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["totalCount"], nil
		},
	},
	"LabelConnection": {
		"nodes": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["nodes"], nil
		},
		"edges": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			nodes, _ := parent["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, gqlValue{"__typename": "LabelEdge", "node": n, "cursor": B64Encode("L:" + n.(gqlValue)["name"].(string))})
			}
			return edges, nil
		},
		"pageInfo": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pageInfo"], nil
		},
		"totalCount": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["totalCount"], nil
		},
	},
	"PullRequestChangedFile": {
		"path": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			path, _ := parent["path"].(string)
			return path, nil
		},
		"additions": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["additions"], nil
		},
		"deletions": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["deletions"], nil
		},
		"changeType": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			changeType, _ := parent["changeType"].(string)
			return changeType, nil
		},
	},
	"IssueComment": {
		"id": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["id"], nil
		},
		"databaseId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			c, _ := gqlComment(parent)
			return c.ID, nil
		},
		"body": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			c, _ := gqlComment(parent)
			return c.Body, nil
		},
		"author": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlViewerValue(), nil
		},
		"createdAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			c, _ := gqlComment(parent)
			return gqlStore(parent).IsoFor(c.CreatedSeq), nil
		},
		"updatedAt": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			c, _ := gqlComment(parent)
			return gqlStore(parent).IsoFor(c.CreatedSeq), nil
		},
		"url": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			c, _ := gqlComment(parent)
			return gqlRepoURL(gqlRepo(parent)) + "/issues/" + strconv.Itoa(gqlIssue(parent).Number) + "#issuecomment-" + strconv.Itoa(c.ID), nil
		},
	},
	"IssueCommentEdge": {
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["node"], nil
		},
		"cursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["cursor"], nil
		},
	},
	"LabelEdge": {
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["node"], nil
		},
		"cursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["cursor"], nil
		},
	},
	"IssueEdge": {
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["node"], nil
		},
		"cursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["cursor"], nil
		},
	},
	"PullRequestEdge": {
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["node"], nil
		},
		"cursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["cursor"], nil
		},
	},
	"PullRequestChangedFileEdge": {
		"node": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["node"], nil
		},
		"cursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["cursor"], nil
		},
	},
	"PageInfo": {
		"hasNextPage": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["hasNextPage"], nil
		},
		"hasPreviousPage": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["hasPreviousPage"], nil
		},
		"startCursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["startCursor"], nil
		},
		"endCursor": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["endCursor"], nil
		},
	},
	"__Type": {
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["name"], nil
		},
		"kind": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			kind, _ := parent["kind"].(string)
			return kind, nil
		},
		"fields": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["fields"], nil
		},
		"enumValues": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["enumValues"], nil
		},
		"description": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return nil, nil
		},
	},
	"__Field": {
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			name, _ := parent["name"].(string)
			return name, nil
		},
		"description": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["description"], nil
		},
		"type": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return gqlValue{"__typename": "__Type", "name": "String", "kind": "SCALAR", "fields": nil, "enumValues": nil}, nil
		},
	},
	"__EnumValue": {
		"name": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			name, _ := parent["name"].(string)
			return name, nil
		},
		"description": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["description"], nil
		},
	},
	"__Schema": {
		"types": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["types"], nil
		},
		"queryType": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["queryType"], nil
		},
		"mutationType": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["mutationType"], nil
		},
	},
	"CreateIssuePayload": {
		"issue": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["issue"], nil
		},
		"clientMutationId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["clientMutationId"], nil
		},
	},
	"CloseIssuePayload": {
		"issue": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["issue"], nil
		},
		"clientMutationId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["clientMutationId"], nil
		},
	},
	"CreatePullRequestPayload": {
		"pullRequest": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pullRequest"], nil
		},
		"clientMutationId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["clientMutationId"], nil
		},
	},
	"MergePullRequestPayload": {
		"pullRequest": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["pullRequest"], nil
		},
		"clientMutationId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["clientMutationId"], nil
		},
	},
	"AddCommentPayload": {
		"commentEdge": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["commentEdge"], nil
		},
		"clientMutationId": func(parent gqlValue, args map[string]any, ectx *gqlExecCtx) (any, error) {
			return parent["clientMutationId"], nil
		},
	},
}

// ---------------------------------------------------------- executor

// gqlInvalidField reports a field the type does not answer.
func gqlInvalidField(ectx *gqlExecCtx, field, typename string) {
	ectx.addError(fmt.Sprintf("Cannot query field %q on type %q.", field, typename))
}

// gqlGoTypeName mirrors JS typeof for the subfield error message.
func gqlGoTypeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, float32, int, int64:
		return "number"
	case bool:
		return "boolean"
	default:
		return "object"
	}
}

// gqlExecuteSelections executes a selection set against a value, collecting
// field results.
func gqlExecuteSelections(parent gqlValue, sels []*gqlSelection, vars map[string]any, frags map[string]*gqlFragment, ectx *gqlExecCtx) map[string]any {
	out := map[string]any{}
	typename := gqlTypename(parent)
	for _, sel := range sels {
		if !gqlDirectivesAllow(sel.directives, vars) {
			continue
		}
		if sel.kind == gqlSelFragmentSpread {
			frag, ok := frags[sel.name]
			if !ok {
				ectx.addError(fmt.Sprintf("Unknown fragment %q.", sel.name))
				continue
			}
			if frag.typeCondition != typename {
				continue
			}
			for k, v := range gqlExecuteSelections(parent, frag.selections, vars, frags, ectx) {
				out[k] = v
			}
			continue
		}
		if sel.kind == gqlSelInlineFragment {
			if sel.hasTypeCondition && sel.typeCondition != typename {
				continue
			}
			for k, v := range gqlExecuteSelections(parent, sel.selections, vars, frags, ectx) {
				out[k] = v
			}
			continue
		}
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		if sel.name == "__typename" {
			out[key] = typename
			continue
		}
		if !gqlFieldAllowed(typename, sel.name) {
			gqlInvalidField(ectx, sel.name, typename)
			out[key] = nil
			continue
		}
		resolvers, ok := gqlResolvers[typename]
		if !ok {
			gqlInvalidField(ectx, sel.name, typename)
			out[key] = nil
			continue
		}
		resolver, ok := resolvers[sel.name]
		if !ok {
			gqlInvalidField(ectx, sel.name, typename)
			out[key] = nil
			continue
		}
		args := map[string]any{}
		for argName, argNode := range sel.args {
			args[argName] = resolveGqlValue(argNode, vars)
		}
		value, err := resolver(parent, args, ectx)
		if err != nil {
			ectx.addError(err.Error())
			out[key] = nil
			continue
		}
		if len(sel.selections) == 0 {
			out[key] = value
			continue
		}
		if value == nil {
			out[key] = nil
			continue
		}
		if arr, ok := value.([]any); ok {
			mapped := make([]any, 0, len(arr))
			for _, item := range arr {
				if item != nil {
					if gv, ok := item.(gqlValue); ok {
						mapped = append(mapped, gqlExecuteSelections(gv, sel.selections, vars, frags, ectx))
						continue
					}
				}
				mapped = append(mapped, nil)
			}
			out[key] = mapped
			continue
		}
		if gv, ok := value.(gqlValue); ok {
			out[key] = gqlExecuteSelections(gv, sel.selections, vars, frags, ectx)
			continue
		}
		ectx.addError(fmt.Sprintf("Field %q must not have a selection since type %q has no subfields.", sel.name, gqlGoTypeName(value)))
		out[key] = nil
	}
	return out
}

func gqlErrorResult(message string) map[string]any {
	return map[string]any{
		"data":   nil,
		"errors": []any{map[string]any{"message": message}},
	}
}

// gqlExecuteOperation executes one operation document against the store.
func gqlExecuteOperation(store *Store, doc *gqlDocument, operationName string, hasOperationName bool, requestVars map[string]any) map[string]any {
	var operation *gqlOperation
	if hasOperationName {
		for _, op := range doc.operations {
			if op.hasName && op.name == operationName {
				operation = op
				break
			}
		}
		if operation == nil {
			return gqlErrorResult(fmt.Sprintf("Unknown operation named %q.", operationName))
		}
	} else if len(doc.operations) > 1 {
		return gqlErrorResult("Must provide operation name if query contains multiple operations.")
	} else {
		operation = doc.operations[0]
	}
	if operation == nil {
		return gqlErrorResult("No operation found.")
	}
	vars := map[string]any{}
	for _, def := range operation.variables {
		if v, ok := requestVars[def.name]; ok {
			vars[def.name] = v
		} else if def.defaultValue != nil {
			vars[def.name] = resolveGqlValue(def.defaultValue, vars)
		} else {
			vars[def.name] = nil
		}
	}
	ectx := &gqlExecCtx{store: store}
	typename := "Query"
	if operation.typ == "mutation" {
		typename = "Mutation"
	}
	root := gqlValue{"__typename": typename, "store": store}
	data := gqlExecuteSelections(root, operation.selections, vars, doc.fragments, ectx)
	if len(ectx.errors) > 0 {
		errList := make([]any, 0, len(ectx.errors))
		for _, msg := range ectx.errors {
			errList = append(errList, map[string]any{"message": msg})
		}
		return map[string]any{"data": data, "errors": errList}
	}
	return map[string]any{"data": data}
}

// gqlExecuteBody executes one request body (a single operation, or a batch
// array).
func gqlExecuteBody(store *Store, body any) any {
	if arr, ok := body.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, gqlExecuteOne(store, item))
		}
		return out
	}
	return gqlExecuteOne(store, body)
}

func gqlExecuteOne(store *Store, body any) any {
	if body == nil {
		return gqlErrorResult("Request body must be a JSON object with a query string.")
	}
	record, ok := body.(map[string]any)
	if !ok {
		return gqlErrorResult("Request body must be a JSON object with a query string.")
	}
	query, ok := record["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return gqlErrorResult("Must provide query string.")
	}
	tokens, err := tokenizeGraphQL(query)
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	doc, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	operationName, hasOperationName := record["operationName"].(string)
	variables := map[string]any{}
	if vars, ok := record["variables"].(map[string]any); ok {
		variables = vars
	}
	return gqlExecuteOperation(store, doc, operationName, hasOperationName, variables)
}

// ---------------------------------------------------------- handler

// HandleGraphql handles POST /api/graphql (and a GET with ?query=, for
// completeness). It returns false when the path is not its own.
func HandleGraphql(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Path != "/api/graphql" {
		return false
	}
	if !Authorized(ctx) {
		SendUnauthorized(w)
		return true
	}
	if ctx.Method == "GET" {
		values, ok := ctx.Query["query"]
		if !ok {
			SendError(w, 400, "Must provide query string.")
			return true
		}
		result := gqlExecuteBody(ctx.Store, map[string]any{"query": values[0], "variables": map[string]any{}})
		SendJSON(w, 200, result, nil)
		return true
	}
	if ctx.Method != "POST" {
		SendNotFound(w, "Not Found")
		return true
	}
	body, ok := JSONBody(ctx)
	if !ok {
		SendError(w, 400, "Invalid JSON body")
		return true
	}
	result := gqlExecuteBody(ctx.Store, body)
	SendJSON(w, 200, result, nil)
	return true
}
