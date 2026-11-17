// Package pocketbase adapts the neutral github.com/compdani/pocketbase-mcp Server to stock PocketBase.
package pocketbase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/search"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/auth/stockoauth"

	pbmcp "github.com/compdani/pocketbase-mcp"
)

const defaultLimit, maxLimit, summaryMaxChars = 20, 50, 200

var reservedArgs = []string{"query", "limit", "page", "sort", "collection", "filters", "id"}

// Adapter implements pbmcp.Host against a stock PocketBase app. Per-server state only.
type Adapter struct {
	app core.App
	cfg pbmcp.Config

	mu        sync.RWMutex
	configs   map[string]*pbmcp.CollectionConfig
	order     []string
	stableIDs map[string]string // manifest collection name -> resolved collection ID

	routerMu sync.Mutex
	rtr      *router.Router[*core.RequestEvent]
	handler  http.Handler

	plan     pbmcp.OAuthPlan
	provider *stockoauth.Provider
}

// Server wraps the neutral server plus its adapter.
type Server struct {
	*pbmcp.Server
	Adapter *Adapter
}

// New validates the manifest and builds the registry; it binds collection-change hooks (per server).
func New(app core.App, cfg pbmcp.Config) (*Server, error) {
	manifest := cfg.EffectiveManifest()
	customNames := []string{}
	for _, ct := range cfg.CustomTools {
		customNames = append(customNames, ct.Name)
	}
	if err := manifest.ValidateStructural(cfg.Policies, customNames); err != nil {
		return nil, err
	}
	mount := cfg.Path
	if mount == "" {
		mount = "/mcp"
	}
	key := fmt.Sprintf("%p|%s", app, mount)
	if _, loaded := oauthMountGuard.Load(key); cfg.OAuthEnabled() && loaded {
		return nil, fmt.Errorf("duplicate MCP registration for app and mount %q", mount)
	}
	success := false
	defer func() {
		if !success {
			oauthMountGuard.Delete(key)
		}
	}()
	a := &Adapter{app: app, cfg: cfg, configs: map[string]*pbmcp.CollectionConfig{}, stableIDs: map[string]string{}}
	if err := a.rebuild(); err != nil {
		return nil, err
	}
	if err := a.initOAuth(); err != nil {
		return nil, err
	}
	if a.plan.Enabled && len(cfg.Auth.Collections) == 0 {
		cfg.Auth.Collections = []string{a.plan.CollectionID}
		a.cfg = cfg
	}
	rebuild := func(e *core.CollectionEvent) error {
		err := e.Next()
		if rerr := a.rebuild(); rerr != nil {
			a.app.Logger().Warn("MCP: keeping previous manifest snapshot after failed revalidation", "error", rerr)
		}
		return err
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: "pbmcp:" + mount + ":schema:create", Func: rebuild})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: "pbmcp:" + mount + ":schema:update", Func: rebuild})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: "pbmcp:" + mount + ":schema:delete", Func: rebuild})
	srv := pbmcp.New(cfg, a)
	if cfg.OAuthEnabled() {
		oauthMountGuard.Store(key, true)
	}
	success = true
	return &Server{Server: srv, Adapter: a}, nil
}

// RegisterRoutes mounts the MCP endpoint(s) on the PB router. Call during OnServe.
func (s *Server) RegisterRoutes(r *router.Router[*core.RequestEvent]) error {
	if err := s.Adapter.registerOAuthRoutes(r); err != nil {
		return err
	}
	s.Adapter.setRouter(r)
	for _, p := range s.Paths() {
		for _, path := range []string{p, p + "/"} {
			r.POST(path, s.handle)
			r.GET(path, s.handle)
			r.DELETE(path, s.handle)
		}
	}
	return nil
}

func (s *Server) handle(e *core.RequestEvent) error {
	if e.Auth == nil {
		cfg := s.Config()
		if cfg.OAuthEnabled() {
			base := cfg.PublicBaseFor(e.Request)
			e.Response.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+pbmcp.ProtectedResourcePath(cfg.Path)+`"`)
		}
		return e.JSON(http.StatusUnauthorized, map[string]any{"message": "Unauthorized"})
	}
	p := principalFromRecord(e.Auth)
	p.Token = callerToken(e.Request)
	rec := httptest.NewRecorder()
	s.ServeWithPrincipal(rec, e.Request, p)
	for k, vs := range rec.Header() {
		for _, v := range vs {
			e.Response.Header().Set(k, v)
		}
	}
	e.Response.WriteHeader(rec.Code)
	_, err := e.Response.Write(rec.Body.Bytes())
	return err
}

