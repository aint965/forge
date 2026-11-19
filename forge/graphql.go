// GraphQL v4: a small executor for the schema surface gh and
// github-mcp-server use — viewer, repository, issues, pull requests, node,
// __type introspection, the five mutations, and operation batching. A
// GraphQL-level failure (an unknown field, a bad argument) is a 200 carrying
// an errors array, never an HTTP error.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer

type gqlTokenKind int

const (
	tokEOF gqlTokenKind = iota
	tokName
	tokInt
	tokFloat
	tokString
	tokLBrace
	tokRBrace
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokColon
	tokDollar
	tokBang
	tokEquals
	tokAt
	tokSpread
)

type gqlToken struct {
	kind gqlTokenKind
	text string
}

type gqlLexer struct {
	rest string
}

func (l *gqlLexer) next() (gqlToken, error) {
	l.skipTrivia()
	if l.rest == "" {
		return gqlToken{kind: tokEOF}, nil
	}
	first := l.rest[0]
	if kind := gqlPunctuation[first]; kind != tokEOF {
		return l.emit(kind, 1), nil
	}
	switch {
	case strings.HasPrefix(l.rest, "..."):
		return l.emit(tokSpread, 3), nil
	case first == '"':
		return l.scanString()
	case first == '-' || gqlDigit(first):
		return l.scanNumber(), nil
	case gqlNameStart(first):
		end := 1
		for end < len(l.rest) && (gqlNameStart(l.rest[end]) || gqlDigit(l.rest[end])) {
			end++
		}
		return l.emit(tokName, end), nil
	default:
		return gqlToken{}, fmt.Errorf("unexpected character %q in document", first)
	}
}

var gqlPunctuation = [256]gqlTokenKind{
	'{': tokLBrace, '}': tokRBrace,
	'(': tokLParen, ')': tokRParen,
	'[': tokLBracket, ']': tokRBracket,
	':': tokColon, '$': tokDollar, '!': tokBang, '=': tokEquals, '@': tokAt,
}

func gqlNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func gqlDigit(c byte) bool     { return c >= '0' && c <= '9' }

func (l *gqlLexer) emit(kind gqlTokenKind, size int) gqlToken {
	token := gqlToken{kind: kind, text: l.rest[:size]}
	l.rest = l.rest[size:]
	return token
}

func (l *gqlLexer) skipTrivia() {
	for {
		l.rest = strings.TrimLeft(l.rest, " \t\r\n,")
		if !strings.HasPrefix(l.rest, "#") {
			return
		}
		_, l.rest, _ = strings.Cut(l.rest, "\n")
	}
}

func (l *gqlLexer) scanString() (gqlToken, error) {
	input := l.rest[1:]
	var decoded strings.Builder
	for start := 0; start < len(input); {
		width := strings.IndexAny(input[start:], "\"\\\n")
		if width < 0 {
			break
		}
		end := start + width
		switch input[end] {
		case '"':
			l.rest = input[end+1:]
			if start == 0 {
				return gqlToken{kind: tokString, text: input[:end]}, nil
			}
			decoded.WriteString(input[start:end])
			return gqlToken{kind: tokString, text: decoded.String()}, nil
		case '\n':
			return gqlToken{}, fmt.Errorf("unterminated string")
		case '\\':
			decoded.WriteString(input[start:end])
			start = end + 1 + appendGQLEscape(&decoded, input[end+1:])
		}
	}
	return gqlToken{}, fmt.Errorf("unterminated string")
}

// The forge has historically accepted unknown escapes and ignored a malformed
// Unicode escape marker. Keep that decoding behavior for existing queries.
func appendGQLEscape(text *strings.Builder, escape string) int {
	if escape == "" {
		return 0
	}
	if escape[0] == 'u' {
		if len(escape) >= 5 {
			if code, err := strconv.ParseUint(escape[1:5], 16, 32); err == nil {
				text.WriteRune(rune(code))
				return 5
			}
		}
		return 1
	}
	const spellings = "bfnrt"
	const replacements = "\b\f\n\r\t"
	if i := strings.IndexByte(spellings, escape[0]); i >= 0 {
		text.WriteByte(replacements[i])
	} else {
		text.WriteByte(escape[0])
	}
	return 1
}

func (l *gqlLexer) scanNumber() gqlToken {
	input := l.rest
	end := 0
	if input[0] == '-' {
		end++
	}
	end = gqlDigitEnd(input, end)
	kind := tokInt
	if end < len(input) && input[end] == '.' {
		kind = tokFloat
		end = gqlDigitEnd(input, end+1)
	}
	if end < len(input) && (input[end] == 'e' || input[end] == 'E') {
		kind = tokFloat
		end++
		if end < len(input) && (input[end] == '+' || input[end] == '-') {
			end++
		}
		end = gqlDigitEnd(input, end)
	}
	return l.emit(kind, end)
}

func gqlDigitEnd(text string, start int) int {
	for start < len(text) && gqlDigit(text[start]) {
		start++
	}
	return start
}

