// package loki generates qgueries for logs stored in Loki or LokiStack
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/korrel8r/korrel8r/internal/pkg/openshift"
	"github.com/korrel8r/korrel8r/internal/pkg/openshift/console"
	"github.com/korrel8r/korrel8r/pkg/korrel8r"
	"github.com/korrel8r/korrel8r/pkg/korrel8r/impl"
	"golang.org/x/exp/slices"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// Verify implementing interfaces.
	_ korrel8r.Domain   = Domain
	_ console.Converter = Domain
	_ korrel8r.Store    = &Store{}
	_ korrel8r.Query    = &Query{}
	_ korrel8r.Class    = Class("")
)

var Domain = domain{}

type domain struct{}

func (domain) String() string                   { return "loki" }
func (domain) Class(name string) korrel8r.Class { return classMap[name] }
func (domain) Classes() []korrel8r.Class        { return classes }
func (domain) Query(c korrel8r.Class) korrel8r.Query {
	if cc, ok := c.(Class); ok {
		return &Query{Tenant: string(cc)}
	}
	return &Query{}
}

func (domain) QueryToConsoleURL(query korrel8r.Query) (*url.URL, error) {
	q, err := impl.TypeAssert[*Query](query)
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Add("q", q.LogQL)
	v.Add("tenant", q.Tenant)
	return &url.URL{Path: "/monitoring/logs", RawQuery: v.Encode()}, nil
}

func (domain) ConsoleURLToQuery(u *url.URL) (korrel8r.Query, error) {
	if c, ok := classMap[u.Query().Get("tenant")]; ok {
		return &Query{
			LogQL:  u.Query().Get("q"),
			Tenant: c.String(),
		}, nil
	}
	return nil, fmt.Errorf("not a valid Loki URL: %v", u)
}

// Class is the log_type name (aka tenant in lokistack)
type Class string

func (c Class) Domain() korrel8r.Domain { return Domain }
func (c Class) String() string          { return string(c) }
func (c Class) New() korrel8r.Object    { return Object("") }

type Object string // Log record

// Query is a LogQL query string
type Query struct {
	LogQL  string
	Tenant string
}

const (
	Application    = "application"
	Infrastructure = "infrastructure"
	Audit          = "audit"
)

var (
	classes  = []korrel8r.Class{Class(Application), Class(Infrastructure), Class(Audit)}
	classMap = map[string]Class{Application: Class(Application), Infrastructure: Class(Infrastructure), Audit: Class(Audit)}
)

func (q *Query) String() string        { return q.LogQL }
func (q *Query) Class() korrel8r.Class { return Class(q.Tenant) }

func (q *Query) plainURL() *url.URL {
	v := url.Values{}
	v.Add("query", q.LogQL)
	v.Add("direction", "forward")
	// TODO constraint inside query
	// if constraint != nil {
	// 	if constraint.Limit != nil {
	// 		v.Add("limit", fmt.Sprintf("%v", *constraint.Limit))
	// 	}
	// 	if constraint.Start != nil {
	// 		v.Add("start", fmt.Sprintf("%v", constraint.Start.UnixNano()))
	// 	}
	// 	if constraint.End != nil {
	// 		v.Add("end", fmt.Sprintf("%v", constraint.End.UnixNano()))
	// 	}
	// }
	return &url.URL{Path: "/loki/api/v1/query_range", RawQuery: v.Encode()}
}

func (q *Query) lokiStackURL() *url.URL {
	u := q.plainURL()
	if q.Tenant == "" {
		q.Tenant = Application
	}
	u.Path = path.Join("/api/logs/v1/", q.Tenant, u.Path)
	return u
}

type Store struct {
	c        *http.Client
	base     *url.URL
	queryURL func(*Query) *url.URL
}

func (Store) Domain() korrel8r.Domain { return Domain }

// NewLokiStackStore returns a store that uses a LokiStack observatorium-style URLs.
func NewLokiStackStore(base *url.URL, c *http.Client) (*Store, error) {
	return &Store{c: c, base: base, queryURL: (*Query).lokiStackURL}, nil
}

// NewPlainLokiStore returns a store that uses plain Loki URLs.
func NewPlainLokiStore(base *url.URL, c *http.Client) (*Store, error) {
	return &Store{c: c, base: base, queryURL: (*Query).plainURL}, nil
}

func (s *Store) Get(ctx context.Context, query korrel8r.Query, result korrel8r.Appender) error {
	q, err := impl.TypeAssert[*Query](query)
	if err != nil {
		return err
	}
	u := s.base.ResolveReference(s.queryURL(q))

	resp, err := s.c.Get(u.String())
	if err != nil {
		return fmt.Errorf("%w: %v", err, u)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%v: %v", resp.Status, u)
	}
	qr := queryResponse{}
	if err = json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return err
	}
	if qr.Status != "success" {
		return fmt.Errorf("expected 'status: success' in %v", qr)
	}
	if qr.Data.ResultType != "streams" {
		return fmt.Errorf("expected 'resultType: streams' in %v", qr)
	}
	// Interleave and sort the stream results.
	var logs [][]string // Each log is [timestamp,logline]
	for _, sv := range qr.Data.Result {
		logs = append(logs, sv.Values...)
	}
	slices.SortStableFunc(logs, func(a, b []string) bool { return a[0] < b[0] })
	for _, tl := range logs { // tl is [time, line]
		result.Append(Object(tl[1]))
	}
	return nil
}

// queryResponse is the response to a loki query.
type queryResponse struct {
	Status string    `json:"status"`
	Data   queryData `json:"data"`
}

// queryData holds the data for a query
type queryData struct {
	ResultType string         `json:"resultType"`
	Result     []streamValues `json:"result"`
}

// streamValues is a set of log values ["time", "line"] for a log stream.
type streamValues struct {
	Stream map[string]string `json:"stream"` // Labels for the stream
	Values [][]string        `json:"values"`
}

func NewOpenshiftLokiStackStore(ctx context.Context, c client.Client, cfg *rest.Config) (*Store, error) {
	host, err := openshift.RouteHost(ctx, c, openshift.LokiStackNSName)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return NewLokiStackStore(&url.URL{Scheme: "https", Host: host}, hc)
}