func (a *Adapter) setRouter(r *router.Router[*core.RequestEvent]) {
	a.routerMu.Lock()
	defer a.routerMu.Unlock()
	a.rtr = r
	a.handler = nil
}

func (a *Adapter) apiHandler() (http.Handler, error) {
	a.routerMu.Lock()
	defer a.routerMu.Unlock()
	if a.handler != nil {
		return a.handler, nil
	}
	if a.rtr == nil {
		return nil, fmt.Errorf("router not registered")
	}
	mux, err := a.rtr.BuildMux()
	if err != nil {
		return nil, err
	}
	a.handler = mux
	return mux, nil
}

func principalFromRecord(rec *core.Record) pbmcp.Principal {
	p := pbmcp.Principal{ID: rec.Id, CollectionName: rec.Collection().Name, CollectionID: rec.Collection().Id, RecordValues: recordValues(rec)}
	if rec.Collection().Name == core.CollectionNameSuperusers || rec.IsSuperuser() {
		p.IsSuperuser = true
	}
	return p
}

func recordValues(rec *core.Record) map[string][]string {
	out := map[string][]string{}
	if rec == nil || rec.Collection() == nil {
		return out
	}
	for _, f := range rec.Collection().Fields {
		if f == nil || f.GetHidden() {
			continue
		}
		name := f.GetName()
		switch f.Type() {
		case core.FieldTypePassword, core.FieldTypeFile:
			continue
		}
		switch v := rec.Get(name).(type) {
		case string:
			if v != "" {
				out[name] = []string{v}
			}
		case []string:
			if len(v) > 0 {
				out[name] = append([]string(nil), v...)
			}
		case []any:
			vals := []string{}
			for _, e := range v {
				if str, ok := e.(string); ok && str != "" {
					vals = append(vals, str)
				}
			}
			if len(vals) > 0 {
				out[name] = vals
			}
		case bool:
			out[name] = []string{strconv.FormatBool(v)}
		case float64:
			out[name] = []string{strconv.FormatFloat(v, 'f', -1, 64)}
		}
	}
	return out
}

// --- Host implementation ---

func (a *Adapter) principalFor(p pbmcp.Principal) *core.Record {
	rec, err := a.app.FindRecordById(p.CollectionName, p.ID)
	if err != nil {
		return nil
	}
	return rec
}

func requestInfoFor(rec *core.Record) *core.RequestInfo {
	info := &core.RequestInfo{Method: http.MethodPost, Context: core.RequestInfoContextDefault}
	if rec != nil {
		info.Auth = rec
	}
	return info
}

func (a *Adapter) Authenticate(token string) (pbmcp.Principal, error) {
	rec, err := a.app.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil || rec == nil {
		return pbmcp.Principal{}, errors.New("invalid token")
	}
	return principalFromRecord(rec), nil
}

func (a *Adapter) LookupCollection(nameOrAlias string) (*pbmcp.CollectionConfig, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if c, ok := a.configs[nameOrAlias]; ok {
		return c, true
	}
	for _, c := range a.configs {
		if c.Name == nameOrAlias || c.Alias == nameOrAlias {
			return c, true
		}
	}
	return nil, false
}

func (a *Adapter) AllCollections() []*pbmcp.CollectionConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*pbmcp.CollectionConfig, 0, len(a.order))
	for _, n := range a.order {
		if c, ok := a.configs[n]; ok {
			out = append(out, c)
		}
	}
	return out
}

func (a *Adapter) collectionVisible(_ *core.Collection) bool { return true }

func (a *Adapter) VisibleCollections(p pbmcp.Principal) []*pbmcp.CollectionConfig {
	out := []*pbmcp.CollectionConfig{}
	for _, cfg := range a.AllCollections() {
		col, err := a.app.FindCachedCollectionByNameOrId(cfg.Name)
		if err != nil || !a.collectionVisible(col) {
			continue
		}
		if !p.IsSuperuser && col.ListRule == nil && col.ViewRule == nil {
			continue
		}
		out = append(out, cfg)
	}
	return out
}

