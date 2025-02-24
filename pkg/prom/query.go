package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kubecost/cost-model/pkg/env"
	"github.com/kubecost/cost-model/pkg/errors"
	"github.com/kubecost/cost-model/pkg/log"
	"github.com/kubecost/cost-model/pkg/util/httputil"
	"github.com/kubecost/cost-model/pkg/util/json"
	prometheus "github.com/prometheus/client_golang/api"
)

const (
	apiPrefix    = "/api/v1"
	epQuery      = apiPrefix + "/query"
	epQueryRange = apiPrefix + "/query_range"
)

// prometheus query offset to apply to each non-range query
// package scope to prevent calling duration parse each use
var promQueryOffset time.Duration = env.GetPrometheusQueryOffset()

// Context wraps a Prometheus client and provides methods for querying and
// parsing query responses and errors.
type Context struct {
	Client         prometheus.Client
	name           string
	errorCollector *QueryErrorCollector
}

// NewContext creates a new Promethues querying context from the given client
func NewContext(client prometheus.Client) *Context {
	var ec QueryErrorCollector

	return &Context{
		Client:         client,
		name:           "",
		errorCollector: &ec,
	}
}

// NewNamedContext creates a new named Promethues querying context from the given client
func NewNamedContext(client prometheus.Client, name string) *Context {
	ctx := NewContext(client)
	ctx.name = name
	return ctx
}

// Warnings returns the warnings collected from the Context's ErrorCollector
func (ctx *Context) Warnings() []*QueryWarning {
	return ctx.errorCollector.Warnings()
}

// HasWarnings returns true if the ErrorCollector has warnings.
func (ctx *Context) HasWarnings() bool {
	return ctx.errorCollector.IsWarning()
}

// Errors returns the errors collected from the Context's ErrorCollector.
func (ctx *Context) Errors() []*QueryError {
	return ctx.errorCollector.Errors()
}

// HasErrors returns true if the ErrorCollector has errors
func (ctx *Context) HasErrors() bool {
	return ctx.errorCollector.IsError()
}

// ErrorCollection returns the aggregation of errors if there exists errors. Otherwise,
// nil is returned
func (ctx *Context) ErrorCollection() error {
	if ctx.errorCollector.IsError() {
		// errorCollector implements the error interface
		return ctx.errorCollector
	}

	return nil
}

// Query returns a QueryResultsChan, then runs the given query and sends the
// results on the provided channel. Receiver is responsible for closing the
// channel, preferably using the Read method.
func (ctx *Context) Query(query string) QueryResultsChan {
	resCh := make(QueryResultsChan)

	go runQuery(query, ctx, resCh, "")

	return resCh
}

// ProfileQuery returns a QueryResultsChan, then runs the given query with a profile
// label and sends the results on the provided channel. Receiver is responsible for closing the
// channel, preferably using the Read method.
func (ctx *Context) ProfileQuery(query string, profileLabel string) QueryResultsChan {
	resCh := make(QueryResultsChan)

	go runQuery(query, ctx, resCh, profileLabel)

	return resCh
}

// QueryAll returns one QueryResultsChan for each query provided, then runs
// each query concurrently and returns results on each channel, respectively,
// in the order they were provided; i.e. the response to queries[1] will be
// sent on channel resChs[1].
func (ctx *Context) QueryAll(queries ...string) []QueryResultsChan {
	resChs := []QueryResultsChan{}

	for _, q := range queries {
		resChs = append(resChs, ctx.Query(q))
	}

	return resChs
}

// ProfileQueryAll returns one QueryResultsChan for each query provided, then runs
// each ProfileQuery concurrently and returns results on each channel, respectively,
// in the order they were provided; i.e. the response to queries[1] will be
// sent on channel resChs[1].
func (ctx *Context) ProfileQueryAll(queries ...string) []QueryResultsChan {
	resChs := []QueryResultsChan{}

	for _, q := range queries {
		resChs = append(resChs, ctx.ProfileQuery(q, fmt.Sprintf("Query #%d", len(resChs)+1)))
	}

	return resChs
}

