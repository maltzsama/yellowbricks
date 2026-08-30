package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "github.com/databricks/databricks-sql-go"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/maltzsama/yellow-bricks/pkg/models"
	"vitess.io/vitess/go/vt/sqlparser"
)

// Garante que Datasource implementa as interfaces necessárias
var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

type Datasource struct {
	DB     *sql.DB
	config *models.PluginSettings
}

func parseQueryParams(req *backend.CallResourceRequest) (url.Values, error) {
	u, err := url.Parse("http://dummy.io/" + req.URL)
	if err != nil {
		return nil, err
	}
	return u.Query(), nil
}

func validateQuery(query string) error {

	stmt, err := sqlparser.NewTestParser().Parse(query)
	if err != nil {
		return fmt.Errorf("query parse error: %w", err)
	}

	switch stmt.(type) {
	case *sqlparser.Select:
	default:
		return fmt.Errorf("only SELECT queries are allowed")
	}

	return nil
}

func wrapErr(msg string, err error) backend.DataResponse {
	return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("%s: %v", msg, err))
}

var validIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func validateIdentifier(name, label string) error {
	if !validIdentifier.MatchString(name) {
		return fmt.Errorf("invalid %s: must contain only letters, numbers, and underscores", label)
	}
	return nil
}

func (d *Datasource) contextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if d.config.Timeout > 0 {
		return context.WithTimeout(ctx, time.Duration(d.config.Timeout)*time.Second)
	}
	return ctx, func() {}
}

func NewDatasource(_ context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	config, err := models.LoadPluginSettings(settings)
	if err != nil {
		return nil, fmt.Errorf("fail to load config: %w", err)
	}

	connStr := fmt.Sprintf("token:%s@%s:443/%s", config.Token.Token, config.Host, config.Path)
	db, err := sql.Open("databricks", connStr)
	if err != nil {
		return nil, fmt.Errorf("fail to connect to Databricks: %w", err)
	}

	db.SetConnMaxLifetime(time.Duration(config.Timeout) * time.Second)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	return &Datasource{DB: db, config: config}, nil

}

func (d *Datasource) Dispose() {
	if d.DB != nil {
		d.DB.Close()
	}
}

func injectCatalogIntoQuery(catalog, rawQuery string) string {
	parser := sqlparser.NewTestParser()
	stmt, err := parser.Parse(rawQuery)
	if err != nil {
		return rawQuery
	}

	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return rawQuery
	}

	tables := extractTableNames(sel)
	result := rawQuery

	for _, tbl := range tables {
		qualifier := tbl.Qualifier
		name := tbl.Name

		var oldRef, newRef string
		switch {
		case qualifier == "":
			oldRef = name
			newRef = catalog + "." + name
		case qualifier == catalog || strings.HasPrefix(qualifier, catalog+"."):
			continue
		default:
			oldRef = qualifier + "." + name
			newRef = catalog + "." + qualifier + "." + name
		}

		result = injectBeforeTableRef(result, oldRef, newRef)
	}

	return result
}

type tableNameRef struct {
	Qualifier string
	Name      string
}

func extractTableNames(sel *sqlparser.Select) []tableNameRef {
	var tables []tableNameRef
	for _, expr := range sel.From {
		collectTableNames(expr, &tables)
	}
	return tables
}

func collectTableNames(expr sqlparser.TableExpr, tables *[]tableNameRef) {
	switch e := expr.(type) {
	case *sqlparser.AliasedTableExpr:
		if tbl, ok := e.Expr.(sqlparser.TableName); ok {
			*tables = append(*tables, tableNameRef{
				Qualifier: tbl.Qualifier.String(),
				Name:      tbl.Name.String(),
			})
		}
	case *sqlparser.JoinTableExpr:
		collectTableNames(e.LeftExpr, tables)
		collectTableNames(e.RightExpr, tables)
	case *sqlparser.ParenTableExpr:
		for _, inner := range e.Exprs {
			collectTableNames(inner, tables)
		}
	}
}

func injectBeforeTableRef(query, oldRef, newRef string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(oldRef) + `\b`)
	return re.ReplaceAllString(query, newRef)
}

func sanitizeQueryWithLimit(query string, maxRows int) (string, error) {
	cleanQuery := strings.TrimSpace(query)
	cleanQuery = strings.TrimSuffix(cleanQuery, ";")

	upperQuery := strings.ToUpper(cleanQuery)

	if strings.Contains(upperQuery, "LIMIT") {
		return fmt.Sprintf("%s;", cleanQuery), nil
	}

	limitedQuery := fmt.Sprintf("%s LIMIT %d;", cleanQuery, maxRows)

	return limitedQuery, nil
}

