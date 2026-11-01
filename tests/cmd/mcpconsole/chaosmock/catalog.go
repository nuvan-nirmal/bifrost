package chaosmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Prompts, resources and resource templates are served the way tools are: a
// base list every caller sees, and per-identity overlays that replace it
// wholesale for one caller. A server advertises a kind in initialize only when
// it holds some of it, in the base list or in any overlay, and never per
// caller: capabilities describe the server, so a caller with an empty view of
// a kind the server offers gets an empty list rather than method-not-found.
//
// What prompts/get and resources/read return names the identity that asked,
// so a test can prove from the response alone whose view answered.

// Prompt is one prompt a virtual server offers. Every field goes on the wire
// in prompts/list.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is one argument a prompt takes.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Validate rejects a prompt or argument a client could not address.
func (p Prompt) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("every prompt needs a name")
	}
	for _, a := range p.Arguments {
		if a.Name == "" {
			return fmt.Errorf("prompt %q: every argument needs a name", p.Name)
		}
	}
	return nil
}

// Resource is one resource a virtual server offers to read. Every field goes
// on the wire in resources/list; an empty name is filled with the uri.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// Validate rejects a resource with no uri to read it by.
func (r Resource) Validate() error {
	if r.URI == "" {
		return fmt.Errorf("every resource needs a uri")
	}
	return nil
}

// ResourceTemplate is a family of resources addressed by an RFC 6570 URI
// template. Only simple {var} and reserved {+var} expressions are supported,
// which is what servers publish in practice; anything else is refused rather
// than matched wrongly. An empty name is filled with the template.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// Validate rejects a template the mock could not match a uri against.
func (rt ResourceTemplate) Validate() error {
	if rt.URITemplate == "" {
		return fmt.Errorf("every template needs a uriTemplate")
	}
	if _, err := compileURITemplate(rt.URITemplate); err != nil {
		return fmt.Errorf("template %q: %w", rt.URITemplate, err)
	}
	return nil
}

// validator is what every catalog entry implements.
type validator interface {
	Validate() error
}

// catalog is one kind's base list and per-identity overlays. Lists are kept
// sorted by key, so successive listings differ only when the content does.
type catalog[T validator] struct {
	mu   sync.RWMutex
	kind string         // names the kind in errors
	key  func(T) string // what two entries may not share
	fill func(T) T      // fills defaults on the way in

	base     []T
	overlays map[string][]T
}

func newCatalog[T validator](kind string, key func(T) string, fill func(T) T) *catalog[T] {
	return &catalog[T]{kind: kind, key: key, fill: fill, base: []T{}, overlays: map[string][]T{}}
}

func newPromptCatalog() *catalog[Prompt] {
	return newCatalog("prompt", func(p Prompt) string { return p.Name }, func(p Prompt) Prompt { return p })
}

func newResourceCatalog() *catalog[Resource] {
	return newCatalog("resource", func(r Resource) string { return r.URI }, func(r Resource) Resource {
		if r.Name == "" {
			r.Name = r.URI
		}
		return r
	})
}

func newTemplateCatalog() *catalog[ResourceTemplate] {
	return newCatalog("template", func(t ResourceTemplate) string { return t.URITemplate }, func(t ResourceTemplate) ResourceTemplate {
		if t.Name == "" {
			t.Name = t.URITemplate
		}
		return t
	})
}

// validate checks every entry and refuses two with the same key, since only
// one of them could ever be addressed.
func (c *catalog[T]) validate(items []T) error {
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return err
		}
		k := c.key(item)
		if seen[k] {
			return fmt.Errorf("two %ss share %q", c.kind, k)
		}
		seen[k] = true
	}
	return nil
}

// sorted returns a filled, sorted copy of items, never nil.
func (c *catalog[T]) sorted(items []T) []T {
	out := make([]T, len(items))
	for i, item := range items {
		out[i] = c.fill(item)
	}
	sort.Slice(out, func(i, j int) bool { return c.key(out[i]) < c.key(out[j]) })
	return out
}

// set replaces the base list.
func (c *catalog[T]) set(items []T) {
	sorted := c.sorted(items)
	c.mu.Lock()
	c.base = sorted
	c.mu.Unlock()
}

// setOverlay gives identity its own list, which may be empty.
func (c *catalog[T]) setOverlay(identity string, items []T) {
	sorted := c.sorted(items)
	c.mu.Lock()
	c.overlays[identity] = sorted
	c.mu.Unlock()
}

// clearOverlay returns identity to the base list, reporting whether it had an
// overlay to clear.
func (c *catalog[T]) clearOverlay(identity string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.overlays[identity]; !ok {
		return false
	}
	delete(c.overlays, identity)
	return true
}

// list returns a copy of what identity sees, never nil.
func (c *catalog[T]) list(identity string) []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	src := c.base
	if identity != "" {
		if o, ok := c.overlays[identity]; ok {
			src = o
		}
	}
	return append([]T{}, src...)
}

