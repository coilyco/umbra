// Collect action execution: auto-paginate a granted list leaf and emit one
// accumulated JSON array. See docs/specverb-actions.md.

package specverb

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/http/respfmt"
	"github.com/coilyco/umbra/pkg/config"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/ttlcache"
	"github.com/urfave/cli/v3"
)

func resolveCollectAction(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, a guardfile.Action) (actionDescriptor, error) {
	col := a.Collect
	inputNames := map[string]bool{}
	for _, in := range a.Inputs {
		inputNames[in.Name] = true
		if in.Default != "" {
			return actionDescriptor{}, fmt.Errorf("specverb: action %q: input %q: `default` is a poll-action pre-flight binding and is not supported on collect actions (fail-closed)", a.Name, in.Name)
		}
	}
	g, ok := granted[grantKey{Verb: col.Verb, Resource: col.Resource}]
	if !ok {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q collects %q %q which no `can` grant authorizes (deny-by-default; add `can %s %s`)", a.Name, col.Verb, col.Resource, col.Verb, col.Resource)
	}
	leaf, err := resolveDescriptor(spec, gf.Group, g)
	if err != nil {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: %w", a.Name, err)
	}
	limit, err := strconv.Atoi(col.DefaultLimit)
	if err != nil || limit <= 0 {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: default-limit %q must be a positive integer", a.Name, col.DefaultLimit)
	}
	if err := validateCollectArgs(a.Name, *col, leaf, inputNames); err != nil {
		return actionDescriptor{}, err
	}
	if a.FailWhen != "" {
		if err := respfmt.Validate(a.FailWhen); err != nil {
			return actionDescriptor{}, fmt.Errorf("specverb: action %q: fail-when: %w", a.Name, err)
		}
	}
	cacheTTL, err := parseCollectCache(a.Name, col.Cache)
	if err != nil {
		return actionDescriptor{}, err
	}
	return actionDescriptor{
		Name:          a.Name,
		VerbName:      actionVerbName(gf.Group, a),
		Describe:      a.Describe,
		Inputs:        a.Inputs,
		Collect:       &collectStep{Leaf: leaf, Args: col.Args, PageParam: col.PageParam, LimitParam: col.LimitParam, Limit: col.Limit, DefaultLimit: limit, As: col.As, CacheTTL: cacheTTL},
		FailWhen:      a.FailWhen,
		MountVerb:     a.MountVerb,
		MountResource: a.MountResource,
	}, nil
}

// parseCollectCache resolves the `cache "<ttl>"` modifier into a positive
// duration, failing closed on a bad TTL. Empty means no cache (disabled).
func parseCollectCache(action, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("specverb: action %q: cache %q is not a valid duration (e.g. \"10m\", \"1h\"): %w", action, raw, err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("specverb: action %q: cache %q must be a positive duration", action, raw)
	}
	return ttl, nil
}

func validateCollectArgs(action string, col guardfile.Collect, leaf opDescriptor, inputNames map[string]bool) error {
	queryNames := map[string]bool{}
	for _, f := range leaf.QueryFlags {
		queryNames[f.Name] = true
	}
	if !queryNames[col.PageParam] {
		return fmt.Errorf("specverb: action %q: page-param %q is not a query flag on %s %s", action, col.PageParam, leaf.Method, leaf.Path)
	}
	if !queryNames[col.LimitParam] {
		return fmt.Errorf("specverb: action %q: limit-param %q is not a query flag on %s %s", action, col.LimitParam, leaf.Method, leaf.Path)
	}
	if col.Limit != "" {
		if err := validateCollectRef(action, "limit", col.Limit, inputNames); err != nil {
			return err
		}
	}
	return validateCallArgs(action, "collect", col.Args, leaf, inputNames, nil)
}

func validateCollectRef(action, field, value string, inputNames map[string]bool) error {
	if !strings.HasPrefix(value, "$") {
		return nil
	}
	ref := strings.TrimPrefix(value, "$")
	if !inputNames[ref] {
		return fmt.Errorf("specverb: action %q: %s references $%s, which no `input` declares", action, field, ref)
	}
	return nil
}

