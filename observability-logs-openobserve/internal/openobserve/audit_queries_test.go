// Copyright 2026 The OpenChoreo Authors
// SPDX-License-Identifier: Apache-2.0

package openobserve

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	auditStart = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	auditEnd   = time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
)

func auditColumns(names ...string) map[string]bool {
	columns := map[string]bool{}
	for _, n := range names {
		columns[n] = true
	}
	return columns
}

// auditSQL takes a generator's results and returns its SQL and query envelope.
func auditSQL(t *testing.T) func([]byte, error) (string, map[string]interface{}) {
	return func(queryJSON []byte, err error) (string, map[string]interface{}) {
		t.Helper()
		if err != nil {
			t.Fatalf("generate query: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(queryJSON, &parsed); err != nil {
			t.Fatalf("parse query: %v", err)
		}
		query := parsed["query"].(map[string]interface{})
		return query["sql"].(string), query
	}
}

func TestAuditConditions_QuotesValuesLiterally(t *testing.T) {
	conditions, ok := auditConditions(AuditLogsParams{
		ActorIDs:   []string{"it's", `dev\ops`},
		UserAgents: []string{"occ (50%_off)"},
	}, auditColumns("actor_id", "user_agent"))
	if !ok {
		t.Fatal("expected the conditions to be buildable")
	}

	want := []string{
		`(actor_id = 'it''s' OR actor_id = 'dev\ops')`,
		`(user_agent = 'occ (50%_off)')`,
	}
	if strings.Join(conditions, " AND ") != strings.Join(want, " AND ") {
		t.Errorf("got %v, want %v", conditions, want)
	}
}

func TestAuditConditions_RejectsAFilterOnAColumnNeverStored(t *testing.T) {
	_, ok := auditConditions(AuditLogsParams{
		ResourceComponents: []string{"api"},
	}, auditColumns("resource_project"))
	if ok {
		t.Error("expected a filter on an unstored column to match nothing")
	}
}

func TestAuditConditions_MatchesEntitlementsAcrossEveryClaim(t *testing.T) {
	conditions, ok := auditConditions(AuditLogsParams{
		ActorEntitlements: []string{"admins"},
	}, auditColumns("actor_id", "actor_entitlements_sub", "actor_entitlements_groups"))
	if !ok {
		t.Fatal("expected the conditions to be buildable")
	}

	want := `(array_has(cast_to_arr("actor_entitlements_groups"), 'admins') OR ` +
		`array_has(cast_to_arr("actor_entitlements_sub"), 'admins'))`
	if len(conditions) != 1 || conditions[0] != want {
		t.Errorf("got %v, want %s", conditions, want)
	}
}

func TestAuditConditions_RejectsAnEntitlementFilterWithNoClaimStored(t *testing.T) {
	if _, ok := auditConditions(AuditLogsParams{
		ActorEntitlements: []string{"admins"},
	}, auditColumns("actor_id")); ok {
		t.Error("expected an entitlement filter with no claim columns to match nothing")
	}
}

func TestAuditConditions_MatchesAReturnedItemNameAnchoredOnItsKey(t *testing.T) {
	conditions, ok := auditConditions(AuditLogsParams{
		ResourceTypes:     []string{"secret"},
		ReturnedItemNames: []string{"db-creds", "api-key"},
	}, auditColumns("resource_type", "returned_items"))
	if !ok {
		t.Fatal("expected the conditions to be buildable")
	}

	want := []string{
		`(resource_type = 'secret')`,
		`(strpos(returned_items, '"name":"db-creds"') > 0 OR strpos(returned_items, '"name":"api-key"') > 0)`,
	}
	if strings.Join(conditions, " AND ") != strings.Join(want, " AND ") {
		t.Errorf("got %v, want %v", conditions, want)
	}
}

func TestAuditConditions_EscapesAReturnedItemNameForJSONAndSQL(t *testing.T) {
	conditions, ok := auditConditions(AuditLogsParams{
		ReturnedItemNames: []string{`it's "a"<b>\c`},
	}, auditColumns("returned_items"))
	if !ok {
		t.Fatal("expected the conditions to be buildable")
	}

	want := `(strpos(returned_items, '"name":"it''s \"a\"<b>\\c"') > 0)`
	if len(conditions) != 1 || conditions[0] != want {
		t.Errorf("got %v, want %s", conditions, want)
	}
}

func TestAuditConditions_RejectsAReturnedItemFilterWithNoItemsStored(t *testing.T) {
	if _, ok := auditConditions(AuditLogsParams{
		ReturnedItemNames: []string{"db-creds"},
	}, auditColumns("actor_id", "returned_count")); ok {
		t.Error("expected a returned item filter with no returned_items column to match nothing")
	}
}

func TestAuditConditions_MatchesTheSearchPhraseLiterally(t *testing.T) {
	conditions, ok := auditConditions(AuditLogsParams{SearchPhrase: "50%_'off"}, auditColumns("log"))
	if !ok {
		t.Fatal("expected the conditions to be buildable")
	}
	if want := `strpos(log, '50%_''off') > 0`; len(conditions) != 1 || conditions[0] != want {
		t.Errorf("got %v, want %s", conditions, want)
	}
}

func TestGenerateAuditLogsQuery_OrdersByTimeThenEventID(t *testing.T) {
	params := AuditLogsParams{StartTime: auditStart, EndTime: auditEnd, Limit: 25, SortOrder: "asc"}
	sql, query := auditSQL(t)(generateAuditLogsQuery(params, "audit_logs", []string{"(result = 'denied')"}, nil))

	want := `SELECT * FROM "audit_logs" WHERE (result = 'denied') ORDER BY _timestamp ASC, event_id ASC`
	if sql != want {
		t.Errorf("sql = %s, want %s", sql, want)
	}
	if query["size"].(float64) != 25 {
		t.Errorf("size = %v, want 25", query["size"])
	}
	if int64(query["start_time"].(float64)) != auditStart.UnixMicro() ||
		int64(query["end_time"].(float64)) != auditEnd.UnixMicro() {
		t.Errorf("window = %v..%v, want %d..%d", query["start_time"], query["end_time"],
			auditStart.UnixMicro(), auditEnd.UnixMicro())
	}
}

func TestGenerateAuditLogsQuery_DefaultsToNewestFirst(t *testing.T) {
	sql, _ := auditSQL(t)(generateAuditLogsQuery(
		AuditLogsParams{StartTime: auditStart, EndTime: auditEnd}, "audit_logs", nil, nil))
	if !strings.HasSuffix(sql, "ORDER BY _timestamp DESC, event_id DESC") {
		t.Errorf("sql = %s", sql)
	}
}

func TestGenerateAuditLogsQuery_KeepsSubSecondPrecisionInTheWindow(t *testing.T) {
	start := auditStart.Add(123456 * time.Microsecond)
	_, query := auditSQL(t)(generateAuditLogsQuery(
		AuditLogsParams{StartTime: start, EndTime: auditEnd}, "audit_logs", nil, nil))
	if int64(query["start_time"].(float64)) != start.UnixMicro() {
		t.Errorf("start_time = %v, want %d", query["start_time"], start.UnixMicro())
	}
}

func TestGenerateAuditTimelineQuery_NumbersBucketsFromTheWindowStart(t *testing.T) {
	sql, _ := auditSQL(t)(generateAuditTimelineQuery(
		AuditLogsParams{StartTime: auditStart, EndTime: auditEnd}, "audit_logs", nil, 15*time.Minute, nil))

	want := "SELECT CAST((_timestamp - 1789560000000000) / 900000000 AS BIGINT) as bucket, " +
		`result as result, count(*) as record_count FROM "audit_logs" GROUP BY bucket, result`
	if sql != want {
		t.Errorf("sql = %s, want %s", sql, want)
	}
}

func TestBuildAuditTimeline_FillsEmptyBucketsAndEndsWithAShortOne(t *testing.T) {
	end := auditStart.Add(50 * time.Minute)
	timeline, err := buildAuditTimeline("15m", auditStart, end, []map[string]interface{}{
		{"bucket": float64(0), "result": "success", "record_count": float64(2)},
		{"bucket": float64(0), "result": "denied", "record_count": float64(1)},
		{"bucket": float64(3), "result": "success", "record_count": float64(4)},
		{"bucket": float64(9), "result": "success", "record_count": float64(99)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(timeline.Buckets) != 4 {
		t.Fatalf("got %d buckets, want 4 for a 50m window at 15m", len(timeline.Buckets))
	}
	wantTotals := []int64{3, 0, 0, 4}
	for i, b := range timeline.Buckets {
		if !b.StartTime.Equal(auditStart.Add(time.Duration(i) * 15 * time.Minute)) {
			t.Errorf("bucket %d starts at %s", i, b.StartTime)
		}
		if b.Total != wantTotals[i] {
			t.Errorf("bucket %d total = %d, want %d", i, b.Total, wantTotals[i])
		}
	}
	if timeline.Buckets[0].Counts["denied"] != 1 || timeline.Buckets[0].Counts["success"] != 2 {
		t.Errorf("bucket 0 counts = %v", timeline.Buckets[0].Counts)
	}
}

func TestResolveTimelineInterval(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name      string
		requested string
		window    time.Duration
		want      string
	}{
		{"honours a request within the ceiling", "15m", time.Hour, "15m"},
		{"chooses about sixty buckets when omitted", "", 10 * time.Hour, "10m"},
		{"never goes below a minute", "", 10 * time.Minute, "1m"},
		{"coarsens a request past the ceiling", "1m", 30 * day, "1d"},
		{"counts the partial bucket", "1m", 500*time.Minute + time.Second, "1h"},
		{"widens past weeks in whole weeks", "1w", 4000 * day, "2w"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTimelineInterval(tt.requested, auditStart, auditStart.Add(tt.window))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestResolveTimelineInterval_RejectsAnInvalidInterval(t *testing.T) {
	for _, requested := range []string{"15s", "m", "0m", "-1h", "99999999999w"} {
		if _, err := ResolveTimelineInterval(requested, auditStart, auditEnd); err == nil {
			t.Errorf("expected %q to be rejected", requested)
		}
	}
}

func TestAuditValuesSource_ReadsTheFilterColumn(t *testing.T) {
	source, ok := auditValuesSource("audit_logs", "resource.project", []string{"(result = 'denied')"},
		auditColumns("resource_project", "result"))
	if !ok {
		t.Fatal("expected a source")
	}
	if want := `(SELECT resource_project as value, event_id FROM "audit_logs" WHERE (result = 'denied'))`; source != want {
		t.Errorf("source = %s, want %s", source, want)
	}
}

func TestAuditValuesSource_HasNoneForAColumnNeverStored(t *testing.T) {
	if _, ok := auditValuesSource("audit_logs", "resource.component", nil, auditColumns("resource_project")); ok {
		t.Error("expected no source for an unstored column")
	}
}

func TestAuditValuesSource_UnnestsEveryEntitlementClaim(t *testing.T) {
	source, ok := auditValuesSource("audit_logs", AuditEntitlementsFilter, nil,
		auditColumns("actor_entitlements_sub", "actor_entitlements_groups"))
	if !ok {
		t.Fatal("expected a source")
	}
	want := `(SELECT unnest(cast_to_arr("actor_entitlements_groups")) as value, event_id FROM "audit_logs" ` +
		`WHERE "actor_entitlements_groups" IS NOT NULL UNION ALL ` +
		`SELECT unnest(cast_to_arr("actor_entitlements_sub")) as value, event_id FROM "audit_logs" ` +
		`WHERE "actor_entitlements_sub" IS NOT NULL)`
	if source != want {
		t.Errorf("source = %s, want %s", source, want)
	}
}

func TestGenerateAuditFilterValuesQuery_OrdersBusiestFirstAndSearchesCaseInsensitively(t *testing.T) {
	params := AuditLogsParams{StartTime: auditStart, EndTime: auditEnd}
	sql, query := auditSQL(t)(generateAuditFilterValuesQuery(params, "(src)", "Dev's", 50, nil))

	want := `SELECT value, count(distinct event_id) as record_count FROM (src) WHERE value IS NOT NULL AND value != '' ` +
		`AND strpos(lower(value), 'dev''s') > 0 GROUP BY value ORDER BY record_count DESC, value ASC LIMIT 50`
	if sql != want {
		t.Errorf("sql = %s, want %s", sql, want)
	}
	if query["size"].(float64) != 50 {
		t.Errorf("size = %v, want 50", query["size"])
	}

	totalSQL, _ := auditSQL(t)(generateAuditFilterValuesTotalQuery(params, "(src)", "Dev's", nil))
	wantTotal := `SELECT count(distinct value) as total FROM (src) WHERE value IS NOT NULL AND value != '' ` +
		`AND strpos(lower(value), 'dev''s') > 0`
	if totalSQL != wantTotal {
		t.Errorf("total sql = %s, want %s", totalSQL, wantTotal)
	}
}

func TestParseAuditFilterValues_SkipsEmptyValues(t *testing.T) {
	values := parseAuditFilterValues(&OpenObserveResponse{Hits: []map[string]interface{}{
		{"value": "denied", "record_count": float64(3)},
		{"value": "", "record_count": float64(9)},
	}})
	if len(values) != 1 || values[0] != (AuditFilterValue{Value: "denied", Count: 3}) {
		t.Errorf("got %v", values)
	}
}