func (a *Adapter) rebuild() error {
	manifest := a.cfg.EffectiveManifest()
	configs := map[string]*pbmcp.CollectionConfig{}
	order := []string{}
	newIDs := map[string]string{}
	a.mu.RLock()
	prevIDs := make(map[string]string, len(a.stableIDs))
	for k, v := range a.stableIDs {
		prevIDs[k] = v
	}
	a.mu.RUnlock()
	resolve := func(nameOrID string) (pbmcp.SchemaCollection, bool) {
		col, err := a.app.FindCollectionByNameOrId(nameOrID)
		if err != nil || col == nil {
			return pbmcp.SchemaCollection{}, false
		}
		return a.schemaFor(col), true
	}
	for _, entry := range manifest.Collections {
		col, err := a.app.FindCollectionByNameOrId(entry.Collection)
		if err != nil || col == nil {
			if entry.Optional {
				a.app.Logger().Warn("MCP: optional collection unavailable, skipping", "collection", entry.Collection)
				continue
			}
			return fmt.Errorf("manifest: collection %q not found", entry.Collection)
		}
		if prev, ok := prevIDs[entry.Collection]; ok && prev != col.Id {
			return fmt.Errorf("manifest: collection %q now resolves to a different collection ID (%s -> %s); refusing to silently redirect", entry.Collection, prev, col.Id)
		}
		built, err := pbmcp.BuildCollectionConfig(entry, a.schemaFor(col), resolve)
		if err != nil {
			if entry.Optional {
				// optional only covers missing collections; schema errors on existing collections are fatal per plan
				return err
			}
			return err
		}
		if _, dup := configs[built.Name]; dup {
			return fmt.Errorf("manifest: duplicate collection %q in snapshot", built.Name)
		}
		configs[built.Name] = built
		order = append(order, built.Name)
		newIDs[entry.Collection] = col.Id
		// also record by resolved name for rename detection on the entry key
		_ = newIDs
	}
	a.mu.Lock()
	a.configs, a.order = configs, order
	// preserve prior stable IDs for entries no longer resolvable? replace with new set merged with prev for missing optional
	merged := make(map[string]string, len(prevIDs)+len(newIDs))
	for k, v := range prevIDs {
		merged[k] = v
	}
	for k, v := range newIDs {
		merged[k] = v
	}
	a.stableIDs = merged
	a.mu.Unlock()
	return nil
}

// Refresh rebuilds the registry (called by hooks; exported for tests). A failed rebuild keeps the previous snapshot.
func (a *Adapter) Refresh() {
	if err := a.rebuild(); err != nil {
		a.app.Logger().Warn("MCP: keeping previous manifest snapshot after failed revalidation", "error", err)
	}
}

func (a *Adapter) schemaFor(collection *core.Collection) pbmcp.SchemaCollection {
	meta := pbmcp.SchemaCollection{
		Name: collection.Name, ID: collection.Id, Visible: a.collectionVisible(collection),
		ListRule: pbmcp.RuleStateFromPtr(collection.ListRule), ViewRule: pbmcp.RuleStateFromPtr(collection.ViewRule),
		CreateRule: pbmcp.RuleStateFromPtr(collection.CreateRule), UpdateRule: pbmcp.RuleStateFromPtr(collection.UpdateRule), DeleteRule: pbmcp.RuleStateFromPtr(collection.DeleteRule),
	}
	for _, field := range collection.Fields {
		if field == nil {
			continue
		}
		name := field.GetName()
		info := pbmcp.FieldInfo{Name: name, Type: field.Type(), Hidden: field.GetHidden()}
		switch f := field.(type) {
		case *core.SelectField:
			info.Values = append([]string(nil), f.Values...)
			info.Multiple = f.IsMultiple()
		case *core.RelationField:
			info.Multiple = f.IsMultiple()
			if rel, err := a.app.FindCachedCollectionByNameOrId(f.CollectionId); err == nil {
				info.RelCollection = rel.Name
			} else {
				info.RelCollection = f.CollectionId
			}
		case *core.FileField:
			info.Multiple = f.IsMultiple()
		}
		meta.Fields = append(meta.Fields, info)
	}
	return meta
}

// --- search / fetch (ported from customjs search.go) ---

func filterArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		if !slices.Contains(reservedArgs, k) {
			out[k] = v
		}
	}
	if nested, ok := args["filters"].(map[string]any); ok {
		maps.Copy(out, nested)
	}
	return out
}