func (ctx *Context) QuerySync(query string) ([]*QueryResult, prometheus.Warnings, error) {
	raw, warnings, err := ctx.query(query)
	if err != nil {
		return nil, warnings, err
	}

	results := NewQueryResults(query, raw)
	if results.Error != nil {
		return nil, warnings, results.Error
	}

	return results.Results, warnings, nil
}

// QueryURL returns the URL used to query Prometheus
func (ctx *Context) QueryURL() *url.URL {
	return ctx.Client.URL(epQuery, nil)
}

// runQuery executes the prometheus query asynchronously, collects results and
// errors, and passes them through the results channel.
func runQuery(query string, ctx *Context, resCh QueryResultsChan, profileLabel string) {
	defer errors.HandlePanic()
	startQuery := time.Now()

	raw, warnings, requestError := ctx.query(query)
	results := NewQueryResults(query, raw)

	// report all warnings, request, and parse errors (nils will be ignored)
	ctx.errorCollector.Report(query, warnings, requestError, results.Error)

	if profileLabel != "" {
		log.Profile(startQuery, profileLabel)
	}

	resCh <- results
}

// RawQuery is a direct query to the prometheus client and returns the body of the response
func (ctx *Context) RawQuery(query string) ([]byte, error) {
	u := ctx.Client.URL(epQuery, nil)
	q := u.Query()
	q.Set("query", query)

	// for non-range queries, we set the timestamp for the query to time-offset
	// this is a special use case that's typically only used when our primary
	// prom db has delayed insertion (thanos, cortex, etc...)
	if promQueryOffset != 0 && ctx.name != AllocationContextName {
		q.Set("time", time.Now().Add(-promQueryOffset).UTC().Format(time.RFC3339))
	} else {
		q.Set("time", time.Now().UTC().Format(time.RFC3339))
	}

	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}

	// Set QueryContext name if non empty
	if ctx.name != "" {
		req = httputil.SetName(req, ctx.name)
	}
	req = httputil.SetQuery(req, query)

	// Note that the warnings return value from client.Do() is always nil using this
	// version of the prometheus client library. We parse the warnings out of the response
	// body after json decodidng completes.
	resp, body, _, err := ctx.Client.Do(context.Background(), req)
	if err != nil {
		if resp == nil {
			return nil, fmt.Errorf("query error: '%s' fetching query '%s'", err.Error(), query)
		}

		return nil, fmt.Errorf("query error %d: '%s' fetching query '%s'", resp.StatusCode, err.Error(), query)
	}

	// Unsuccessful Status Code, log body and status
	statusCode := resp.StatusCode
	statusText := http.StatusText(statusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, CommErrorf("%d (%s) URL: '%s', Request Headers: '%s', Headers: '%s', Body: '%s' Query: '%s'", statusCode, statusText, req.URL, req.Header, httputil.HeaderString(resp.Header), body, query)
	}

	return body, err
}

func (ctx *Context) query(query string) (interface{}, prometheus.Warnings, error) {
	body, err := ctx.RawQuery(query)
	if err != nil {
		return nil, nil, err
	}

	var toReturn interface{}
	err = json.Unmarshal(body, &toReturn)
	if err != nil {
		return nil, nil, fmt.Errorf("Unmarshal Error: %s\nQuery: %s", err, query)
	}

	warnings := warningsFrom(toReturn)
	for _, w := range warnings {
		// NoStoreAPIWarning is a warning that we would consider an error. It returns partial data relating only to the
		// store apis which were reachable. In order to ensure integrity of data across all clusters, we'll need to identify
		// this warning and convert it to an error.
		if IsNoStoreAPIWarning(w) {
			return nil, warnings, CommErrorf("Error: %s, Body: %s, Query: %s", w, body, query)
		}

		log.Warningf("fetching query '%s': %s", query, w)
	}

	return toReturn, warnings, nil
}

func (ctx *Context) QueryRange(query string, start, end time.Time, step time.Duration) QueryResultsChan {
	resCh := make(QueryResultsChan)

	go runQueryRange(query, start, end, step, ctx, resCh, "")

	return resCh
}

func (ctx *Context) ProfileQueryRange(query string, start, end time.Time, step time.Duration, profileLabel string) QueryResultsChan {
	resCh := make(QueryResultsChan)

	go runQueryRange(query, start, end, step, ctx, resCh, profileLabel)

	return resCh
}