func (rt *runtime) runCollectAction(ctx context.Context, c *cli.Command, ad actionDescriptor, strVars map[string]string, jmesVars map[string]any, outcome *cacheOutcome) error {
	limit, err := collectLimit(ad, strVars)
	if err != nil {
		return err
	}
	cache, key, cacheOn, err := rt.prepCollectCache(ctx, ad, strVars, c.Bool(flagNoCache))
	if err != nil {
		return err
	}
	if c.Bool(flagDryRun) {
		return rt.collectDryRun(ctx, ad, strVars, limit, key, cacheOn, c.String(flagOutput))
	}
	if cacheOn {
		served, serr := rt.serveCollectFromCache(cache, key, ad, jmesVars, c, outcome)
		if serr != nil || served {
			return serr
		}
	}
	all, err := rt.collectAllPages(ctx, c, ad, strVars, limit)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	if cacheOn {
		_ = cache.Set(key, raw, ad.Collect.CacheTTL) // perf hint, not load-bearing
	}
	return rt.finishCollect(ad, all, raw, jmesVars, c)
}

// prepCollectCache derives the cache handle, request-only key, and live flag
// (declared TTL and no --no-cache). The key resolves even under --no-cache.
func (rt *runtime) prepCollectCache(ctx context.Context, ad actionDescriptor, strVars map[string]string, noCache bool) (cache *ttlcache.Cache, key string, cacheOn bool, err error) {
	if !ad.cacheable() {
		return nil, "", false, nil
	}
	key, err = rt.collectCacheKey(ctx, ad.Collect, strVars)
	if err != nil {
		return nil, "", false, err
	}
	return ttlcache.New(config.CacheDir()), key, !noCache, nil
}

// collectDryRun prints the offline collect plan (page 1, never fired), with the
// cache block when the action caches.
func (rt *runtime) collectDryRun(ctx context.Context, ad actionDescriptor, strVars map[string]string, limit int, key string, cacheOn bool, output string) error {
	method, url, contentType, err := rt.buildCollectRequest(ctx, true, ad.Collect, strVars, 1, limit)
	if err != nil {
		return err
	}
	return rt.renderCollectPlan(ad, method, url, nil, contentType, limit, key, cacheOn, output)
}

// serveCollectFromCache honours --refresh (invalidate, then fetch live), then
// serves a warm entry. served=true means a hit was emitted; false means fetch.
func (rt *runtime) serveCollectFromCache(cache *ttlcache.Cache, key string, ad actionDescriptor, jmesVars map[string]any, c *cli.Command, outcome *cacheOutcome) (served bool, err error) {
	if c.Bool(flagRefresh) {
		if ierr := cache.Invalidate(key); ierr != nil {
			return false, exitcode.New(exitcode.Internal, "internal", ierr, "could not invalidate the cached collect entry for --refresh")
		}
		return false, nil
	}
	cached, ok := cache.Get(key)
	if !ok {
		return false, nil
	}
	var all []any
	if uerr := json.Unmarshal(cached, &all); uerr != nil {
		return false, exitcode.New(exitcode.Internal, "internal", uerr, "the cached collect payload was not a JSON array")
	}
	outcome.hit = true
	return true, rt.finishCollect(ad, all, cached, jmesVars, c)
}

// collectAllPages walks the paginated leaf, accumulating every array response
// until a page returns fewer than limit items (the short-page stop).
func (rt *runtime) collectAllPages(ctx context.Context, c *cli.Command, ad actionDescriptor, strVars map[string]string, limit int) ([]any, error) {
	var all []any
	for page := 1; ; page++ {
		method, url, contentType, berr := rt.buildCollectRequest(ctx, false, ad.Collect, strVars, page, limit)
		if berr != nil {
			return nil, berr
		}
		decoded, _, ferr := rt.fireCallAudited(ctx, ad.Collect.Leaf, method, url, nil, contentType, c)
		if ferr != nil {
			return nil, exitcode.New(exitcode.UpstreamFailed, "action_failed", fmt.Errorf("collect page %d (%s): %w", page, ad.Collect.Leaf.Leaf, ferr), "a page in the collection failed; the accumulated result was not emitted")
		}
		items, ok := decoded.([]any)
		if !ok {
			return nil, exitcode.New(exitcode.UpstreamFailed, "action_failed", fmt.Errorf("collect page %d: response is not a JSON array", page), "collect actions require array responses")
		}
		all = append(all, items...)
		if len(items) < limit {
			break
		}
	}
	return all, nil
}

// finishCollect binds the accumulated array, renders it, and applies fail-when.
// Shared by the live page-walk and the cache-hit path so both emit identically.
func (rt *runtime) finishCollect(ad actionDescriptor, all []any, raw []byte, jmesVars map[string]any, c *cli.Command) error {
	jmesVars[ad.Collect.As] = all
	if err := renderFinal(raw, c.String(flagQuery), c.String(flagOutput)); err != nil {
		return err
	}
	return rt.applyFailWhen(ad, raw, jmesVars)
}