func buildSearchFilter(cfg *pbmcp.CollectionConfig, query string, args map[string]any) (string, dbx.Params, error) {
	clauses := []string{}
	params := dbx.Params{}
	if query = strings.TrimSpace(query); query != "" && len(cfg.SearchFields) > 0 {
		parts := make([]string, 0, len(cfg.SearchFields))
		for _, field := range cfg.SearchFields {
			parts = append(parts, field+" ~ {:q}")
		}
		clauses = append(clauses, "("+strings.Join(parts, " || ")+")")
		params["q"] = query
	}
	specs := map[string]pbmcp.ResolvedFilter{}
	for _, sp := range pbmcp.FiltersFor(cfg) {
		specs[sp.Argument] = sp
	}
	keys := slices.Sorted(maps.Keys(args))
	var unknown []string
	paramIdx := 0
	nextParam := func() string { paramIdx++; return "f" + strconv.Itoa(paramIdx) }
	for _, key := range keys {
		value := args[key]
		if isEmptyFilterValue(value) {
			continue
		}
		spec, ok := specs[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		if spec.Operator == "in" {
			vals, err := convertInValues(spec, value)
			if err != nil {
				return "", nil, fmt.Errorf("filter %s: %w", key, err)
			}
			if len(vals) == 0 {
				continue
			}
			parts := []string{}
			for _, v := range vals {
				pn := nextParam()
				params[pn] = v
				op := "="
				if spec.Info.Multiple {
					op = "?="
				}
				parts = append(parts, spec.Field+" "+op+" {:"+pn+"}")
			}
			clauses = append(clauses, "("+strings.Join(parts, " || ")+")")
			continue
		}
		bound, err := convertFilterValue(spec, value)
		if err != nil {
			return "", nil, fmt.Errorf("filter %s: %w", key, err)
		}
		pn := nextParam()
		params[pn] = bound
		clauses = append(clauses, spec.Field+" "+filterOperator(spec)+" {:"+pn+"}")
	}
	if len(unknown) > 0 {
		valid := slices.Sorted(maps.Keys(specs))
		return "", nil, fmt.Errorf("unknown filter(s) %s; valid filters: %s", strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
	return strings.Join(clauses, " && "), params, nil
}

func filterOperator(spec pbmcp.ResolvedFilter) string {
	switch spec.Operator {
	case "contains":
		if spec.Info.Multiple {
			return "?="
		}
		return "~"
	case "eq":
		if spec.Info.Multiple {
			return "?="
		}
		return "="
	case "neq":
		return "!="
	case "gt":
		return ">"
	case "gte":
		return ">="
	case "lt":
		return "<"
	case "lte":
		return "<="
	}
	return "="
}

func convertInValues(spec pbmcp.ResolvedFilter, value any) ([]any, error) {
	var raw []any
	switch v := value.(type) {
	case []any:
		raw = v
	case string:
		// allow comma-separated or single value
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				raw = append(raw, part)
			}
		}
	default:
		raw = []any{value}
	}
	out := []any{}
	for _, e := range raw {
		// reuse scalar conversion by pretending eq
		single := spec
		single.Operator = "eq"
		b, err := convertFilterValue(single, e)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func convertFilterValue(spec pbmcp.ResolvedFilter, value any) (any, error) {
	if spec.Operator == "gte" || spec.Operator == "lte" || spec.Operator == "gt" || spec.Operator == "lt" {
		if spec.Info.Type == core.FieldTypeDate || spec.Info.Type == core.FieldTypeAutodate {
			s, ok := value.(string)
			if !ok {
				return nil, errors.New("expected a date string")
			}
			s = strings.TrimSpace(s)
			dt, err := types.ParseDateTime(s)
			if err != nil || dt.IsZero() {
				return nil, fmt.Errorf("invalid date %q", s)
			}
			if spec.Operator == "lte" && len(s) == len("2006-01-02") {
				dt = dt.Add(24*time.Hour - time.Millisecond)
			}
			return dt.String(), nil
		}
		if spec.Info.Type == core.FieldTypeNumber {
			return toNumber(value)
		}
	}
	switch spec.Info.Type {
	case core.FieldTypeNumber:
		return toNumber(value)
	case core.FieldTypeBool:
		switch v := value.(type) {
		case bool:
			return v, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return nil, errors.New("expected true or false")
			}
			return b, nil
		}
		return nil, errors.New("expected true or false")
	}
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if len(spec.Info.Values) > 0 && !slices.Contains(spec.Info.Values, v) {
			return nil, fmt.Errorf("must be one of %s", strings.Join(spec.Info.Values, ", "))
		}
		return v, nil
	case float64, bool:
		if spec.Operator == "contains" {
			return fmt.Sprint(v), nil
		}
	}
	return nil, errors.New("expected a string")
}

func toNumber(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n, nil
		}
	}
	return 0, errors.New("expected a number")
}

func isEmptyFilterValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	}
	return false
}

