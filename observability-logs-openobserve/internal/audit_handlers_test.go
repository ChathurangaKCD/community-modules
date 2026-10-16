// Copyright 2026 The OpenChoreo Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openchoreo/community-modules/observability-logs-openobserve/internal/api/gen"
	"github.com/openchoreo/community-modules/observability-logs-openobserve/internal/openobserve"
)

var (
	auditStart = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	auditEnd   = time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
)

const auditTestLine = `{"schema_version":"1.0","event_id":"0192-a","event_time":"2026-09-16T12:10:00.5Z",` +
	`"actor":{"type":"user","id":"alice"},"action":"create_project","category":"management",` +
	`"result":"denied","producer":"openchoreo-api","http":{"method":"POST","path":"/api/v1/projects"},` +
	`"resource":{"type":"project","namespace":"default"}}`

type auditSearch struct {
	SQL  string
	Size int
}

// auditServer fakes OpenObserve's stream schema and _search endpoints.
func auditServer(t *testing.T, columns []string, searches *[]auditSearch) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/streams/audit_logs/schema") {
			fields := make([]map[string]string, 0, len(columns))
			for _, c := range columns {
				fields = append(fields, map[string]string{"name": c, "type": "Utf8"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"schema": fields})
			return
		}

		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Query struct {
				SQL  string `json:"sql"`
				Size int    `json:"size"`
			} `json:"query"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("parse request body: %v", err)
		}
		if searches != nil {
			*searches = append(*searches, auditSearch{SQL: parsed.Query.SQL, Size: parsed.Query.Size})
		}

		var hits []map[string]interface{}
		switch sql := parsed.Query.SQL; {
		case strings.Contains(sql, "as total"):
			hits = []map[string]interface{}{{"total": 42}}
		case strings.Contains(sql, "as bucket"):
			hits = []map[string]interface{}{{"bucket": 0, "result": "denied", "record_count": 5}}
		case strings.Contains(sql, "record_count"):
			hits = []map[string]interface{}{{"value": "alice", "record_count": 7}}
		default:
			hits = []map[string]interface{}{
				{"log": auditTestLine, "kubernetes_pod_name": "api-0"},
				{"log": "{}"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"took": 3, "hits": hits})
	}))
}

func auditHandler(serverURL string) *LogsHandler {
	return NewLogsHandler(
		openobserve.NewClient(serverURL, "default", "default", "k8s_events", "admin", "token", testLogger()),
		nil, testLogger(),
	)
}

var storedAuditColumns = []string{"_timestamp", "log", "event_id", "result", "actor_id", "resource_project"}

func TestQueryAuditLogs_RejectsAnInvalidRequest(t *testing.T) {
	h := auditHandler("http://unused")
	for name, body := range map[string]*gen.AuditLogsQueryRequest{
		"no body":            nil,
		"empty window":       {StartTime: auditStart, EndTime: auditStart},
		"bad timeline width": {StartTime: auditStart, EndTime: auditEnd, IncludeTimeline: ptr(true), TimelineInterval: ptr("5s")},
		"unknown category":   {StartTime: auditStart, EndTime: auditEnd, Category: &[]gen.AuditLogsQueryRequestCategory{"bogus"}},
		"unknown result":     {StartTime: auditStart, EndTime: auditEnd, Result: &[]gen.AuditLogsQueryRequestResult{"bogus"}},
		"unknown surface":    {StartTime: auditStart, EndTime: auditEnd, Surface: &[]gen.AuditLogsQueryRequestSurface{"bogus"}},
		"unknown sortOrder":  {StartTime: auditStart, EndTime: auditEnd, SortOrder: ptr(gen.AuditLogsQueryRequestSortOrder("bogus"))},
	} {
		resp, err := h.QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := resp.(gen.QueryAuditLogs400JSONResponse); !ok {
			t.Errorf("%s: got %T, want 400", name, resp)
		}
	}
}

func TestQueryAuditLogs_ReturnsRecordsTotalAndTimeline(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, storedAuditColumns, &searches)
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{
			StartTime:        auditStart,
			EndTime:          auditEnd,
			Result:           &[]gen.AuditLogsQueryRequestResult{"denied"},
			IncludeTimeline:  ptr(true),
			TimelineInterval: ptr("30m"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := resp.(gen.QueryAuditLogs200JSONResponse)
	if !isOK {
		t.Fatalf("got %T, want 200", resp)
	}

	if len(ok.Records) != 1 {
		t.Fatalf("got %d records, want the malformed row skipped", len(ok.Records))
	}
	record := ok.Records[0]
	if record.EventId != "0192-a" || record.Result != "denied" || record.Http == nil || *record.Http.Method != "POST" {
		t.Errorf("record = %+v", record)
	}
	if record.Collector == nil || *record.Collector.PodName != "api-0" {
		t.Errorf("collector = %+v", record.Collector)
	}
	if ok.Total != 42 {
		t.Errorf("total = %d, want the count query's 42", ok.Total)
	}
	if ok.Timeline == nil || ok.Timeline.Interval != "30m" || len(ok.Timeline.Buckets) != 2 ||
		ok.Timeline.Buckets[0].Total != 5 {
		t.Errorf("timeline = %+v", ok.Timeline)
	}

	if len(searches) != 3 {
		t.Fatalf("got %d searches, want records, count and timeline", len(searches))
	}
	for _, s := range searches {
		if !strings.Contains(s.SQL, "(result = 'denied')") {
			t.Errorf("search does not apply the filter: %s", s.SQL)
		}
	}
}

// publishedAuditLine is a 1.1 record as the producer writes it.
const publishedAuditLine = `{"schema_version":"1.1","event_id":"0192-b","event_time":"2026-09-16T12:20:00Z",` +
	`"actor":{"type":"user","id":"u1"},"action":"list_secrets","category":"access","result":"success",` +
	`"producer":"openchoreo-api","surface":"rest","operation_id":"ListSecrets",` +
	`"resource":{"type":"secret","namespace":"acme"},` +
	`"request":{"parameters":{"container":"api","sinceSeconds":3600,` +
	`"actor":{"id":["alice"]},"search_scope":{"namespace":"acme"}}},` +
	`"returned":{"count":2,"items":[` +
	`{"type":"secret","namespace":"acme","name":"db-creds","uid":"uid-a"},` +
	`{"type":"secret","namespace":"acme","name":"api-key","uid":"uid-b"}]}}`

func TestToGenAuditLogRecord_ReturnsRequestAndReturnedAsPublished(t *testing.T) {
	parsed, err := openobserve.ParseAuditRecord(map[string]interface{}{"log": publishedAuditLine})
	if err != nil {
		t.Fatal(err)
	}

	served, err := json.Marshal(toGenAuditLogRecord(parsed))
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]interface{}
	if err := json.Unmarshal(served, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(publishedAuditLine), &want); err != nil {
		t.Fatal(err)
	}

	for _, group := range []string{"request", "returned"} {
		if !reflect.DeepEqual(got[group], want[group]) {
			t.Errorf("%s = %v, want %v", group, got[group], want[group])
		}
	}
	if got["schema_version"] != "1.1" {
		t.Errorf("schema_version = %v, want it served as stored", got["schema_version"])
	}
}

func TestToGenAuditLogRecord_KeepsAReturnedCountOfZero(t *testing.T) {
	line := strings.TrimSuffix(auditTestLine, "}") + `,"returned":{"count":0}}`
	parsed, err := openobserve.ParseAuditRecord(map[string]interface{}{"log": line})
	if err != nil {
		t.Fatal(err)
	}

	record := toGenAuditLogRecord(parsed)
	if record.Returned == nil {
		t.Fatal("returned = nil, want a count of 0")
	}
	if record.Returned.Count != 0 || record.Returned.Items != nil {
		t.Errorf("returned = %+v, want count 0 and no items", record.Returned)
	}
}

func TestQueryAuditLogs_LeavesAbsentGroupsOutOfA10Record(t *testing.T) {
	server := auditServer(t, storedAuditColumns, nil)
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{StartTime: auditStart, EndTime: auditEnd},
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := resp.(gen.QueryAuditLogs200JSONResponse)
	if !isOK || len(ok.Records) != 1 {
		t.Fatalf("got %+v, want one record", resp)
	}
	if ok.Records[0].Request != nil || ok.Records[0].Returned != nil {
		t.Errorf("request = %+v, returned = %+v, want both nil", ok.Records[0].Request, ok.Records[0].Returned)
	}
}

func TestQueryAuditLogs_FiltersOnReturnedItemNames(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, append(storedAuditColumns, "returned_items"), &searches)
	defer server.Close()

	_, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{
			StartTime: auditStart,
			EndTime:   auditEnd,
			Returned: &gen.AuditLogsReturnedFilter{
				Items: &gen.AuditLogsReturnedItemsFilter{Name: &[]string{"db-creds"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(searches) == 0 {
		t.Fatal("sent no searches")
	}
	for _, s := range searches {
		if !strings.Contains(s.SQL, `(strpos(returned_items, '"name":"db-creds"') > 0)`) {
			t.Errorf("search does not apply the filter: %s", s.SQL)
		}
	}
}

func TestQueryAuditLogs_ClampsTheLimitToTheContractMaximum(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, storedAuditColumns, &searches)
	defer server.Close()

	_, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{StartTime: auditStart, EndTime: auditEnd, Limit: ptr(5000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(searches) == 0 || searches[0].Size != 1000 {
		t.Errorf("searches = %+v, want the record query sized 1000", searches)
	}
}

func TestQueryAuditLogs_AnswersEmptyForAFieldNeverStored(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, storedAuditColumns, &searches)
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{
			StartTime:       auditStart,
			EndTime:         auditEnd,
			Resource:        &gen.AuditLogsResourceFilter{Component: &[]string{"api"}},
			IncludeTimeline: ptr(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := resp.(gen.QueryAuditLogs200JSONResponse)
	if !isOK {
		t.Fatalf("got %T, want 200", resp)
	}
	if len(ok.Records) != 0 || ok.Total != 0 {
		t.Errorf("got %d records, total %d, want none", len(ok.Records), ok.Total)
	}
	if ok.Timeline == nil || len(ok.Timeline.Buckets) == 0 || ok.Timeline.Buckets[0].Total != 0 {
		t.Errorf("timeline = %+v, want zero-filled buckets", ok.Timeline)
	}
	if len(searches) != 0 {
		t.Errorf("sent %d searches OpenObserve would reject", len(searches))
	}
}

func TestQueryAuditLogs_SearchFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogs(context.Background(), gen.QueryAuditLogsRequestObject{
		Body: &gen.AuditLogsQueryRequest{StartTime: auditStart, EndTime: auditEnd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.(gen.QueryAuditLogs500JSONResponse); !ok {
		t.Errorf("got %T, want 500", resp)
	}
}

func TestQueryAuditLogFilterValues_RejectsAnInvalidRequest(t *testing.T) {
	h := auditHandler("http://unused")
	for name, body := range map[string]*gen.AuditLogFilterValuesRequest{
		"no body":        nil,
		"unknown filter": {Filter: "request_id", Query: gen.AuditLogsQueryRequest{StartTime: auditStart, EndTime: auditEnd}},
		"empty window":   {Filter: "actor.id", Query: gen.AuditLogsQueryRequest{StartTime: auditEnd, EndTime: auditStart}},
		"unknown category in query": {Filter: "actor.id", Query: gen.AuditLogsQueryRequest{
			StartTime: auditStart, EndTime: auditEnd,
			Category: &[]gen.AuditLogsQueryRequestCategory{"bogus"},
		}},
	} {
		resp, err := h.QueryAuditLogFilterValues(context.Background(), gen.QueryAuditLogFilterValuesRequestObject{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := resp.(gen.QueryAuditLogFilterValues400JSONResponse); !ok {
			t.Errorf("%s: got %T, want 400", name, resp)
		}
	}
}

func TestQueryAuditLogFilterValues_DropsOwnSelectionsAndClampsMaxValues(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, storedAuditColumns, &searches)
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogFilterValues(context.Background(), gen.QueryAuditLogFilterValuesRequestObject{
		Body: &gen.AuditLogFilterValuesRequest{
			Filter: "actor.id",
			Query: gen.AuditLogsQueryRequest{
				StartTime: auditStart,
				EndTime:   auditEnd,
				Actor:     &gen.AuditLogsActorFilter{Id: &[]string{"bob"}},
				Resource:  &gen.AuditLogsResourceFilter{Project: &[]string{"p1"}},
			},
			MaxValues: ptr(5000),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := resp.(gen.QueryAuditLogFilterValues200JSONResponse)
	if !isOK {
		t.Fatalf("got %T, want 200", resp)
	}
	if len(ok.Values) != 1 || ok.Values[0].Value != "alice" || ok.Values[0].Count != 7 || ok.TotalValues != 42 {
		t.Errorf("response = %+v", ok)
	}

	if len(searches) != 2 {
		t.Fatalf("got %d searches, want values and total", len(searches))
	}
	for _, s := range searches {
		if strings.Contains(s.SQL, "'bob'") {
			t.Errorf("search applies the listed filter's own selection: %s", s.SQL)
		}
		if !strings.Contains(s.SQL, "(resource_project = 'p1')") {
			t.Errorf("search drops the other filters: %s", s.SQL)
		}
	}
	if !strings.HasSuffix(searches[0].SQL, "LIMIT 1000") {
		t.Errorf("values query = %s, want LIMIT 1000", searches[0].SQL)
	}
}

func TestQueryAuditLogFilterValues_AnswersEmptyForAFieldNeverStored(t *testing.T) {
	var searches []auditSearch
	server := auditServer(t, storedAuditColumns, &searches)
	defer server.Close()

	resp, err := auditHandler(server.URL).QueryAuditLogFilterValues(context.Background(), gen.QueryAuditLogFilterValuesRequestObject{
		Body: &gen.AuditLogFilterValuesRequest{
			Filter: "actor.entitlements",
			Query:  gen.AuditLogsQueryRequest{StartTime: auditStart, EndTime: auditEnd},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := resp.(gen.QueryAuditLogFilterValues200JSONResponse)
	if !isOK {
		t.Fatalf("got %T, want 200", resp)
	}
	if len(ok.Values) != 0 || ok.TotalValues != 0 || len(searches) != 0 {
		t.Errorf("response = %+v after %d searches, want empty with none sent", ok, len(searches))
	}
}