// ---------------------------------------------------------------------------
// Parser

type gqlValue struct {
	variable string
	literal  any
	list     []gqlValue
	object   []gqlArg
}

type gqlArg struct {
	name  string
	value gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  []gqlArg
	sel   *gqlSelectionSet
}

type gqlSelection struct {
	field    *gqlField
	fragment *gqlInlineFragment
	spread   string
}

type gqlInlineFragment struct {
	typeCond string
	sel      *gqlSelectionSet
}

type gqlSelectionSet struct {
	sels []gqlSelection
}

type gqlOperation struct {
	opType string
	name   string
	sel    *gqlSelectionSet
}

type gqlFragmentDef struct {
	name     string
	typeCond string
	sel      *gqlSelectionSet
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragmentDef
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func parseGraphQLDocument(src string) (*gqlDocument, error) {
	tokens, err := tokenizeGraphQL(src)
	if err != nil {
		return nil, err
	}
	p := gqlParser{tokens: tokens}
	doc := &gqlDocument{fragments: make(map[string]*gqlFragmentDef)}
	for p.peek().kind != tokEOF {
		if p.matchName("fragment") {
			definition, err := p.parseFragment()
			if err != nil {
				return nil, err
			}
			doc.fragments[definition.name] = definition
		} else {
			operation, err := p.parseOperation()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, operation)
		}
	}
	if len(doc.operations) == 0 {
		return nil, fmt.Errorf("the document holds no operation")
	}
	return doc, nil
}

// Tokenize up front so a lexical error still takes precedence over a syntax
// error earlier in the document.
func tokenizeGraphQL(src string) ([]gqlToken, error) {
	lexer := gqlLexer{rest: src}
	tokens := make([]gqlToken, 0, min(64, len(src)/3+1))
	for {
		t, err := lexer.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
		if t.kind == tokEOF {
			return tokens, nil
		}
	}
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }

func (p *gqlParser) take() gqlToken {
	t := p.peek()
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *gqlParser) match(kind gqlTokenKind) bool {
	if p.peek().kind != kind {
		return false
	}
	p.take()
	return true
}

func (p *gqlParser) matchName(name string) bool {
	if t := p.peek(); t.kind == tokName && t.text == name {
		p.take()
		return true
	}
	return false
}

func (p *gqlParser) expect(kind gqlTokenKind) (gqlToken, error) {
	t := p.take()
	if t.kind != kind {
		return t, fmt.Errorf("expected %v, got %q", kind, t.text)
	}
	return t, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	operation := gqlOperation{opType: "query"}
	if first := p.peek(); first.kind == tokName {
		switch first.text {
		case "query", "mutation", "subscription":
			operation.opType = p.take().text
			if next := p.peek(); next.kind == tokName {
				operation.name = p.take().text
			}
		}
	}
	if p.match(tokLParen) {
		if err := p.skipVariables(); err != nil {
			return nil, err
		}
	}
	sel, err := p.directedSelectionSet(false)
	if err != nil {
		return nil, err
	}
	operation.sel = sel
	return &operation, nil
}