func parseSort(cfg *pbmcp.CollectionConfig, raw string) ([]search.SortField, error) {
	raw = strings.TrimSpace(raw)
	var parts []string
	if raw == "" {
		if len(cfg.DefaultSort) > 0 {
			parts = append(parts, cfg.DefaultSort...)
		} else if _, ok := cfg.FieldMap["updated"]; ok {
			parts = []string{"-updated"}
		} else {
			parts = []string{"-id"}
		}
	} else {
		for part := range strings.SplitSeq(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, part)
			}
		}
	}
	out := []search.SortField{}
	seen := map[string]bool{}
	for _, part := range parts {
		dir := search.SortAsc
		name := part
		if n, ok := strings.CutPrefix(part, "-"); ok {
			dir, name = search.SortDesc, n
		} else {
			name = strings.TrimPrefix(part, "+")
		}
		if !slices.Contains(cfg.SortFields, name) {
			return nil, fmt.Errorf("cannot sort by %q; allowed: %s", name, strings.Join(cfg.SortFields, ", "))
		}
		out = append(out, search.SortField{Name: name, Direction: dir})
		seen[name] = true
	}
	if !seen["id"] {
		out = append(out, search.SortField{Name: "id", Direction: search.SortAsc})
	}
	return out, nil
}

func intArg(args map[string]any, key string, fallback, minV, maxV int) int {
	n := fallback
	switch v := args[key].(type) {
	case float64:
		n = int(v)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			n = parsed
		}
	}
	return max(minV, min(n, maxV))
}

func getStringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func (a *Adapter) Search(_ context.Context, p pbmcp.Principal, cfg *pbmcp.CollectionConfig, args map[string]any, baseURL string) (any, error) {
	auth := a.principalFor(p)
	info := requestInfoFor(auth)
	collection, err := a.app.FindCachedCollectionByNameOrId(cfg.Name)
	if err != nil {
		return nil, err
	}
	if cfg.CollectionID != "" && collection.Id != cfg.CollectionID {
		return nil, pbmcp.ToolError("You are not allowed to list " + cfg.Name + " records.")
	}
	if !a.collectionVisible(collection) {
		return nil, pbmcp.ToolError("You are not allowed to list " + cfg.Name + " records.")
	}
	superuser := p.IsSuperuser
	if collection.ListRule == nil && !superuser {
		return nil, pbmcp.ToolError("You are not allowed to list " + cfg.Name + " records.")
	}
	where, params, err := buildSearchFilter(cfg, getStringArg(args, "query"), filterArgs(args))
	if err != nil {
		return nil, pbmcp.ToolError(err.Error())
	}
	sortFields, err := parseSort(cfg, getStringArg(args, "sort"))
	if err != nil {
		return nil, pbmcp.ToolError(err.Error())
	}
	defLim := cfg.DefaultLimit
	if defLim == 0 {
		defLim = defaultLimit
	}
	maxLim := cfg.MaxLimit
	if maxLim == 0 {
		maxLim = maxLimit
	}
	limit := intArg(args, "limit", defLim, 1, maxLim)
	page := intArg(args, "page", 1, 1, 1<<20)
	query := a.app.RecordQuery(collection)
	resolver := core.NewRecordFieldResolver(a.app, collection, info, true)
	if !superuser && collection.ListRule != nil && *collection.ListRule != "" {
		expr, err := search.FilterData(*collection.ListRule).BuildExpr(resolver)
		if err != nil {
			return nil, err
		}
		query.AndWhere(expr)
	}
	resolver.SetAllowHiddenFields(superuser)
	if where != "" {
		expr, err := search.FilterData(where).BuildExpr(resolver, params)
		if err != nil {
			return nil, err
		}
		query.AndWhere(expr)
	}
	records := []*core.Record{}
	// Dialect-safe counting: "id" exists on every dialect (replaces CountCol("_rowid_")).
	result, err := search.NewProvider(resolver).Query(query).CountCol("id").Page(page).PerPage(limit).Sort(sortFields).Exec(&records)
	if err != nil {
		return nil, err
	}
	if len(cfg.Expand) > 0 && len(records) > 0 {
		a.app.ExpandRecords(records, cfg.Expand, a.accessCheckedExpandFetch(info))
	}
	results := make([]map[string]any, 0, len(records))
	for _, record := range records {
		item := map[string]any{"id": record.Id, "collection": cfg.Name, "title": a.recordTitle(record, cfg), "url": recordURL(baseURL, record, cfg), "fields": summaryFields(record, cfg)}
		if updated := record.GetString("updated"); updated != "" {
			item["updated"] = updated
		}
		if related := a.relatedSummaries(record, cfg); len(related) > 0 {
			item["related"] = related
		}
		results = append(results, item)
	}
	return map[string]any{"collection": cfg.Name, "results": results, "page": result.Page, "perPage": result.PerPage, "totalItems": result.TotalItems, "totalPages": result.TotalPages, "hasMore": result.Page < result.TotalPages}, nil
}