func (ctx *Context) QueryRangeSync(query string, start, end time.Time, step time.Duration) ([]*QueryResult, prometheus.Warnings, error) {
	raw, warnings, err := ctx.queryRange(query, start, end, step)
	if err != nil {
		return nil, warnings, err
	}

	results := NewQueryResults(query, raw)
	if results.Error != nil {
		return nil, warnings, results.Error
	}

	return results.Results, warnings, nil
}

// QueryRangeURL returns the URL used to query_range Prometheus
func (ctx *Context) QueryRangeURL() *url.URL {
	return ctx.Client.URL(epQueryRange, nil)
}

// runQueryRange executes the prometheus queryRange asynchronously, collects results and
// errors, and passes them through the results channel.
func runQueryRange(query string, start, end time.Time, step time.Duration, ctx *Context, resCh QueryResultsChan, profileLabel string) {
	defer errors.HandlePanic()
	startQuery := time.Now()

	raw, warnings, requestError := ctx.queryRange(query, start, end, step)
	results := NewQueryResults(query, raw)

	// report all warnings, request, and parse errors (nils will be ignored)
	ctx.errorCollector.Report(query, warnings, requestError, results.Error)

	if profileLabel != "" {
		log.Profile(startQuery, profileLabel)
	}

	resCh <- results
}

// RawQuery is a direct query to the prometheus client and returns the body of the response
func (ctx *Context) RawQueryRange(query string, start, end time.Time, step time.Duration) ([]byte, error) {
	u := ctx.Client.URL(epQueryRange, nil)
	q := u.Query()
	q.Set("query", query)
	q.Set("start", start.Format(time.RFC3339Nano))
	q.Set("end", end.Format(time.RFC3339Nano))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}

	// Set QueryContext name if non empty
	if ctx.name != "" {
		req = httputil.SetName(req, ctx.name)
	}
	req = httputil.SetQuery(req, query)

	// Note that the warnings return value from client.Do() is always nil using this
	// version of the prometheus client library. We parse the warnings out of the response
	// body after json decodidng completes.
	resp, body, _, err := ctx.Client.Do(context.Background(), req)
	if err != nil {
		if resp == nil {
			return nil, fmt.Errorf("Error: %s, Body: %s Query: %s", err.Error(), body, query)
		}

		return nil, fmt.Errorf("%d (%s) Headers: %s Error: %s Body: %s Query: %s", resp.StatusCode, http.StatusText(resp.StatusCode), httputil.HeaderString(resp.Header), body, err.Error(), query)
	}

	// Unsuccessful Status Code, log body and status
	statusCode := resp.StatusCode
	statusText := http.StatusText(statusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, CommErrorf("%d (%s) Headers: %s, Body: %s Query: %s", statusCode, statusText, httputil.HeaderString(resp.Header), body, query)
	}

	return body, err
}

func (ctx *Context) queryRange(query string, start, end time.Time, step time.Duration) (interface{}, prometheus.Warnings, error) {
	body, err := ctx.RawQueryRange(query, start, end, step)
	if err != nil {
		return nil, nil, err
	}

	var toReturn interface{}
	err = json.Unmarshal(body, &toReturn)
	if err != nil {
		return nil, nil, fmt.Errorf("Unmarshal Error: %s\nQuery: %s", err, query)
	}

	warnings := warningsFrom(toReturn)
	for _, w := range warnings {
		// NoStoreAPIWarning is a warning that we would consider an error. It returns partial data relating only to the
		// store apis which were reachable. In order to ensure integrity of data across all clusters, we'll need to identify
		// this warning and convert it to an error.
		if IsNoStoreAPIWarning(w) {
			return nil, warnings, CommErrorf("Error: %s, Body: %s, Query: %s", w, body, query)
		}

		log.Warningf("fetching query '%s': %s", query, w)
	}

	return toReturn, warnings, nil
}

// Extracts the warnings from the resulting json if they exist (part of the prometheus response api).
func warningsFrom(result interface{}) prometheus.Warnings {
	var warnings prometheus.Warnings

	if resultMap, ok := result.(map[string]interface{}); ok {
		if warningProp, ok := resultMap["warnings"]; ok {
			if w, ok := warningProp.([]string); ok {
				warnings = w
			}
		}
	}

	return warnings
}