// Definitions are validated syntactically, but this schema takes variable
// values only from the operation envelope.
func (p *gqlParser) skipVariables() error {
	for !p.match(tokRParen) {
		if _, err := p.expect(tokDollar); err != nil {
			return err
		}
		if _, err := p.expect(tokName); err != nil {
			return err
		}
		if _, err := p.expect(tokColon); err != nil {
			return err
		}
		if err := p.parseType(); err != nil {
			return err
		}
		if p.match(tokEquals) {
			if _, err := p.parseValue(); err != nil {
				return err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gqlParser) parseType() error {
	// A type consists of one name wrapped by zero or more list modifiers.
	depth := 0
	for p.match(tokLBracket) {
		depth++
	}
	if !p.match(tokName) {
		return fmt.Errorf("expected a type, got %q", p.peek().text)
	}
	p.match(tokBang)
	for range depth {
		if _, err := p.expect(tokRBracket); err != nil {
			return err
		}
		p.match(tokBang)
	}
	return nil
}

func (p *gqlParser) skipDirectives() error {
	for p.match(tokAt) {
		if _, err := p.expect(tokName); err != nil {
			return err
		}
		if p.match(tokLParen) {
			if _, err := p.parseNamedValues(tokRParen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) directedSelectionSet(optional bool) (*gqlSelectionSet, error) {
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if optional && p.peek().kind != tokLBrace {
		return nil, nil
	}
	return p.parseSelectionSet()
}

func (p *gqlParser) parseSelectionSet() (*gqlSelectionSet, error) {
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	var selections []gqlSelection
	for !p.match(tokRBrace) {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	return &gqlSelectionSet{sels: selections}, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.match(tokSpread) {
		return p.parseSpread()
	}
	nameTok, err := p.expect(tokName)
	if err != nil {
		return gqlSelection{}, err
	}
	f := &gqlField{name: nameTok.text}
	if p.match(tokColon) {
		fieldTok, err := p.expect(tokName)
		if err != nil {
			return gqlSelection{}, err
		}
		f.alias = f.name
		f.name = fieldTok.text
	}
	if p.match(tokLParen) {
		args, err := p.parseNamedValues(tokRParen)
		if err != nil {
			return gqlSelection{}, err
		}
		f.args = args
	}
	f.sel, err = p.directedSelectionSet(true)
	if err != nil {
		return gqlSelection{}, err
	}
	return gqlSelection{field: f}, nil
}

func (p *gqlParser) parseSpread() (gqlSelection, error) {
	var typeCondition string
	if p.matchName("on") {
		t, err := p.expect(tokName)
		if err != nil {
			return gqlSelection{}, err
		}
		typeCondition = t.text
	} else if p.peek().kind == tokName {
		name := p.take().text
		return gqlSelection{spread: name}, p.skipDirectives()
	}
	selection, err := p.directedSelectionSet(false)
	return gqlSelection{fragment: &gqlInlineFragment{typeCond: typeCondition, sel: selection}}, err
}

// Arguments and input objects have the same name/value grammar. Retain their
// source order so duplicate names keep the last value during evaluation.
func (p *gqlParser) parseNamedValues(close gqlTokenKind) ([]gqlArg, error) {
	pairs := make([]gqlArg, 0)
	for !p.match(close) {
		name, err := p.expect(tokName)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokColon); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, gqlArg{name: name.text, value: value})
	}
	return pairs, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.take()
	switch t.kind {
	case tokDollar:
		name, err := p.expect(tokName)
		return gqlValue{variable: name.text}, err
	case tokInt:
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			n = 0
		}
		return gqlValue{literal: int(n)}, nil
	case tokFloat:
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			n = 0
		}
		return gqlValue{literal: n}, nil
	case tokString:
		return gqlValue{literal: t.text}, nil
	case tokLBracket:
		list, err := p.parseList()
		return gqlValue{list: list}, err
	case tokLBrace:
		fields, err := p.parseNamedValues(tokRBrace)
		return gqlValue{object: fields}, err
	case tokName:
		switch t.text {
		case "true":
			return gqlValue{literal: true}, nil
		case "false":
			return gqlValue{literal: false}, nil
		case "null":
			return gqlValue{}, nil
		default:
			return gqlValue{literal: t.text}, nil
		}
	}
	return gqlValue{}, fmt.Errorf("unexpected value token %q", t.text)
}

func (p *gqlParser) parseList() ([]gqlValue, error) {
	values := make([]gqlValue, 0)
	for !p.match(tokRBracket) {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (p *gqlParser) parseFragment() (*gqlFragmentDef, error) {
	name, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	if !p.matchName("on") {
		return nil, fmt.Errorf("expected 'on' in a fragment definition")
	}
	typeTok, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	sel, err := p.directedSelectionSet(false)
	if err != nil {
		return nil, err
	}
	return &gqlFragmentDef{name: name.text, typeCond: typeTok.text, sel: sel}, nil
}

// ---------------------------------------------------------------------------
// Values and global ids

func (v gqlValue) evaluate(vars map[string]any) any {
	switch {
	case v.variable != "":
		return vars[v.variable]
	case v.list != nil:
		values := make([]any, len(v.list))
		for i, element := range v.list {
			values[i] = element.evaluate(vars)
		}
		return values
	case v.object != nil:
		return evaluateGQLPairs(v.object, vars)
	default:
		return v.literal
	}
}

func evaluateGQLPairs(pairs []gqlArg, vars map[string]any) map[string]any {
	values := make(map[string]any, len(pairs))
	for _, pair := range pairs {
		values[pair.name] = pair.value.evaluate(vars)
	}
	return values
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}

func toString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func toMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// Global ids: base64 of a typed key, opaque and stable across reads.
func globalID(parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}

func repoGlobalID(r *repository) string { return globalID("R", strconv.Itoa(r.id)) }

func issueGlobalID(r *repository, it *issue) string {
	return globalID("I", strconv.Itoa(r.id), strconv.Itoa(it.number))
}

func pullGlobalID(r *repository, pr *pullRequest) string {
	return globalID("P", strconv.Itoa(r.id), strconv.Itoa(pr.number))
}

func commentGlobalID(r *repository, it *issue, c *comment) string {
	return globalID("C", strconv.Itoa(r.id), strconv.Itoa(it.number), strconv.Itoa(c.id))
}

// decodeGlobalID splits a global id back into its parts.
func decodeGlobalID(id string) ([]string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil, false
	}
	return parts, true
}

// ---------------------------------------------------------------------------
// Executor

// gqlNode is a resolved object: its GraphQL type and its store value.
type gqlNode struct {
	typ string
	val any
}

type gqlError struct {
	Message string `json:"message"`
}

type gqlExecutor struct {
	srv  *server
	vars map[string]any
	doc  *gqlDocument
	errs []gqlError
}

type gqlFieldDef struct {
	resolve func(e *gqlExecutor, obj any, args map[string]any) (any, error)
}

// fieldTables maps each object type to its executable fields.
var fieldTables map[string]map[string]gqlFieldDef

func init() {
	fieldTables = map[string]map[string]gqlFieldDef{
		"Query": {
			"viewer":     {resolve: resolveViewer},
			"repository": {resolve: resolveRepository},
			"node":       {resolve: resolveNode},
			"__type":     {resolve: resolveType},
		},
		"Mutation": {
			"createIssue":       {resolve: resolveCreateIssue},
			"closeIssue":        {resolve: resolveCloseIssue},
			"createPullRequest": {resolve: resolveCreatePullRequest},
			"mergePullRequest":  {resolve: resolveMergePullRequest},
			"addComment":        {resolve: resolveAddComment},
		},
		"User": {
			"login": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return e.srv.st.viewerLogin, nil }},
		},
		"Repository": {
			"id": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return repoGlobalID(obj.(*repository)), nil
			}},
			"databaseId": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*repository).id, nil }},
			"name":       {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*repository).name, nil }},
			"nameWithOwner": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return obj.(*repository).fullName(), nil
			}},
			"owner":            {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return &gqlNode{typ: "User"}, nil }},
			"defaultBranchRef": {resolve: resolveDefaultBranchRef},
			"viewerPermission": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return "ADMIN", nil }},
			"hasIssuesEnabled": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return true, nil }},
			"issue":            {resolve: resolveIssueField},
			"issues":           {resolve: resolveIssuesField},
			"pullRequests":     {resolve: resolvePullRequestsField},
			"pullRequest":      {resolve: resolvePullRequestField},
		},
		"Ref": {
			"name": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(string), nil }},
		},
		"Issue": {
			"id": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				p := obj.(*issueRef)
				return issueGlobalID(p.repo, p.issue), nil
			}},
			"number": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return obj.(*issueRef).issue.number, nil
			}},
			"title": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return obj.(*issueRef).issue.title, nil
			}},
			"state": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				if obj.(*issueRef).issue.state == "closed" {
					return "CLOSED", nil
				}
				return "OPEN", nil
			}},
			"body": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return obj.(*issueRef).issue.body, nil
			}},
		},
		"IssueConnection": {
			"nodes": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*issueConn).nodes, nil }},
			"pageInfo": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return &gqlNode{typ: "PageInfo", val: obj.(*issueConn).page}, nil
			}},
		},
		"PageInfo": {
			"hasNextPage": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pageInfo).hasNext, nil }},
			"endCursor":   {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pageInfo).endCursor, nil }},
		},
		"PullRequest": {
			"id": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				p := obj.(*pullRef)
				return pullGlobalID(p.repo, p.pr), nil
			}},
			"number":      {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pullRef).pr.number, nil }},
			"state":       {resolve: resolvePullState},
			"merged":      {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pullRef).pr.merged, nil }},
			"headRefName": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pullRef).pr.headRef, nil }},
			"baseRefName": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*pullRef).pr.baseRef, nil }},
			"mergeable":   {resolve: resolvePullMergeable},
			"files":       {resolve: resolvePullFiles},
		},
		"PullRequestConnection": {
			"nodes": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.([]*gqlNode), nil }},
		},
		"PullRequestChangedFileConnection": {
			"nodes": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.([]*gqlNode), nil }},
		},
		"PullRequestChangedFile": {
			"path": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(string), nil }},
		},
		"CreateIssuePayload": {
			"issue": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"CloseIssuePayload": {
			"issue": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"CreatePullRequestPayload": {
			"pullRequest": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"MergePullRequestPayload": {
			"pullRequest": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"AddCommentPayload": {
			"commentEdge": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"AddedCommentEdge": {
			"node": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*gqlNode), nil }},
		},
		"IssueComment": {
			"id": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				p := obj.(*commentRef)
				return commentGlobalID(p.repo, p.issue, p.comment), nil
			}},
			"body": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) {
				return obj.(*commentRef).comment.body, nil
			}},
		},
		"__Type": {
			"name":       {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*typeInfo).name, nil }},
			"kind":       {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(*typeInfo).kind, nil }},
			"fields":     {resolve: resolveTypeFields},
			"enumValues": {resolve: resolveTypeEnumValues},
		},
		"__Field": {
			"name": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(string), nil }},
		},
		"__EnumValue": {
			"name": {resolve: func(e *gqlExecutor, obj any, args map[string]any) (any, error) { return obj.(string), nil }},
		},
	}
}