// lookup finds the entry with key k in identity's view.
func (c *catalog[T]) lookup(identity, k string) (T, bool) {
	for _, item := range c.list(identity) {
		if c.key(item) == k {
			return item, true
		}
	}
	var zero T
	return zero, false
}

// views returns every overlay, nil when there are none.
func (c *catalog[T]) views() map[string][]T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.overlays) == 0 {
		return nil
	}
	out := make(map[string][]T, len(c.overlays))
	for identity, items := range c.overlays {
		out[identity] = append([]T{}, items...)
	}
	return out
}

// holdsAny reports whether any caller can see an entry, which is what decides
// whether the kind is advertised.
func (c *catalog[T]) holdsAny() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.base) > 0 {
		return true
	}
	for _, items := range c.overlays {
		if len(items) > 0 {
			return true
		}
	}
	return false
}

// applyCatalogControl handles the <kind>/set and <kind>/overlay verbs. Set
// replaces the base list and refuses an identity, which would otherwise be
// ignored; overlay needs one, and clearing an overlay that is not there is
// refused like every other verb that would do nothing.
func applyCatalogControl[T validator](c *catalog[T], overlay bool, items []T, body controlBody) error {
	if !overlay {
		if body.Identity != "" || body.Clear {
			return errBadRequest(c.kind + "s/set replaces the base list; give one identity its own with " + c.kind + "s/overlay")
		}
		if err := c.validate(items); err != nil {
			return errBadRequest(err.Error())
		}
		c.set(items)
		return nil
	}
	if body.Identity == "" {
		return errBadRequest(c.kind + "s/overlay needs an identity")
	}
	if body.Clear {
		if !c.clearOverlay(body.Identity) {
			return errBadRequest(fmt.Sprintf("%q has no %s overlay to clear", body.Identity, c.kind))
		}
		return nil
	}
	if err := c.validate(items); err != nil {
		return errBadRequest(err.Error())
	}
	c.setOverlay(body.Identity, items)
	return nil
}

// reader names the caller in the content prompts/get and resources/read
// return. A caller with no identity is the anonymous one.
func reader(identity string) string {
	if identity == "" {
		return "anonymous"
	}
	return identity
}

// offersResources reports whether the server advertises resources, which
// covers both resources/list and resources/templates/list.
func (s *Server) offersResources() bool {
	return s.resources.holdsAny() || s.templates.holdsAny()
}

// notOffered is the answer to a method of a kind the server does not
// advertise, the same one a server without it would give.
func notOffered(req *request, kind string) handleResult {
	return handleResult{Response: newError(req.ID, CodeMethodNotFound,
		fmt.Sprintf("method %q is not available: this server offers no %s", req.Method, kind))}
}

// listFault answers a list method with list_fail or malformed, when either is
// set, and reports whether it did. Every list method goes through it, so a
// discovery that lists tools, prompts and resources fails the same way on
// each.
func (s *Server) listFault(req *request, f Faults) (handleResult, bool) {
	switch {
	case f.ListFail:
		s.counters.faulted()
		return handleResult{Response: newError(req.ID, CodeInternalError, f.text(req.Method+" failed (injected)"))}, true
	case f.Malformed:
		s.counters.faulted()
		return handleResult{Malformed: true}, true
	}
	return handleResult{}, false
}

func (s *Server) handlePromptsList(req *request, identity string, f Faults) handleResult {
	if !s.prompts.holdsAny() {
		return notOffered(req, "prompts")
	}
	if res, failed := s.listFault(req, f); failed {
		return res
	}
	return handleResult{Response: newResult(req.ID, map[string][]Prompt{"prompts": s.prompts.list(identity)})}
}

func (s *Server) handleResourcesList(req *request, identity string, f Faults) handleResult {
	if !s.offersResources() {
		return notOffered(req, "resources")
	}
	if res, failed := s.listFault(req, f); failed {
		return res
	}
	return handleResult{Response: newResult(req.ID, map[string][]Resource{"resources": s.resources.list(identity)})}
}

func (s *Server) handleTemplatesList(req *request, identity string, f Faults) handleResult {
	if !s.offersResources() {
		return notOffered(req, "resources")
	}
	if res, failed := s.listFault(req, f); failed {
		return res
	}
	return handleResult{Response: newResult(req.ID, map[string][]ResourceTemplate{"resourceTemplates": s.templates.list(identity)})}
}

type promptMessage struct {
	Role    string       `json:"role"`
	Content contentBlock `json:"content"`
}

type getPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []promptMessage `json:"messages"`
}

