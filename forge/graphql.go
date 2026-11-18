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
	src string
	pos int
}

func (l *gqlLexer) next() (gqlToken, error) {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.pos++
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '{':
			l.pos++
			return gqlToken{kind: tokLBrace, text: "{"}, nil
		case c == '}':
			l.pos++
			return gqlToken{kind: tokRBrace, text: "}"}, nil
		case c == '(':
			l.pos++
			return gqlToken{kind: tokLParen, text: "("}, nil
		case c == ')':
			l.pos++
			return gqlToken{kind: tokRParen, text: ")"}, nil
		case c == '[':
			l.pos++
			return gqlToken{kind: tokLBracket, text: "["}, nil
		case c == ']':
			l.pos++
			return gqlToken{kind: tokRBracket, text: "]"}, nil
		case c == ':':
			l.pos++
			return gqlToken{kind: tokColon, text: ":"}, nil
		case c == '$':
			l.pos++
			return gqlToken{kind: tokDollar, text: "$"}, nil
		case c == '!':
			l.pos++
			return gqlToken{kind: tokBang, text: "!"}, nil
		case c == '=':
			l.pos++
			return gqlToken{kind: tokEquals, text: "="}, nil
		case c == '@':
			l.pos++
			return gqlToken{kind: tokAt, text: "@"}, nil
		case c == '.' && l.pos+2 < len(l.src) && l.src[l.pos+1] == '.' && l.src[l.pos+2] == '.':
			l.pos += 3
			return gqlToken{kind: tokSpread, text: "..."}, nil
		case c == '"':
			return l.readString()
		case c == '-' || (c >= '0' && c <= '9'):
			return l.readNumber()
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			start := l.pos
			for l.pos < len(l.src) {
				d := l.src[l.pos]
				if d == '_' || (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z') || (d >= '0' && d <= '9') {
					l.pos++
				} else {
					break
				}
			}
			return gqlToken{kind: tokName, text: l.src[start:l.pos]}, nil
		default:
			return gqlToken{}, fmt.Errorf("unexpected character %q in document", c)
		}
	}
	return gqlToken{kind: tokEOF}, nil
}

func (l *gqlLexer) readString() (gqlToken, error) {
	l.pos++ // opening quote
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			l.pos++
			return gqlToken{kind: tokString, text: b.String()}, nil
		}
		if c == '\n' {
			return gqlToken{}, fmt.Errorf("unterminated string")
		}
		if c == '\\' && l.pos+1 < len(l.src) {
			l.pos++
			esc := l.src[l.pos]
			switch esc {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if l.pos+4 < len(l.src) {
					if n, err := strconv.ParseUint(l.src[l.pos+1:l.pos+5], 16, 32); err == nil {
						b.WriteRune(rune(n))
						l.pos += 4
					}
				}
			default:
				b.WriteByte(esc)
			}
			l.pos++
			continue
		}
		b.WriteByte(c)
		l.pos++
	}
	return gqlToken{}, fmt.Errorf("unterminated string")
}

func (l *gqlLexer) readNumber() (gqlToken, error) {
	start := l.pos
	if l.src[l.pos] == '-' {
		l.pos++
	}
	isFloat := false
	for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		isFloat = true
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		isFloat = true
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	}
	kind := tokInt
	if isFloat {
		kind = tokFloat
	}
	return gqlToken{kind: kind, text: l.src[start:l.pos]}, nil
}

// ---------------------------------------------------------------------------
// Parser

type gqlValue struct {
	kind    string // var, int, float, string, bool, null, enum, list, object
	name    string // variable name or enum value
	str     string
	num     string
	boolean bool
	list    []*gqlValue
	object  []gqlObjectField
}

type gqlObjectField struct {
	name  string
	value *gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
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

type gqlVarDef struct {
	name string
}

type gqlOperation struct {
	opType string
	name   string
	vars   []gqlVarDef
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
	lx := &gqlLexer{src: src}
	var tokens []gqlToken
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
		if t.kind == tokEOF {
			break
		}
	}
	p := &gqlParser{tokens: tokens}
	doc := &gqlDocument{fragments: map[string]*gqlFragmentDef{}}
	for p.peek().kind != tokEOF {
		if p.peek().kind == tokName && p.peek().text == "fragment" {
			f, err := p.parseFragment()
			if err != nil {
				return nil, err
			}
			doc.fragments[f.name] = f
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, op)
	}
	if len(doc.operations) == 0 {
		return nil, fmt.Errorf("the document holds no operation")
	}
	return doc, nil
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) take() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }

func (p *gqlParser) expect(kind gqlTokenKind) (gqlToken, error) {
	t := p.take()
	if t.kind != kind {
		return t, fmt.Errorf("expected %v, got %q", kind, t.text)
	}
	return t, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{opType: "query"}
	if t := p.peek(); t.kind == tokName && (t.text == "query" || t.text == "mutation" || t.text == "subscription") {
		op.opType = p.take().text
		if p.peek().kind == tokName {
			op.name = p.take().text
		}
	}
	if p.peek().kind == tokLParen {
		if err := p.parseVarDefs(op); err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) parseVarDefs(op *gqlOperation) error {
	if _, err := p.expect(tokLParen); err != nil {
		return err
	}
	for p.peek().kind != tokRParen {
		if _, err := p.expect(tokDollar); err != nil {
			return err
		}
		name, err := p.expect(tokName)
		if err != nil {
			return err
		}
		op.vars = append(op.vars, gqlVarDef{name: name.text})
		if _, err := p.expect(tokColon); err != nil {
			return err
		}
		if err := p.parseType(); err != nil {
			return err
		}
		if p.peek().kind == tokEquals {
			p.take()
			if _, err := p.parseValue(); err != nil {
				return err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return err
		}
	}
	_, err := p.expect(tokRParen)
	return err
}

func (p *gqlParser) parseType() error {
	switch p.peek().kind {
	case tokLBracket:
		p.take()
		if err := p.parseType(); err != nil {
			return err
		}
		if _, err := p.expect(tokRBracket); err != nil {
			return err
		}
	case tokName:
		p.take()
	default:
		return fmt.Errorf("expected a type, got %q", p.peek().text)
	}
	if p.peek().kind == tokBang {
		p.take()
	}
	return nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == tokAt {
		p.take()
		if _, err := p.expect(tokName); err != nil {
			return err
		}
		if p.peek().kind == tokLParen {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() (*gqlSelectionSet, error) {
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	set := &gqlSelectionSet{}
	for p.peek().kind != tokRBrace {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		set.sels = append(set.sels, sel)
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	return set, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek().kind == tokSpread {
		p.take()
		if t := p.peek(); t.kind == tokName && t.text == "on" {
			p.take()
			typeTok, err := p.expect(tokName)
			if err != nil {
				return gqlSelection{}, err
			}
			if err := p.skipDirectives(); err != nil {
				return gqlSelection{}, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{fragment: &gqlInlineFragment{typeCond: typeTok.text, sel: sel}}, nil
		}
		if p.peek().kind == tokName {
			name := p.take().text
			if err := p.skipDirectives(); err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{spread: name}, nil
		}
		if err := p.skipDirectives(); err != nil {
			return gqlSelection{}, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		return gqlSelection{fragment: &gqlInlineFragment{sel: sel}}, nil
	}
	nameTok, err := p.expect(tokName)
	if err != nil {
		return gqlSelection{}, err
	}
	f := &gqlField{name: nameTok.text}
	if p.peek().kind == tokColon {
		p.take()
		fieldTok, err := p.expect(tokName)
		if err != nil {
			return gqlSelection{}, err
		}
		f.alias = f.name
		f.name = fieldTok.text
	}
	if p.peek().kind == tokLParen {
		args, err := p.parseArguments()
		if err != nil {
			return gqlSelection{}, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return gqlSelection{}, err
	}
	if p.peek().kind == tokLBrace {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		f.sel = sel
	}
	return gqlSelection{field: f}, nil
}

func (p *gqlParser) parseArguments() ([]gqlArg, error) {
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	var args []gqlArg
	for p.peek().kind != tokRParen {
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
		args = append(args, gqlArg{name: name.text, value: value})
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.take()
	switch t.kind {
	case tokDollar:
		name, err := p.expect(tokName)
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", name: name.text}, nil
	case tokInt:
		return &gqlValue{kind: "int", num: t.text}, nil
	case tokFloat:
		return &gqlValue{kind: "float", num: t.text}, nil
	case tokString:
		return &gqlValue{kind: "string", str: t.text}, nil
	case tokLBracket:
		v := &gqlValue{kind: "list"}
		for p.peek().kind != tokRBracket {
			elem, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, elem)
		}
		if _, err := p.expect(tokRBracket); err != nil {
			return nil, err
		}
		return v, nil
	case tokLBrace:
		v := &gqlValue{kind: "object"}
		for p.peek().kind != tokRBrace {
			name, err := p.expect(tokName)
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
			elem, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.object = append(v.object, gqlObjectField{name: name.text, value: elem})
		}
		if _, err := p.expect(tokRBrace); err != nil {
			return nil, err
		}
		return v, nil
	case tokName:
		switch t.text {
		case "true":
			return &gqlValue{kind: "bool", boolean: true}, nil
		case "false":
			return &gqlValue{kind: "bool", boolean: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", name: t.text}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.text)
}

func (p *gqlParser) parseFragment() (*gqlFragmentDef, error) {
	p.take() // "fragment"
	name, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	on, err := p.expect(tokName)
	if err != nil || on.text != "on" {
		return nil, fmt.Errorf("expected 'on' in a fragment definition")
	}
	typeTok, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlFragmentDef{name: name.text, typeCond: typeTok.text, sel: sel}, nil
}

// ---------------------------------------------------------------------------
// Values and global ids

// evalValue resolves a value node against the operation's variables.
func evalValue(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		if vars == nil {
			return nil
		}
		return vars[v.name]
	case "int":
		if n, err := strconv.ParseInt(v.num, 10, 64); err == nil {
			return int(n)
		}
		return 0
	case "float":
		if f, err := strconv.ParseFloat(v.num, 64); err == nil {
			return f
		}
		return 0.0
	case "string":
		return v.str
	case "bool":
		return v.boolean
	case "null":
		return nil
	case "enum":
		return v.name
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, evalValue(e, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range v.object {
			out[f.name] = evalValue(f.value, vars)
		}
		return out
	}
	return nil
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
	message string
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
			if dbid, err := strconv.Atoi(parts[1]); err == nil {
				for _, r := range e.srv.st.order {
					if r.id == dbid {
						return &gqlNode{typ: "Repository", val: r}, nil
					}
				}
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

// lookupIssue resolves ["I", repoId, number] to an issue reference.
func (e *gqlExecutor) lookupIssue(parts []string) *issueRef {
	if len(parts) != 3 {
		return nil
	}
	dbid, err1 := strconv.Atoi(parts[1])
	number, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return nil
	}
	for _, r := range e.srv.st.order {
		if r.id == dbid {
			if it := r.issuesByNum[number]; it != nil {
				return &issueRef{repo: r, issue: it}
			}
		}
	}
	return nil
}

func (e *gqlExecutor) lookupPull(parts []string) *pullRef {
	if len(parts) != 3 {
		return nil
	}
	dbid, err1 := strconv.Atoi(parts[1])
	number, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return nil
	}
	for _, r := range e.srv.st.order {
		if r.id == dbid {
			if pr := r.pullsByNum[number]; pr != nil {
				return &pullRef{repo: r, pr: pr}
			}
		}
	}
	return nil
}

func (e *gqlExecutor) lookupRepoByID(id string) *repository {
	parts, ok := decodeGlobalID(id)
	if !ok || len(parts) != 2 || parts[0] != "R" {
		return nil
	}
	dbid, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	for _, r := range e.srv.st.order {
		if r.id == dbid {
			return r
		}
	}
	return nil
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
	it := r.issuesByNum[number]
	if it == nil {
		return nil, nil
	}
	return &gqlNode{typ: "Issue", val: &issueRef{repo: r, issue: it}}, nil
}

func resolveIssuesField(e *gqlExecutor, obj any, args map[string]any) (any, error) {
	r := obj.(*repository)
	// States filter: a list of OPEN/CLOSED, or all states when absent.
	var wantOpen, wantClosed bool
	if raw, ok := args["states"]; ok && raw != nil {
		if list, ok := raw.([]any); ok {
			for _, s := range list {
				if str, ok := toString(s); ok {
					if str == "OPEN" {
						wantOpen = true
					}
					if str == "CLOSED" {
						wantClosed = true
					}
				}
			}
		}
	} else {
		wantOpen, wantClosed = true, true
	}
	// Order: CREATED_AT DESC is the default; ASC reverses it.
	desc := true
	if raw, ok := args["orderBy"]; ok {
		if ob, ok := toMap(raw); ok {
			if dir, ok := toString(ob["direction"]); ok && dir == "ASC" {
				desc = false
			}
		}
	}
	var ordered []*issue
	for _, it := range r.issues {
		if it.state == "open" && !wantOpen {
			continue
		}
		if it.state == "closed" && !wantClosed {
			continue
		}
		ordered = append(ordered, it)
	}
	if desc {
		for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		}
	}
	// Cursor: continue after the issue the cursor names.
	start := 0
	if raw, ok := args["after"]; ok && raw != nil {
		if cursor, ok := toString(raw); ok && cursor != "" {
			if num, ok := decodeCursor(cursor); ok {
				for i, it := range ordered {
					if it.number == num {
						start = i + 1
						break
					}
				}
			}
		}
	}
	first := len(ordered)
	if raw, ok := args["first"]; ok {
		if n, ok := toInt(raw); ok && n >= 0 {
			first = n
		}
	}
	end := start + first
	if end > len(ordered) {
		end = len(ordered)
	}
	if start > len(ordered) {
		start = len(ordered)
	}
	page := ordered[start:end]
	nodes := make([]*gqlNode, 0, len(page))
	for _, it := range page {
		nodes = append(nodes, &gqlNode{typ: "Issue", val: &issueRef{repo: r, issue: it}})
	}
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].number)
	}
	return &gqlNode{typ: "IssueConnection", val: &issueConn{
		nodes: nodes,
		page:  &pageInfo{hasNext: end < len(ordered), endCursor: endCursor},
	}}, nil
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
	return e.executeSelectionSet(op.sel, root)
}

func (e *gqlExecutor) executeSelectionSet(sel *gqlSelectionSet, obj *gqlNode) map[string]any {
	out := map[string]any{}
	if sel == nil || obj == nil {
		return out
	}
	for _, s := range sel.sels {
		switch {
		case s.field != nil:
			key := s.field.name
			if s.field.alias != "" {
				key = s.field.alias
			}
			out[key] = e.executeField(s.field, obj)
		case s.fragment != nil:
			if s.fragment.typeCond == "" || s.fragment.typeCond == obj.typ {
				for k, v := range e.executeSelectionSet(s.fragment.sel, obj) {
					out[k] = v
				}
			}
		case s.spread != "":
			if def, ok := e.doc.fragments[s.spread]; ok {
				if def.typeCond == "" || def.typeCond == obj.typ {
					for k, v := range e.executeSelectionSet(def.sel, obj) {
						out[k] = v
					}
				}
			}
		}
	}
	return out
}

func (e *gqlExecutor) executeField(f *gqlField, obj *gqlNode) any {
	table := fieldTables[obj.typ]
	def, ok := table[f.name]
	if !ok {
		e.errs = append(e.errs, gqlError{message: fmt.Sprintf("Cannot query field %q on type %q.", f.name, obj.typ)})
		return nil
	}
	args := map[string]any{}
	for _, a := range f.args {
		args[a.name] = evalValue(a.value, e.vars)
	}
	value, err := def.resolve(e, obj.val, args)
	if err != nil {
		e.errs = append(e.errs, gqlError{message: err.Error()})
		return nil
	}
	if value == nil {
		return nil
	}
	if f.sel == nil {
		// A leaf: scalars pass through; an object without a selection set is
		// an error in GraphQL, but the documents never do that.
		if node, ok := value.(*gqlNode); ok {
			_ = node
			return nil
		}
		return value
	}
	switch v := value.(type) {
	case *gqlNode:
		return e.executeSelectionSet(f.sel, v)
	case []*gqlNode:
		out := make([]any, 0, len(v))
		for _, n := range v {
			out = append(out, e.executeSelectionSet(f.sel, n))
		}
		return out
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
		return jsonResponse(400, mustJSON(map[string]any{"errors": []map[string]string{{"message": "the request body is empty"}}}))
	}
	if strings.HasPrefix(trimmed, "[") {
		var ops []map[string]any
		if err := json.Unmarshal(q.body, &ops); err != nil {
			return jsonResponse(400, mustJSON(map[string]any{"errors": []map[string]string{{"message": "the request body is not JSON"}}}))
		}
		results := make([]any, 0, len(ops))
		for _, op := range ops {
			results = append(results, s.runGraphQLOperation(op))
		}
		return jsonResponse(200, mustJSON(results))
	}
	var op map[string]any
	if err := json.Unmarshal(q.body, &op); err != nil {
		return jsonResponse(400, mustJSON(map[string]any{"errors": []map[string]string{{"message": "the request body is not JSON"}}}))
	}
	return jsonResponse(200, mustJSON(s.runGraphQLOperation(op)))
}

func (s *server) runGraphQLOperation(op map[string]any) map[string]any {
	query, _ := toString(op["query"])
	if query == "" {
		return map[string]any{"errors": []map[string]string{{"message": "the request carries no query"}}}
	}
	vars := map[string]any{}
	if raw, ok := op["variables"]; ok {
		if m, ok := toMap(raw); ok {
			vars = m
		}
	}
	doc, err := parseGraphQLDocument(query)
	if err != nil {
		return map[string]any{"errors": []map[string]string{{"message": err.Error()}}}
	}
	// The operation name selects among several; with one operation it is it.
	operationName, _ := toString(op["operationName"])
	operation := doc.operations[0]
	if operationName != "" {
		for _, cand := range doc.operations {
			if cand.name == operationName {
				operation = cand
				break
			}
		}
	}
	ex := &gqlExecutor{srv: s, vars: vars, doc: doc}
	data := ex.execute(operation)
	envelope := map[string]any{"data": data}
	if len(ex.errs) > 0 {
		errList := make([]map[string]string, 0, len(ex.errs))
		for _, e := range ex.errs {
			errList = append(errList, map[string]string{"message": e.message})
		}
		envelope["errors"] = errList
	}
	return envelope
}