// collectCacheKey derives the key from the resolved request shape: method +
// base + path + sorted non-pagination query (page/limit/auth excluded; offline).
func (rt *runtime) collectCacheKey(ctx context.Context, col *collectStep, strVars map[string]string) (string, error) {
	b := opcore.NewArgBinder(col.Leaf)
	for _, arg := range col.Args {
		val, ok := resolveCollectArgValue(arg.Value, strVars)
		if !ok {
			continue
		}
		if berr := b.Bind(arg.Name, val); berr != nil {
			return "", berr
		}
	}
	if berr := b.RequireAllPaths(); berr != nil {
		return "", berr
	}
	if rerr := rt.CheckRestrictions(col.Leaf.PathParams, b.PathVals); rerr != nil {
		return "", rerr
	}
	qs := ""
	if len(b.Query) > 0 {
		qs = "?" + b.Query.Encode()
	}
	base, berr := rt.BaseForRequest(ctx, true)
	if berr != nil {
		return "", berr
	}
	return col.Leaf.Method + " " + base + opcore.FillPath(col.Leaf.Path, b.PathVals) + qs, nil
}

func collectLimit(ad actionDescriptor, strVars map[string]string) (int, error) {
	limit := ad.Collect.DefaultLimit
	if ad.Collect.Limit != "" {
		v, ok := resolveCollectArgValue(ad.Collect.Limit, strVars)
		if !ok {
			return limit, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0, exitcode.New(exitcode.UserError, "user_error", fmt.Errorf("collect limit %q must resolve to a positive integer", v), "pass a positive --limit")
		}
		limit = n
	}
	return limit, nil
}

func (rt *runtime) buildCollectRequest(ctx context.Context, dry bool, col *collectStep, strVars map[string]string, page, limit int) (method, url, contentType string, err error) {
	b := opcore.NewArgBinder(col.Leaf)
	for _, arg := range col.Args {
		val, ok := resolveCollectArgValue(arg.Value, strVars)
		if !ok {
			continue
		}
		if berr := b.Bind(arg.Name, val); berr != nil {
			return "", "", "", berr
		}
	}
	if berr := b.Bind(col.PageParam, strconv.Itoa(page)); berr != nil {
		return "", "", "", berr
	}
	if berr := b.Bind(col.LimitParam, strconv.Itoa(limit)); berr != nil {
		return "", "", "", berr
	}
	if berr := b.RequireAllPaths(); berr != nil {
		return "", "", "", berr
	}
	if rerr := rt.CheckRestrictions(col.Leaf.PathParams, b.PathVals); rerr != nil {
		return "", "", "", rerr
	}
	qs := ""
	if len(b.Query) > 0 {
		qs = "?" + b.Query.Encode()
	}
	base, berr := rt.BaseForRequest(ctx, dry)
	if berr != nil {
		return "", "", "", berr
	}
	url = base + opcore.FillPath(col.Leaf.Path, b.PathVals) + qs
	return col.Leaf.Method, url, contentTypeJSON, nil
}

func resolveCollectArgValue(value string, strVars map[string]string) (resolved string, ok bool) {
	if !strings.HasPrefix(value, "$") {
		return value, true
	}
	ref := strings.TrimPrefix(value, "$")
	v, ok := strVars[ref]
	if !ok {
		return "", false
	}
	return v, true
}

func (rt *runtime) renderCollectPlan(ad actionDescriptor, method, url string, body []byte, contentType string, limit int, key string, cacheOn bool, output string) error {
	collect := map[string]any{
		"method":      method,
		"url":         rt.previewURL(url),
		"headers":     rt.previewHeaders(body != nil, contentType),
		"page_param":  ad.Collect.PageParam,
		"limit_param": ad.Collect.LimitParam,
		"limit":       limit,
		"as":          ad.Collect.As,
		"stop":        "short_page",
	}
	if ad.cacheable() {
		collect["cache"] = map[string]any{
			"enabled": cacheOn,
			"key":     key,
			"ttl":     ad.Collect.CacheTTL.String(),
			"dir":     config.CacheDir(),
		}
	}
	plan := map[string]any{
		"action":  ad.Name,
		"collect": collect,
	}
	if ad.FailWhen != "" {
		plan["fail_when"] = ad.FailWhen
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	rendered, err := respfmt.Render(raw, "", output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	fmt.Print(string(rendered))
	return nil
}