func (a *Adapter) Fetch(_ context.Context, p pbmcp.Principal, cfg *pbmcp.CollectionConfig, args map[string]any, baseURL string) (any, error) {
	recordID := strings.TrimSpace(getStringArg(args, "id"))
	if recordID == "" {
		return nil, pbmcp.ToolError("Missing id.")
	}
	auth := a.principalFor(p)
	info := requestInfoFor(auth)
	notFound := "No " + cfg.Name + " record with id " + strconv.Quote(recordID) + " (or you do not have access to it)."
	collection, err := a.app.FindCachedCollectionByNameOrId(cfg.Name)
	if err != nil {
		return nil, err
	}
	if cfg.CollectionID != "" && collection.Id != cfg.CollectionID {
		return nil, pbmcp.ToolError(notFound)
	}
	if !a.collectionVisible(collection) {
		return nil, pbmcp.ToolError(notFound)
	}
	record, err := a.app.FindRecordById(cfg.Name, recordID)
	if err != nil {
		return nil, pbmcp.ToolError(notFound)
	}
	canView, err := a.app.CanAccessRecord(record, info, record.Collection().ViewRule)
	if err != nil {
		return nil, err
	}
	if !canView {
		return nil, pbmcp.ToolError(notFound)
	}
	if len(cfg.Expand) > 0 {
		a.app.ExpandRecord(record, cfg.Expand, a.accessCheckedExpandFetch(info))
	}
	recMap := a.projectRecord(record, cfg)
	return map[string]any{"id": record.Id, "collection": cfg.Name, "title": a.recordTitle(record, cfg), "url": recordURL(baseURL, record, cfg), "record": recMap}, nil
}

func (a *Adapter) accessCheckedExpandFetch(info *core.RequestInfo) core.ExpandFetchFunc {
	expandInfo := *info
	expandInfo.Context = core.RequestInfoContextExpand
	return func(relCollection *core.Collection, relIds []string) ([]*core.Record, error) {
		return a.app.FindRecordsByIds(relCollection.Id, relIds, func(q *dbx.SelectQuery) error {
			if !a.collectionVisible(relCollection) {
				return fmt.Errorf("only superusers can view %q records", relCollection.Name)
			}
			if expandInfo.HasSuperuserAuth() {
				return nil
			}
			if relCollection.ViewRule == nil {
				return fmt.Errorf("only superusers can view %q records", relCollection.Name)
			}
			if *relCollection.ViewRule == "" {
				return nil
			}
			resolver := core.NewRecordFieldResolver(a.app, relCollection, &expandInfo, true)
			expr, err := search.FilterData(*relCollection.ViewRule).BuildExpr(resolver)
			if err != nil {
				return err
			}
			q.AndWhere(expr)
			return resolver.UpdateQuery(q)
		})
	}
}

// --- write via subrequest (ported from customjs write.go) ---

func (a *Adapter) Write(ctx context.Context, p pbmcp.Principal, operation string, cfg *pbmcp.CollectionConfig, args map[string]any, baseURL string) (any, error) {
	recordID := strings.TrimSpace(getStringArg(args, "id"))
	if operation != "create" && recordID == "" {
		return nil, pbmcp.ToolError("Missing id.")
	}
	var data map[string]any
	if operation != "delete" {
		var ok bool
		data, ok = args["data"].(map[string]any)
		if !ok || len(data) == 0 {
			return nil, pbmcp.ToolError("Missing data: provide an object of field values.")
		}
		if err := validateWriteData(cfg, operation, data); err != nil {
			return nil, pbmcp.ToolError(err.Error())
		}
	}
	// fail closed if collection was replaced/renamed
	if col, err := a.app.FindCachedCollectionByNameOrId(cfg.Name); err != nil || (cfg.CollectionID != "" && col.Id != cfg.CollectionID) || !a.collectionVisible(col) {
		return nil, pbmcp.ToolError("Unknown or unavailable collection.")
	}
	path := "/api/collections/" + url.PathEscape(cfg.Name) + "/records"
	method := http.MethodPost
	switch operation {
	case "update":
		path += "/" + url.PathEscape(recordID)
		method = http.MethodPatch
	case "delete":
		path += "/" + url.PathEscape(recordID)
		method = http.MethodDelete
	}
	if p.Token == "" {
		return nil, pbmcp.ToolError("Not authenticated.")
	}
	status, body, err := a.apiSubrequest(ctx, method, path, data, p.Token)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, pbmcp.ToolError(describeAPIError(status, body))
	}
	if operation == "delete" {
		return map[string]any{"deleted": true, "id": recordID, "collection": cfg.Name}, nil
	}
	var exported map[string]any
	if err := json.Unmarshal(body, &exported); err != nil {
		return nil, err
	}
	savedID, _ := exported["id"].(string)
	result := map[string]any{"id": savedID, "collection": cfg.Name, "title": cfg.Name + ":" + savedID, "record": projectMap(exported, cfg.ReadFields)}
	if record, err := a.app.FindRecordById(cfg.Name, savedID); err == nil {
		if len(cfg.Expand) > 0 {
			info := requestInfoFor(a.principalFor(p))
			a.app.ExpandRecord(record, cfg.Expand, a.accessCheckedExpandFetch(info))
		}
		result["title"] = a.recordTitle(record, cfg)
		result["url"] = recordURL(baseURL, record, cfg)
		result["record"] = a.projectRecord(record, cfg)
	}
	return result, nil
}

