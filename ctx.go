package main

// The request context every handler receives: the store, the request line,
// the query, the headers and the raw body (already read by the server).

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Ctx is the per-request context handed to every handler.
type Ctx struct {
	// Store is the shared in-memory store (locked by the server for the
	// whole handler call; handlers must not lock it again).
	Store *Store
	// Token is the bearer token the process was given (MAKEFUN_SHOP_TOKEN).
	Token string
	// Method is the HTTP method, e.g. "GET".
	Method string
	// Path is the decoded request path (no query string).
	Path string
	// Query holds the parsed query string.
	Query url.Values
	// Headers are the request headers.
	Headers http.Header
	// Body is the raw request body (empty when there is none).
	Body []byte
}

// Authorized reports whether the request carries the process's bearer
// token: either as a Bearer token, or as a Basic credential whose user
// or password equals the token (the way a real git client authenticates).
func Authorized(ctx *Ctx) bool {
	header := ctx.Headers.Get("Authorization")
	if header == "" {
		return false
	}
	if strings.HasPrefix(header, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		return token != "" && token == ctx.Token
	}
	if strings.HasPrefix(header, "Basic ") {
		// Strip whitespace before decoding: the TS side uses
		// Buffer.from(..., 'base64'), which tolerates wrapped or
		// padded credentials, while Go's decoder is strict.
		credential := strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\r', '\n', '\t':
				return -1
			default:
				return r
			}
		}, strings.TrimPrefix(header, "Basic "))
		decoded, err := base64.StdEncoding.DecodeString(credential)
		if err != nil {
			return false
		}
		text := string(decoded)
		user, password := text, ""
		if colon := strings.Index(text, ":"); colon >= 0 {
			user, password = text[:colon], text[colon+1:]
		}
		return ctx.Token != "" && (user == ctx.Token || password == ctx.Token)
	}
	return false
}

// JSONBody parses the JSON body: (nil, true) when there is no body,
// (value, true) when it parsed, (nil, false) when it is malformed.
func JSONBody(ctx *Ctx) (any, bool) {
	if len(ctx.Body) == 0 {
		return nil, true
	}
	var value any
	if err := json.Unmarshal(ctx.Body, &value); err != nil {
		return nil, false
	}
	return value, true
}