func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {

	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {

		res := d.query(ctx, req.PluginContext, q)
		response.Responses[q.RefID] = res
	}

	return response, nil
}

type sqlQueryPayload struct {
	RawSQL string `json:"queryText"`
	Format string `json:"format"`
}

func (d *Datasource) query(ctx context.Context, _ backend.PluginContext, query backend.DataQuery) backend.DataResponse {
	var response backend.DataResponse
	var qm sqlQueryPayload

	err := json.Unmarshal(query.JSON, &qm)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

	if qm.Format != "table" && qm.Format != "timeseries" {
		return backend.ErrDataResponse(backend.StatusBadRequest, "invalid format: must be 'table' or 'timeseries'")
	}

	if err := validateQuery(qm.RawSQL); err != nil {
		backend.Logger.Error("Query validation error", "query", qm.RawSQL, "error", err.Error())
		return backend.ErrDataResponse(backend.StatusBadRequest, err.Error())
	}

	catalog := d.config.Catalog
	adjustedQuery := injectCatalogIntoQuery(catalog, qm.RawSQL)

	if d.config.MaxRows > 0 {
		adjustedQuery, err = sanitizeQueryWithLimit(adjustedQuery, d.config.MaxRows)
		if err != nil {
			return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("failed to apply maxRows: %v", err))
		}
	}

	if d.config.Debug {
		backend.Logger.Debug("Running query", "query", adjustedQuery)
	}

	ctx, cancel := d.contextWithTimeout(ctx)
	defer cancel()

	rows, err := d.DB.QueryContext(ctx, adjustedQuery)
	if err != nil {
		return wrapErr("failed to execute query", err)

	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return wrapErr("failed to retrieve columns", err)

	}

	if strings.ToLower(qm.Format) == "timeseries" {
		hasTime := false
		for _, col := range cols {
			lowerCol := strings.ToLower(col)
			if lowerCol == "time" || strings.Contains(lowerCol, "timestamp") {
				hasTime = true
				break
			}
		}
		if !hasTime {
			return backend.ErrDataResponse(backend.StatusBadRequest, "db has no time column: time column is missing; make sure your data includes a time column for time series format or switch to a table format that doesn't require it")
		}
	}

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return wrapErr("failed to retrieve column types", err)
	}

	frame := data.NewFrame("response")
	for i, col := range cols {
		colType := strings.ToUpper(colTypes[i].DatabaseTypeName())
		switch {
		case strings.Contains(colType, "INT") || strings.Contains(colType, "BIGINT") || strings.Contains(colType, "SMALLINT"):
			frame.Fields = append(frame.Fields, data.NewField(col, nil, []int64{}))
		case colType == "FLOAT" || colType == "DOUBLE" || colType == "DECIMAL":
			frame.Fields = append(frame.Fields, data.NewField(col, nil, []float64{}))
		case colType == "BOOLEAN":
			frame.Fields = append(frame.Fields, data.NewField(col, nil, []bool{}))
		case colType == "TIMESTAMP" || colType == "DATE" || colType == "TIME" || colType == "TIMESTAMP_NTZ" || colType == "TIMESTAMP_LTZ":
			frame.Fields = append(frame.Fields, data.NewField(col, nil, []time.Time{}))
		default:
			frame.Fields = append(frame.Fields, data.NewField(col, nil, []string{}))
		}
	}

	scanDests := make([]interface{}, len(cols))
	for i := range scanDests {
		var v interface{}
		scanDests[i] = &v
	}

	for rows.Next() {
		if err := rows.Scan(scanDests...); err != nil {
			return wrapErr("failed to scan row", err)
		}
		for i, dest := range scanDests {
			v := *(dest.(*interface{}))
			colType := strings.ToUpper(colTypes[i].DatabaseTypeName())
			switch {
			case strings.Contains(colType, "INT") || strings.Contains(colType, "BIGINT") || strings.Contains(colType, "SMALLINT"):
				frame.Fields[i].Append(toInt64(v))
			case colType == "FLOAT" || colType == "DOUBLE" || colType == "DECIMAL":
				frame.Fields[i].Append(toFloat64(v))
			case colType == "BOOLEAN":
				frame.Fields[i].Append(toBool(v))
			case colType == "TIMESTAMP" || colType == "DATE" || colType == "TIME" || colType == "TIMESTAMP_NTZ" || colType == "TIMESTAMP_LTZ":
				frame.Fields[i].Append(toTime(v))
			default:
				frame.Fields[i].Append(fmt.Sprintf("%v", v))
			}
		}
	}

	response.Frames = append(response.Frames, frame)
	return response
}

func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if err := d.DB.PingContext(ctx); err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Falha na conexão: %v", err),
		}, nil
	}
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Conexão com Databricks está funcionando",
	}, nil
}

