package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/maltzsama/yellow-bricks/pkg/models"
)

func TestQueryData_Success(t *testing.T) {

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub database connection: %v", err)
	}
	defer db.Close()

	ds := &Datasource{
		DB: db,
		config: &models.PluginSettings{
			Catalog: "my_catalog",
			Token:   &models.SecretPluginSettings{Token: "dummy"},
			Host:    "dummy.host",
			Path:    "dummy/path",
		},
	}

	testSQL := "SELECT * FROM my_schema.my_table"
	payload := fmt.Sprintf(`{"queryText": "%s", "format": "table"}`, testSQL)

	expectedSQL := "SELECT * FROM my_catalog.my_schema.my_table"

	columns := []string{"col1", "col2"}
	rows := sqlmock.NewRows(columns).AddRow("value1", "value2")
	mock.ExpectQuery(regexp.QuoteMeta(expectedSQL)).WillReturnRows(rows)

	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{
			{RefID: "A", JSON: []byte(payload)},
		},
	}

	resp, err := ds.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData returned error: %v", err)
	}

	qResp, ok := resp.Responses["A"]
	if !ok {
		t.Fatal("QueryData must return a response for RefID A")
	}

	if len(qResp.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(qResp.Frames))
	}

	frame := qResp.Frames[0]
	if len(frame.Fields) != len(columns) {
		t.Fatalf("expected %d fields, got %d", len(columns), len(frame.Fields))
	}

	if frame.Fields[0].Len() != 1 || frame.Fields[1].Len() != 1 {
		t.Fatal("expected each field to have 1 value")
	}
	if frame.Fields[0].At(0) != "value1" || frame.Fields[1].At(0) != "value2" {
		t.Fatalf("unexpected field values: got %v and %v", frame.Fields[0].At(0), frame.Fields[1].At(0))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestQueryData_InvalidPayload(t *testing.T) {
	ds := &Datasource{}
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{
			{RefID: "A", JSON: []byte(`{"queryText": 123, "format": "table"}`)},
		},
	}
	resp, err := ds.QueryData(context.Background(), req)
	if err != nil {
		t.Fatal("unexpected error:", err)
	}

	qResp, ok := resp.Responses["A"]
	if !ok {
		t.Fatal("missing response for RefID A")
	}

	if qResp.Status != backend.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest error, got status %d", qResp.Status)
	}
}

func TestValidateQuery(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
		errMsg  string
	}{
		{"simple select", "SELECT * FROM t", false, ""},
		{"select with where", "SELECT id, name FROM users WHERE updated_at > '2024-01-01'", false, ""},
		{"select with count", "SELECT COUNT(*) FROM orders", false, ""},
		{"column named updated_at", "SELECT updated_at FROM logs", false, ""},
		{"column named inserted_at", "SELECT inserted_at FROM logs", false, ""},
		{"column named deleted_at", "SELECT deleted_at FROM logs", false, ""},
		{"column with insert in name", "SELECT insertion_date FROM events", false, ""},
		{"insert statement", "INSERT INTO t VALUES (1)", true, "only SELECT queries are allowed"},
		{"update statement", "UPDATE t SET a=1", true, "only SELECT queries are allowed"},
		{"delete statement", "DELETE FROM t", true, "only SELECT queries are allowed"},
		{"drop statement", "DROP TABLE t", true, "only SELECT queries are allowed"},
		{"alter statement", "ALTER TABLE t ADD COLUMN a INT", true, "only SELECT queries are allowed"},
		{"invalid sql", "NOT VALID SQL ???", true, "query parse error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuery(tt.query)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateQuery(%q) error = %v, wantErr %v", tt.query, err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateQuery(%q) error = %q, want to contain %q", tt.query, err.Error(), tt.errMsg)
			}
		})
	}
}