// handlePromptsGet answers with one user message reading
// "<name>:<arguments> as <identity>", the arguments as sent, so the reply
// shows which prompt ran, with what, for whom.
func (s *Server) handlePromptsGet(req *request, identity string) handleResult {
	if !s.prompts.holdsAny() {
		return notOffered(req, "prompts")
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return handleResult{Response: newError(req.ID, CodeInvalidParams, "invalid prompts/get params")}
	}
	p, ok := s.prompts.lookup(identity, params.Name)
	if !ok {
		return handleResult{Response: newError(req.ID, CodeInvalidParams, fmt.Sprintf("unknown prompt %q", params.Name))}
	}
	text := fmt.Sprintf("%s:%s as %s", p.Name, string(params.Arguments), reader(identity))
	return handleResult{Response: newResult(req.ID, getPromptResult{
		Description: p.Description,
		Messages:    []promptMessage{{Role: "user", Content: contentBlock{Type: "text", Text: text}}},
	})}
}

type resourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

type readResourceResult struct {
	Contents []resourceContents `json:"contents"`
}

// handleResourcesRead serves a uri in the caller's view: a listed resource
// first, then the first of the caller's templates, in listing order, that
// matches it. A listed resource reads "<uri> as <identity>"; a template match
// reads "<uri> from <uriTemplate> <variables> as <identity>", the variables
// percent-decoded as a JSON object, so a test can check the expansion too.
func (s *Server) handleResourcesRead(req *request, identity string) handleResult {
	if !s.offersResources() {
		return notOffered(req, "resources")
	}
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return handleResult{Response: newError(req.ID, CodeInvalidParams, "invalid resources/read params")}
	}
	who := reader(identity)
	read := func(mime, text string) handleResult {
		if mime == "" {
			mime = "text/plain"
		}
		return handleResult{Response: newResult(req.ID, readResourceResult{
			Contents: []resourceContents{{URI: params.URI, MIMEType: mime, Text: text}},
		})}
	}
	if r, ok := s.resources.lookup(identity, params.URI); ok {
		return read(r.MIMEType, fmt.Sprintf("%s as %s", params.URI, who))
	}
	for _, t := range s.templates.list(identity) {
		compiled, err := compileURITemplate(t.URITemplate)
		if err != nil {
			continue
		}
		if vars, ok := compiled.match(params.URI); ok {
			return read(t.MIMEType, fmt.Sprintf("%s from %s %s as %s", params.URI, t.URITemplate, plainJSON(vars), who))
		}
	}
	return handleResult{Response: newError(req.ID, CodeResourceNotFound, fmt.Sprintf("resource %q not found", params.URI))}
}

// plainJSON encodes v without escaping characters a uri legitimately holds.
func plainJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimSpace(b.String())
}

// uriTemplate is a compiled RFC 6570 template of literals, {var} and {+var}.
type uriTemplate struct {
	re   *regexp.Regexp
	vars []string
}

// templateVarName is an RFC 6570 varname without percent-encoding.
var templateVarName = regexp.MustCompile(`^[A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*$`)

// The characters each expression may expand to: unreserved for {var}, and
// reserved as well for {+var}, which is what lets {+path} cross a '/'.
const (
	simpleExpansion   = `(?:[A-Za-z0-9\-._~]|%[0-9A-Fa-f]{2})*`
	reservedExpansion = `(?:[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=]|%[0-9A-Fa-f]{2})*`
)

// compileURITemplate turns a template into an anchored matcher.
func compileURITemplate(raw string) (*uriTemplate, error) {
	var b strings.Builder
	var vars []string
	b.WriteString("^")
	rest := raw
	for rest != "" {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		if rest[i] == '}' {
			return nil, fmt.Errorf("'}' without a '{'")
		}
		b.WriteString(regexp.QuoteMeta(rest[:i]))
		end := strings.IndexByte(rest[i:], '}')
		if end < 0 {
			return nil, fmt.Errorf("expression %q is not closed", rest[i:])
		}
		expr := rest[i+1 : i+end]
		expansion := simpleExpansion
		name := expr
		if strings.HasPrefix(expr, "+") {
			expansion, name = reservedExpansion, expr[1:]
		}
		if !templateVarName.MatchString(name) {
			return nil, fmt.Errorf("expression {%s} is not supported: only {var} and {+var} are", expr)
		}
		vars = append(vars, name)
		b.WriteString("(" + expansion + ")")
		rest = rest[i+end+1:]
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	return &uriTemplate{re: re, vars: vars}, nil
}

// match reports whether uri is one the template expands to, with the value of
// each variable percent-decoded.
func (u *uriTemplate) match(uri string) (map[string]string, bool) {
	m := u.re.FindStringSubmatch(uri)
	if m == nil {
		return nil, false
	}
	vars := make(map[string]string, len(u.vars))
	for i, name := range u.vars {
		value, err := url.PathUnescape(m[i+1])
		if err != nil {
			value = m[i+1]
		}
		vars[name] = value
	}
	return vars, true
}
