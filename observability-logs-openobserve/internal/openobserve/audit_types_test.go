// Copyright 2026 The OpenChoreo Authors
// SPDX-License-Identifier: Apache-2.0

package openobserve

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const auditLine = `{"time":"2026-09-16T12:00:00.654321Z","level":"INFO","msg":"AUDIT-LOG",` +
	`"schema_version":"1.0","event_id":"0192-a","event_time":"2026-09-16T12:00:00.654321Z",` +
	`"actor":{"type":"user","id":"alice","issuer":"https://idp","entitlements":{"groups":["admins"]}},` +
	`"action":"create_project","category":"management","result":"success","producer":"openchoreo-api",` +
	`"resource":{"type":"project","namespace":"default","metadata":{"a":{"b":{"c":{"d":1}}}}}}`

func TestParseAuditRecord_ReadsTheRecordFromTheLogLine(t *testing.T) {
	record, err := ParseAuditRecord(map[string]interface{}{
		"log":                       auditLine,
		"actor_id":                  "flattened columns are not the source",
		"kubernetes_namespace_name": "openchoreo-control-plane",
		"kubernetes_pod_name":       "api-0",
		"kubernetes_container_name": "api-server",
	})
	if err != nil {
		t.Fatal(err)
	}

	if record.Actor.ID != "alice" || record.Actor.Entitlements["groups"][0] != "admins" {
		t.Errorf("actor = %+v", record.Actor)
	}
	if want := time.Date(2026, 9, 16, 12, 0, 0, 654321000, time.UTC); !record.EventTime.Equal(want) {
		t.Errorf("event_time = %s, want %s", record.EventTime, want)
	}
	if record.Resource == nil || record.Resource.Metadata["a"] == nil {
		t.Errorf("resource = %+v, want metadata nested past the flatten level intact", record.Resource)
	}
	if record.Collector != (AuditCollectorInfo{
		NamespaceName: "openchoreo-control-plane", PodName: "api-0", ContainerName: "api-server",
	}) {
		t.Errorf("collector = %+v", record.Collector)
	}
}

// The flattened columns lowercase keys and merge nested ones, so the log line is the only
// faithful copy.
func TestParseAuditRecord_ReadsRequestAndReturnedFromTheLogLine(t *testing.T) {
	line := strings.Replace(auditLine, `"schema_version":"1.0"`, `"schema_version":"1.1"`, 1)
	line = strings.TrimSuffix(line, "}") +
		`,"request":{"parameters":{"sinceSeconds":3600,"actor":{"id":["alice"]},"actor_id":"bob"}},` +
		`"returned":{"count":3,"items":[` +
		`{"type":"secret","namespace":"acme","name":"db-creds","uid":"uid-a"},` +
		`{"type":"secret","namespace":"acme","name":"api-key","uid":"uid-b"}]}}`

	record, err := ParseAuditRecord(map[string]interface{}{
		"log":                             line,
		"request_parameters_sinceseconds": float64(3600),
		"returned_items":                  `[{"name":"flattened columns are not the source"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}

	if record.Request == nil {
		t.Fatal("request = nil, want the published parameters")
	}
	wantParameters := map[string]any{
		"sinceSeconds": float64(3600),
		"actor":        map[string]any{"id": []any{"alice"}},
		"actor_id":     "bob",
	}
	if !reflect.DeepEqual(record.Request.Parameters, wantParameters) {
		t.Errorf("request.parameters = %v, want %v", record.Request.Parameters, wantParameters)
	}

	wantReturned := &AuditReturned{
		Count: 3,
		Items: []AuditReturnedItem{
			{Type: "secret", Namespace: "acme", Name: "db-creds", UID: "uid-a"},
			{Type: "secret", Namespace: "acme", Name: "api-key", UID: "uid-b"},
		},
	}
	if !reflect.DeepEqual(record.Returned, wantReturned) {
		t.Errorf("returned = %+v, want %+v", record.Returned, wantReturned)
	}
}

func TestParseAuditRecord_LeavesAbsentGroupsNil(t *testing.T) {
	record, err := ParseAuditRecord(map[string]interface{}{"log": auditLine})
	if err != nil {
		t.Fatal(err)
	}
	if record.Request != nil {
		t.Errorf("request = %+v, want nil", record.Request)
	}
	if record.Returned != nil {
		t.Errorf("returned = %+v, want nil", record.Returned)
	}
}

func TestParseAuditRecord_RejectsARowWithoutAnIdentity(t *testing.T) {
	for _, field := range []string{`"event_id":"0192-a",`, `"result":"success",`, `"schema_version":"1.0",`} {
		line := strings.Replace(auditLine, field, "", 1)
		if _, err := ParseAuditRecord(map[string]interface{}{"log": line}); err == nil {
			t.Errorf("expected a line without %s to be rejected", field)
		}
	}
}

func TestParseAuditRecord_KeepsARecordWithNoResolvedAction(t *testing.T) {
	line := strings.Replace(auditLine, `"action":"create_project","category":"management",`, "", 1)
	record, err := ParseAuditRecord(map[string]interface{}{"log": line})
	if err != nil {
		t.Fatalf("expected a record with no resolved action to be kept: %v", err)
	}
	if record.Action != "" || record.Category != "" {
		t.Errorf("action = %q, category = %q", record.Action, record.Category)
	}
}

func TestParseAuditRecord_RejectsARowWithoutALogLine(t *testing.T) {
	for _, row := range []map[string]interface{}{{}, {"log": ""}, {"log": "not json"}} {
		if _, err := ParseAuditRecord(row); err == nil {
			t.Errorf("expected %v to be rejected", row)
		}
	}
}