func validateWriteData(cfg *pbmcp.CollectionConfig, operation string, data map[string]any) error {
	allowed := cfg.CreateFields
	if operation == "update" {
		allowed = cfg.UpdateFields
	}
	if allowed == nil {
		// legacy hand-built configs: fall back to writable fields
		for _, f := range cfg.Fields {
			if pbmcp.IsWritableField(f) {
				allowed = append(allowed, f.Name)
			}
		}
	}
	allowedSet := map[string]bool{}
	for _, n := range allowed {
		allowedSet[n] = true
	}
	var bad []string
	for key := range data {
		if strings.HasSuffix(key, "+") || strings.HasSuffix(key, "-") {
			bad = append(bad, key+" (field modifiers are not allowed)")
			continue
		}
		if !allowedSet[key] {
			bad = append(bad, key)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("cannot set %s on %s; writable fields: %s", strings.Join(bad, ", "), cfg.Name, strings.Join(allowed, ", "))
}

func projectMap(src map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	if len(fields) == 0 {
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	for _, n := range fields {
		if v, ok := src[n]; ok {
			out[n] = v
		}
	}
	return out
}

func (a *Adapter) projectRecord(record *core.Record, cfg *pbmcp.CollectionConfig) map[string]any {
	src := record.PublicExport()
	out := projectMap(src, cfg.ReadFields)
	// expanded relations: only configured relation fields
	if len(cfg.Expand) > 0 {
		expand := map[string]any{}
		for _, name := range cfg.Expand {
			relSpec, hasSpec := cfg.Relations[name]
			fields := []string{"id"}
			if hasSpec && len(relSpec.Fields) > 0 {
				fields = relSpec.Fields
			}
			if one := record.ExpandedOne(name); one != nil {
				expand[name] = projectMap(one.PublicExport(), fields)
			} else if many := record.ExpandedAll(name); len(many) > 0 {
				arr := []any{}
				for _, rel := range many {
					arr = append(arr, projectMap(rel.PublicExport(), fields))
				}
				expand[name] = arr
			}
		}
		if len(expand) > 0 {
			out["expand"] = expand
		}
	}
	return out
}

// isRecordEndpoint allows only /api/collections/{c}/records[/{id}] with no
// dot segments, so subrequests can never reach /mcp, OAuth or other routes.
func isRecordEndpoint(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/collections/")
	if !ok || strings.ContainsAny(path, "?#") {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "records" {
		return false
	}
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "%") {
			return false
		}
	}
	return true
}

// callerToken returns the raw PB token from the Authorization header.
func callerToken(r *http.Request) string {
	t := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(t) > 7 && strings.EqualFold(t[:7], "Bearer ") {
		return strings.TrimSpace(t[7:])
	}
	return t
}

func (a *Adapter) apiSubrequest(ctx context.Context, method, path string, data map[string]any, token string) (int, []byte, error) {
	if !isRecordEndpoint(path) {
		return 0, nil, fmt.Errorf("subrequest path not allowed")
	}
	handler, err := a.apiHandler()
	if err != nil {
		return 0, nil, err
	}
	var body bytes.Buffer
	if data != nil {
		if err := json.NewEncoder(&body).Encode(data); err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, path, &body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

func describeAPIError(status int, body []byte) string {
	var apiErr struct {
		Message string                    `json:"message"`
		Data    map[string]map[string]any `json:"data"`
	}
	_ = json.Unmarshal(body, &apiErr)
	msg := strings.TrimSpace(apiErr.Message)
	switch status {
	case http.StatusNotFound:
		msg = "Record not found (or you do not have access to it)."
	case http.StatusForbidden, http.StatusUnauthorized:
		if msg == "" {
			msg = "Not allowed by the collection rules."
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(apiErr.Data) > 0 {
		fields := make([]string, 0, len(apiErr.Data))
		for field, detail := range apiErr.Data {
			fields = append(fields, fmt.Sprintf("%s: %v", field, detail["message"]))
		}
		sort.Strings(fields)
		msg += " Field errors: " + strings.Join(fields, "; ") + "."
	}
	return fmt.Sprintf("PocketBase rejected the request (HTTP %d): %s", status, msg)
}

// --- presentation helpers ---

func (a *Adapter) recordTitle(record *core.Record, cfg *pbmcp.CollectionConfig) string {
	sep := cfg.TitleSep
	if sep == "" {
		sep = " "
	}
	parts := make([]string, 0, len(cfg.TitleFields))
next:
	for _, field := range cfg.TitleFields {
		value := strings.TrimSpace(record.GetString(field))
		if value == "" {
			continue
		}
		for i, existing := range parts {
			if strings.Contains(strings.ToLower(existing), strings.ToLower(value)) {
				continue next
			}
			if strings.Contains(strings.ToLower(value), strings.ToLower(existing)) {
				parts[i] = value
				continue next
			}
		}
		parts = append(parts, value)
	}
	// Append expanded relation titles (e.g. inquiries -> contact name),
	// mirroring customjs's inquiryTitle without a per-collection func.
	for _, expName := range cfg.Expand {
		if rel := record.ExpandedOne(expName); rel != nil {
			if relCfg, ok := a.LookupCollection(rel.Collection().Name); ok {
				relFields := []string{}
				if spec, has := cfg.Relations[expName]; has {
					relFields = spec.Fields
				}
				// only use relation title fields that are exposed via the relation projection
				subParts := []string{}
				for _, tf := range relCfg.TitleFields {
					if len(relFields) == 0 || slices.Contains(relFields, tf) {
						if v := strings.TrimSpace(rel.GetString(tf)); v != "" {
							subParts = append(subParts, v)
						}
					}
				}
				if rt := strings.Join(subParts, " "); rt != "" && !strings.Contains(strings.Join(parts, " "), rt) {
					parts = append(parts, rt)
				}
			}
		}
	}
	title := strings.Join(parts, sep)
	if title == "" {
		return cfg.Name + ":" + record.Id
	}
	return title
}

func recordURL(baseURL string, record *core.Record, cfg *pbmcp.CollectionConfig) string {
	if cfg.URLField != "" {
		if c := strings.TrimSpace(record.GetString(cfg.URLField)); c != "" {
			return c
		}
	}
	return strings.TrimRight(baseURL, "/") + "/api/collections/" + cfg.Name + "/records/" + record.Id
}

func summaryFields(record *core.Record, cfg *pbmcp.CollectionConfig) map[string]any {
	out := map[string]any{}
	for _, name := range cfg.SummaryFields {
		if len(cfg.ReadFields) > 0 && !slices.Contains(cfg.ReadFields, name) {
			continue
		}
		if _, ok := cfg.FieldMap[name]; !ok {
			continue
		}
		switch v := record.Get(name).(type) {
		case nil:
		case string:
			if v = strings.TrimSpace(v); v != "" {
				out[name] = truncate(v, summaryMaxChars)
			}
		case []string:
			if len(v) > 0 {
				out[name] = v
			}
		case float64:
			if v != 0 {
				out[name] = v
			}
		case bool:
			out[name] = v
		default:
			if s := record.GetString(name); s != "" {
				out[name] = truncate(s, summaryMaxChars)
			}
		}
	}
	return out
}

func (a *Adapter) relatedSummaries(record *core.Record, cfg *pbmcp.CollectionConfig) map[string]any {
	out := map[string]any{}
	for _, name := range cfg.Expand {
		rel := record.ExpandedOne(name)
		if rel == nil {
			continue
		}
		entry := map[string]any{"id": rel.Id, "collection": rel.Collection().Name}
		relSpec, hasSpec := cfg.Relations[name]
		fields := []string{"id"}
		if hasSpec && len(relSpec.Fields) > 0 {
			fields = relSpec.Fields
		}
		projected := projectMap(rel.PublicExport(), fields)
		entry["fields"] = projected
		// title from configured relation fields only
		if relCfg, ok := a.LookupCollection(rel.Collection().Name); ok {
			parts := []string{}
			for _, tf := range relCfg.TitleFields {
				if slices.Contains(fields, tf) {
					if v := strings.TrimSpace(rel.GetString(tf)); v != "" {
						parts = append(parts, v)
					}
				}
			}
			if len(parts) > 0 {
				sep := relCfg.TitleSep
				if sep == "" {
					sep = " "
				}
				entry["title"] = strings.Join(parts, sep)
			}
		}
		out[name] = entry
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