// issueRef, pullRef and commentRef carry a store object with its repository.
type issueRef struct {
	repo  *repository
	issue *issue
}

type pullRef struct {
	repo *repository
	pr   *pullRequest
}

type commentRef struct {
	repo    *repository
	issue   *issue
	comment *comment
}

type pageInfo struct {
	hasNext   bool
	endCursor any // string or nil
}

type issueConn struct {
	nodes []*gqlNode
	page  *pageInfo
}

type typeInfo struct {
	name   string
	kind   string // "OBJECT" or "ENUM"
	fields []string
	enum   []string
}

// typeCatalog is the introspection data: the fields and enum values each named
// type exposes.
var typeCatalog = map[string]*typeInfo{
	"Issue":                  {name: "Issue", kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "comments", "labels", "url", "repository", "closed", "closedAt", "stateReason", "locked", "milestone", "assignees", "participants"}},
	"PullRequest":            {name: "PullRequest", kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "mergeCommit", "files", "createdAt", "updatedAt", "author", "url", "repository", "isDraft", "additions", "deletions", "changedFiles", "commits", "reviews", "reviewRequests"}},
	"Repository":             {name: "Repository", kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "description", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "isPrivate", "url", "createdAt", "updatedAt", "issue", "issues", "pullRequest", "pullRequests", "ref", "refs", "forkCount", "stargazerCount", "watchers", "diskUsage", "isArchived", "isFork"}},
	"SearchType":             {name: "SearchType", kind: "ENUM", enum: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}},
	"User":                   {name: "User", kind: "OBJECT", fields: []string{"login", "id", "name", "email", "url", "avatarUrl"}},
	"IssueState":             {name: "IssueState", kind: "ENUM", enum: []string{"OPEN", "CLOSED"}},
	"OrderDirection":         {name: "OrderDirection", kind: "ENUM", enum: []string{"ASC", "DESC"}},
	"IssueOrderField":        {name: "IssueOrderField", kind: "ENUM", enum: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"MergeableState":         {name: "MergeableState", kind: "ENUM", enum: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"PullRequestState":       {name: "PullRequestState", kind: "ENUM", enum: []string{"OPEN", "CLOSED", "MERGED"}},
	"PullRequestMergeMethod": {name: "PullRequestMergeMethod", kind: "ENUM", enum: []string{"MERGE", "SQUASH", "REBASE"}},
	"RepositoryPermission":   {name: "RepositoryPermission", kind: "ENUM", enum: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
}

// ---------------------------------------------------------------------------
// Resolvers

func resolveViewer(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	return &gqlNode{typ: "User"}, nil
}

func resolveRepository(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	owner, _ := toString(args["owner"])
	name, _ := toString(args["name"])
	r := e.srv.st.repo(owner, name)
	if r == nil {
		return nil, nil
	}
	return &gqlNode{typ: "Repository", val: r}, nil
}

func resolveNode(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	id, _ := toString(args["id"])
	parts, ok := decodeGlobalID(id)
	if !ok {
		return nil, nil
	}
	switch parts[0] {
	case "R":
		if len(parts) == 2 {
			if r := e.repositoryByNumber(parts[1]); r != nil {
				return &gqlNode{typ: "Repository", val: r}, nil
			}
		}
	case "I":
		if ref := e.lookupIssue(parts); ref != nil {
			return &gqlNode{typ: "Issue", val: ref}, nil
		}
	case "P":
		if ref := e.lookupPull(parts); ref != nil {
			return &gqlNode{typ: "PullRequest", val: ref}, nil
		}
	}
	return nil, nil
}

// numberedNode decodes the common repository/number suffix of issue and pull
// IDs. The callers retain control of which node kinds they accept.
func (e *gqlExecutor) numberedNode(parts []string) (*repository, int) {
	if len(parts) != 3 {
		return nil, 0
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, 0
	}
	return e.repositoryByNumber(parts[1]), number
}

func (e *gqlExecutor) lookupIssue(parts []string) *issueRef {
	if r, n := e.numberedNode(parts); r != nil {
		if it := r.issuesByNum[n]; it != nil {
			return &issueRef{repo: r, issue: it}
		}
	}
	return nil
}

func (e *gqlExecutor) lookupPull(parts []string) *pullRef {
	if r, n := e.numberedNode(parts); r != nil {
		if pr := r.pullsByNum[n]; pr != nil {
			return &pullRef{repo: r, pr: pr}
		}
	}
	return nil
}

func (e *gqlExecutor) lookupRepoByID(id string) *repository {
	parts, ok := decodeGlobalID(id)
	if !ok || len(parts) != 2 || parts[0] != "R" {
		return nil
	}
	return e.repositoryByNumber(parts[1])
}

func (e *gqlExecutor) repositoryByNumber(text string) *repository {
	dbid, err := strconv.Atoi(text)
	if err != nil {
		return nil
	}
	return e.srv.st.reposByID[dbid]
}

func resolveType(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	name, _ := toString(args["name"])
	info := typeCatalog[name]
	if info == nil {
		return nil, nil
	}
	return &gqlNode{typ: "__Type", val: info}, nil
}

func resolveTypeFields(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	info := obj.(*typeInfo)
	nodes := make([]*gqlNode, 0, len(info.fields))
	for _, name := range info.fields {
		nodes = append(nodes, &gqlNode{typ: "__Field", val: name})
	}
	return nodes, nil
}

func resolveTypeEnumValues(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	info := obj.(*typeInfo)
	nodes := make([]*gqlNode, 0, len(info.enum))
	for _, name := range info.enum {
		nodes = append(nodes, &gqlNode{typ: "__EnumValue", val: name})
	}
	return nodes, nil
}

func resolveDefaultBranchRef(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	if _, ok := r.refs["refs/heads/"+r.defaultBranch]; !ok {
		return nil, nil
	}
	return &gqlNode{typ: "Ref", val: r.defaultBranch}, nil
}

func resolveIssueField(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	number, ok := toInt(args["number"])
	if !ok {
		return nil, nil
	}
	if it := r.issuesByNum[number]; it != nil {
		return issueNode(r, it), nil
	}
	return nil, nil
}

func issueNode(r *repository, it *issue) *gqlNode {
	return &gqlNode{typ: "Issue", val: &issueRef{repo: r, issue: it}}
}

type gqlIssueStates uint8

const (
	gqlOpen gqlIssueStates = 1 << iota
	gqlClosed
)

func gqlReadIssueStates(value any) gqlIssueStates {
	if value == nil {
		return gqlOpen | gqlClosed
	}
	var states gqlIssueStates
	list, _ := value.([]any)
	for _, item := range list {
		switch item {
		case "OPEN":
			states |= gqlOpen
		case "CLOSED":
			states |= gqlClosed
		}
	}
	return states
}

func (states gqlIssueStates) includes(it *issue) bool {
	switch it.state {
	case "open":
		return states&gqlOpen != 0
	case "closed":
		return states&gqlClosed != 0
	default:
		return true
	}
}

func resolveIssuesField(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	states := gqlReadIssueStates(args["states"])
	order, _ := toMap(args["orderBy"])
	direction, _ := toString(order["direction"])
	ordered := selectInOrder(r.issues, direction != "ASC", states.includes)
	page, hasNext := sliceIssueConnection(ordered, args)
	nodes := make([]*gqlNode, len(page))
	for i, it := range page {
		nodes[i] = issueNode(r, it)
	}
	info := &pageInfo{hasNext: hasNext}
	if n := len(page); n > 0 {
		info.endCursor = encodeCursor(page[n-1].number)
	}
	return &gqlNode{typ: "IssueConnection", val: &issueConn{nodes: nodes, page: info}}, nil
}

func sliceIssueConnection(issues []*issue, args map[string]any) ([]*issue, bool) {
	cursor, _ := toString(args["after"])
	if number, valid := decodeCursor(cursor); valid {
		// An absent or stale cursor leaves the connection at its first item.
		index := slices.IndexFunc(issues, func(it *issue) bool { return it.number == number })
		issues = issues[index+1:]
	}
	if limit, valid := toInt(args["first"]); valid && limit >= 0 && limit < len(issues) {
		return issues[:limit], true
	}
	return issues, false
}

func encodeCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func decodeCursor(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := strings.TrimPrefix(string(raw), "cursor:")
	n, err := strconv.Atoi(text)
	return n, err == nil
}

func resolvePullRequestsField(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	first := -1
	if raw, ok := args["first"]; ok {
		if n, ok := toInt(raw); ok {
			first = n
		}
	}
	var nodes []*gqlNode
	for _, pr := range r.pulls {
		if pr.state != "open" {
			continue
		}
		nodes = append(nodes, &gqlNode{typ: "PullRequest", val: &pullRef{repo: r, pr: pr}})
		if first >= 0 && len(nodes) >= first {
			break
		}
	}
	if nodes == nil {
		nodes = []*gqlNode{}
	}
	return &gqlNode{typ: "PullRequestConnection", val: nodes}, nil
}

func resolvePullRequestField(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	number, ok := toInt(args["number"])
	if !ok {
		return nil, nil
	}
	pr := r.pullsByNum[number]
	if pr == nil {
		return nil, nil
	}
	return &gqlNode{typ: "PullRequest", val: &pullRef{repo: r, pr: pr}}, nil
}

func resolvePullState(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	pr := obj.(*pullRef).pr
	if pr.merged {
		return "MERGED", nil
	}
	if pr.state == "closed" {
		return "CLOSED", nil
	}
	return "OPEN", nil
}

func resolvePullMergeable(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	p := obj.(*pullRef)
	if p.pr.state != "open" {
		return "UNKNOWN", nil
	}
	if p.repo.mergeable(p.repo.refs["refs/heads/"+p.pr.baseRef], p.repo.refs["refs/heads/"+p.pr.headRef]) {
		return "MERGEABLE", nil
	}
	return "CONFLICTING", nil
}

func resolvePullFiles(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	p := obj.(*pullRef)
	first := -1
	if raw, ok := args["first"]; ok {
		if n, ok := toInt(raw); ok {
			first = n
		}
	}
	changes := e.srv.pullFilesView(p.repo, p.pr)
	var nodes []*gqlNode
	for _, ch := range changes {
		nodes = append(nodes, &gqlNode{typ: "PullRequestChangedFile", val: ch.Filename})
		if first >= 0 && len(nodes) >= first {
			break
		}
	}
	if nodes == nil {
		nodes = []*gqlNode{}
	}
	return &gqlNode{typ: "PullRequestChangedFileConnection", val: nodes}, nil
}

// ---------------------------------------------------------------------------
// Mutation resolvers

func inputOf(args map[string]any) (map[string]any, bool) {
	raw, ok := args["input"]
	if !ok {
		return nil, false
	}
	return toMap(raw)
}

func resolveCreateIssue(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	input, ok := inputOf(args)
	if !ok {
		return nil, fmt.Errorf("createIssue needs an input object")
	}
	repoID, _ := toString(input["repositoryId"])
	r := e.lookupRepoByID(repoID)
	if r == nil {
		return nil, fmt.Errorf("no repository matches repositoryId %q", repoID)
	}
	title, _ := toString(input["title"])
	bodyText, _ := toString(input["body"])
	it := e.srv.st.createIssue(r, title, bodyText, nil)
	return &gqlNode{typ: "CreateIssuePayload", val: &gqlNode{typ: "Issue", val: &issueRef{repo: r, issue: it}}}, nil
}

func resolveCloseIssue(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	input, ok := inputOf(args)
	if !ok {
		return nil, fmt.Errorf("closeIssue needs an input object")
	}
	issueID, _ := toString(input["issueId"])
	parts, ok := decodeGlobalID(issueID)
	if !ok {
		return nil, fmt.Errorf("no issue matches issueId %q", issueID)
	}
	ref := e.lookupIssue(parts)
	if ref == nil {
		return nil, fmt.Errorf("no issue matches issueId %q", issueID)
	}
	ref.issue.state = "closed"
	ref.issue.invalidateView()
	return &gqlNode{typ: "CloseIssuePayload", val: &gqlNode{typ: "Issue", val: ref}}, nil
}

func resolveCreatePullRequest(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	input, ok := inputOf(args)
	if !ok {
		return nil, fmt.Errorf("createPullRequest needs an input object")
	}
	repoID, _ := toString(input["repositoryId"])
	r := e.lookupRepoByID(repoID)
	if r == nil {
		return nil, fmt.Errorf("no repository matches repositoryId %q", repoID)
	}
	base, _ := toString(input["baseRefName"])
	head, _ := toString(input["headRefName"])
	title, _ := toString(input["title"])
	headSha, okHead := r.refs["refs/heads/"+head]
	baseSha, okBase := r.refs["refs/heads/"+base]
	if !okHead {
		return nil, fmt.Errorf("the head branch %q does not exist", head)
	}
	if !okBase {
		return nil, fmt.Errorf("the base branch %q does not exist", base)
	}
	pr := e.srv.st.createPull(r, title, "", head, base, headSha, baseSha)
	return &gqlNode{typ: "CreatePullRequestPayload", val: &gqlNode{typ: "PullRequest", val: &pullRef{repo: r, pr: pr}}}, nil
}

func resolveMergePullRequest(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	input, ok := inputOf(args)
	if !ok {
		return nil, fmt.Errorf("mergePullRequest needs an input object")
	}
	prID, _ := toString(input["pullRequestId"])
	parts, ok := decodeGlobalID(prID)
	if !ok {
		return nil, fmt.Errorf("no pull request matches pullRequestId %q", prID)
	}
	ref := e.lookupPull(parts)
	if ref == nil {
		return nil, fmt.Errorf("no pull request matches pullRequestId %q", prID)
	}
	_, conflict, already := e.srv.mergePullRequest(ref.repo, ref.pr)
	if already {
		return nil, fmt.Errorf("the pull request is already closed")
	}
	if conflict {
		return nil, fmt.Errorf("the pull request cannot be merged: conflicting changes")
	}
	return &gqlNode{typ: "MergePullRequestPayload", val: &gqlNode{typ: "PullRequest", val: ref}}, nil
}

func resolveAddComment(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	input, ok := inputOf(args)
	if !ok {
		return nil, fmt.Errorf("addComment needs an input object")
	}
	subjectID, _ := toString(input["subjectId"])
	parts, ok := decodeGlobalID(subjectID)
	if !ok {
		return nil, fmt.Errorf("no subject matches subjectId %q", subjectID)
	}
	ref := e.lookupIssue(parts)
	if ref == nil {
		return nil, fmt.Errorf("no issue matches subjectId %q", subjectID)
	}
	bodyText, _ := toString(input["body"])
	c := e.srv.st.addComment(ref.repo, ref.issue, bodyText)
	commentNode := &gqlNode{typ: "IssueComment", val: &commentRef{repo: ref.repo, issue: ref.issue, comment: c}}
	edge := &gqlNode{typ: "AddedCommentEdge", val: commentNode}
	return &gqlNode{typ: "AddCommentPayload", val: edge}, nil
}

// ---------------------------------------------------------------------------
// Execution

// execute runs one operation document against the store.
func (e *gqlExecutor) execute(op *gqlOperation) map[string]any {
	rootType := "Query"
	if op.opType == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlNode{typ: rootType}
	return e.executeSelectionSet(op.sel, root, nil)
}

func (e *gqlExecutor) executeSelectionSet(sel *gqlSelectionSet, obj *gqlNode, path *gqlFragmentPath) map[string]any {
	if sel == nil || obj == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(sel.sels))
	e.projectSelections(out, sel, obj, path)
	return out
}

type gqlFragmentPath struct {
	definition *gqlFragmentDef
	parent     *gqlFragmentPath
}

// Inline fragments and spreads project into the same response object, in
// encounter order. This also preserves resolver error and mutation order.
func (e *gqlExecutor) projectSelections(out map[string]any, sel *gqlSelectionSet, obj *gqlNode, path *gqlFragmentPath) {
	for _, selection := range sel.sels {
		if f := selection.field; f != nil {
			key := f.alias
			if key == "" {
				key = f.name
			}
			out[key] = e.executeField(f, obj, path)
			continue
		}
		if inline := selection.fragment; inline != nil {
			if gqlTypeMatches(inline.typeCond, obj.typ) {
				e.projectSelections(out, inline.sel, obj, path)
			}
			continue
		}
		definition := e.doc.fragments[selection.spread]
		if definition == nil || !gqlTypeMatches(definition.typeCond, obj.typ) {
			continue
		}
		if path.contains(definition) {
			e.addError(fmt.Sprintf("Cannot spread fragment %q within itself.", definition.name))
			continue
		}
		e.projectSelections(out, definition.sel, obj, &gqlFragmentPath{definition: definition, parent: path})
	}
}

func gqlTypeMatches(want, actual string) bool { return want == "" || want == actual }

func (p *gqlFragmentPath) contains(definition *gqlFragmentDef) bool {
	for ; p != nil; p = p.parent {
		if p.definition == definition {
			return true
		}
	}
	return false
}

func (e *gqlExecutor) addError(message string) {
	e.errs = append(e.errs, gqlError{Message: message})
}

func (e *gqlExecutor) executeField(f *gqlField, obj *gqlNode, path *gqlFragmentPath) any {
	def, ok := fieldTables[obj.typ][f.name]
	if !ok {
		e.addError(fmt.Sprintf("Cannot query field %q on type %q.", f.name, obj.typ))
		return nil
	}
	value, err := def.resolve(e, obj.val, evaluateGQLPairs(f.args, e.vars))
	if err != nil {
		e.addError(err.Error())
		return nil
	}
	switch v := value.(type) {
	case *gqlNode:
		if f.sel == nil {
			return nil
		}
		return e.executeSelectionSet(f.sel, v, path)
	case []*gqlNode:
		if f.sel == nil {
			return value
		}
		items := make([]any, len(v))
		for i, n := range v {
			items[i] = e.executeSelectionSet(f.sel, n, path)
		}
		return items
	default:
		return value
	}
}

// ---------------------------------------------------------------------------
// HTTP entry point

// handleGraphQL answers POST /api/graphql: one operation or a batched array.
func (s *server) handleGraphQL(q *request) response {
	trimmed := strings.TrimSpace(string(q.body))
	if trimmed == "" {
		return jsonResponse(400, mustJSON(gqlFailure("the request body is empty")))
	}
	if trimmed[0] == '[' {
		return s.handleGraphQLBatch(q.body)
	}
	var op map[string]any
	if err := json.Unmarshal(q.body, &op); err != nil {
		return jsonResponse(400, mustJSON(gqlFailure("the request body is not JSON")))
	}
	return jsonResponse(200, mustJSON(s.runGraphQLOperation(op)))
}

func (s *server) handleGraphQLBatch(body []byte) response {
	var operations []map[string]any
	if err := json.Unmarshal(body, &operations); err != nil {
		return jsonResponse(400, mustJSON(gqlFailure("the request body is not JSON")))
	}
	results := make([]map[string]any, len(operations))
	for i, operation := range operations {
		results[i] = s.runGraphQLOperation(operation)
	}
	return jsonResponse(200, mustJSON(results))
}

func gqlFailure(message string) map[string]any {
	return map[string]any{"errors": []gqlError{{Message: message}}}
}

func (d *gqlDocument) chooseOperation(name string) *gqlOperation {
	if name != "" {
		for _, operation := range d.operations {
			if operation.name == name {
				return operation
			}
		}
	}
	return d.operations[0]
}

func (s *server) runGraphQLOperation(op map[string]any) map[string]any {
	query, _ := toString(op["query"])
	if query == "" {
		return gqlFailure("the request carries no query")
	}
	doc, err := parseGraphQLDocument(query)
	if err != nil {
		return gqlFailure(err.Error())
	}
	// The operation name selects among several; with one operation it is it.
	operationName, _ := toString(op["operationName"])
	vars, _ := toMap(op["variables"])
	ex := &gqlExecutor{srv: s, vars: vars, doc: doc}
	envelope := map[string]any{"data": ex.execute(doc.chooseOperation(operationName))}
	if len(ex.errs) > 0 {
		envelope["errors"] = ex.errs
	}
	return envelope
}