func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	catalog := d.config.Catalog
	if catalog == "" {
		return sendError(sender, backend.StatusBadRequest, "Catalog name is required")
	}

	switch req.Path {

	case "databases":

		databases, err := d.GetDatabases(ctx, catalog)
		if err != nil {
			return sendError(sender, backend.StatusInternal, err.Error())
		}
		return sendJSON(sender, databases)

	case "tables":

		u, err := parseQueryParams(req)
		if err != nil {
			return sendError(sender, backend.StatusBadRequest, "Invalid URL")
		}

		database := u.Get("database")

		if database == "" {
			return sendError(sender, backend.StatusBadRequest, "Database is required")
		}

		if err := validateIdentifier(database, "database"); err != nil {
			return sendError(sender, backend.StatusBadRequest, err.Error())
		}

		tables, err := d.GetTables(ctx, catalog, database)
		if err != nil {
			return sendError(sender, backend.StatusInternal, err.Error())
		}

		if len(tables) == 0 {
			return sendError(sender, backend.StatusNotFound, "No tables found for the selected database")
		}

		return sendJSON(sender, tables)

	case "columns":

		u, err := parseQueryParams(req)
		if err != nil {
			return sendError(sender, backend.StatusBadRequest, "Invalid URL")
		}

		database := u.Get("database")

		table := u.Get("table")
		if table == "" {
			return sendError(sender, backend.StatusBadRequest, "Table is required")
		}

		if err := validateIdentifier(database, "database"); err != nil {
			return sendError(sender, backend.StatusBadRequest, err.Error())
		}
		if err := validateIdentifier(table, "table"); err != nil {
			return sendError(sender, backend.StatusBadRequest, err.Error())
		}

		columns, err := d.GetColumns(ctx, catalog, database, table)
		if err != nil {
			return sendError(sender, backend.StatusInternal, err.Error())
		}
		return sendJSON(sender, columns)

	}

	return sendError(sender, backend.StatusNotFound, "Invalid endpoint")
}

func (d *Datasource) GetDatabases(ctx context.Context, catalog string) ([]string, error) {

	query := fmt.Sprintf("SHOW SCHEMAS IN %s", catalog)

	ctx, cancel := d.contextWithTimeout(ctx)
	defer cancel()

	rows, err := d.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	return databases, nil
}

func (d *Datasource) GetTables(ctx context.Context, catalog string, database string) ([]string, error) {

	query := fmt.Sprintf("SHOW TABLES IN %s.%s", catalog, database)

	ctx, cancel := d.contextWithTimeout(ctx)
	defer cancel()

	rows, err := d.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	var isTemporary string

	for rows.Next() {
		var table string
		if err := rows.Scan(&database, &table, &isTemporary); err != nil {
			return nil, err
		}
		tables = append(tables, table)

	}

	return tables, nil
}

func (d *Datasource) GetColumns(ctx context.Context, catalog string, database string, table string) ([]string, error) {

	query := fmt.Sprintf("DESCRIBE TABLE %s.%s.%s", catalog, database, table)
	if d.config.Debug {
		backend.Logger.Debug("Running DESCRIBE TABLE", "query", query)
	}

	ctx, cancel := d.contextWithTimeout(ctx)
	defer cancel()

	rows, err := d.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var columnName, columnType string
		var comment *string
		if err := rows.Scan(&columnName, &columnType, &comment); err != nil {
			backend.Logger.Error("Scan failed", "error", err)
			return nil, err
		}
		columns = append(columns, columnName)
	}

	if d.config.Debug {
		backend.Logger.Debug("Columns retrieved", "columns", columns)
	}
	return columns, nil
}

func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case float64:
		return int64(val)
	case bool:
		if val {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func toFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int64:
		return float64(val)
	case bool:
		if val {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func toBool(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val != 0
	case float64:
		return val != 0
	default:
		return false
	}
}

func toTime(v interface{}) time.Time {
	switch val := v.(type) {
	case time.Time:
		return val
	default:
		return time.Time{}
	}
}

func sendJSON(sender backend.CallResourceResponseSender, data interface{}) error {
	body, err := json.Marshal(data)
	if err != nil {
		return sendError(sender, backend.StatusInternal, "failed to marshal JSON")
	}

	return sender.Send(&backend.CallResourceResponse{Status: int(backend.StatusOK), Body: body})
}

func sendError(sender backend.CallResourceResponseSender, status backend.Status, message string) error {
	body := []byte(fmt.Sprintf(`{"error": "%s"}`, message))
	return sender.Send(&backend.CallResourceResponse{Status: int(status), Body: body})
}
