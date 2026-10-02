package opcore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// recordingDriver is a real database/sql driver recording what the engine
// handed it, so "nothing is interpolated" is asserted rather than asserted about.
type recordingDriver struct {
	mu        sync.Mutex
	statement string
	args      []driver.Value
	rows      [][]driver.Value
	columns   []string
	affected  int64
}

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{d: d}, nil }

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(query string) (driver.Stmt, error) {
	return &recordingStmt{d: c.d, query: query}, nil
}
func (c *recordingConn) Close() error              { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) { return nil, io.EOF }

type recordingStmt struct {
	d     *recordingDriver
	query string
}

func (s *recordingStmt) Close() error  { return nil }
func (s *recordingStmt) NumInput() int { return -1 }

func (s *recordingStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.record(args)
	return driver.RowsAffected(s.d.affected), nil
}

func (s *recordingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.record(args)
	return &recordingRows{columns: s.d.columns, rows: s.d.rows}, nil
}

func (s *recordingStmt) record(args []driver.Value) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.statement = s.query
	s.d.args = append([]driver.Value(nil), args...)
}

type recordingRows struct {
	columns []string
	rows    [][]driver.Value
	at      int
}

func (r *recordingRows) Columns() []string { return r.columns }
func (r *recordingRows) Close() error      { return nil }
func (r *recordingRows) Next(dest []driver.Value) error {
	if r.at >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.at])
	r.at++
	return nil
}

var (
	registerOnce sync.Once
	testDriver   = &recordingDriver{}
)

func registerTestDriver(t *testing.T) *recordingDriver {
	t.Helper()
	registerOnce.Do(func() { sql.Register("opcoretest", testDriver) })
	testDriver.mu.Lock()
	defer testDriver.mu.Unlock()
	testDriver.statement, testDriver.args, testDriver.rows, testDriver.columns, testDriver.affected = "", nil, nil, nil, 0
	return testDriver
}

func sqlSpec(block string) string {
	return `wrap ward mcp analytics {
    database opcoretest { value literal "dsn://test" }
    can list orders {
` + block + `
    }
}`
}

func sqlDesc(t *testing.T, block string) opcore.Descriptor {
	t.Helper()
	descs, _, err := opcore.ParseInline([]byte(sqlSpec(block)))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	return descByLeaf(t, descs, "list")
}

func sqlOp(t *testing.T, block string) opcore.Operation {
	t.Helper()
	descs, cfg, err := opcore.ParseInline([]byte(sqlSpec(block)))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	cfg.Providers = valuesource.Merge(nil)
	return opcore.Operation{Desc: descByLeaf(t, descs, "list"), RT: opcore.NewRuntime(cfg)}
}

const ordersBlock = `        sql {
            statement "SELECT id, total FROM orders WHERE customer = $1 LIMIT $2"
            param "customer" type="string" required=#true
            param "limit" type="integer" minimum=1 maximum=100
        }`

// The ask, and the safety claim underneath it: the statement reaches the driver
// verbatim and the caller's values arrive as bound arguments, never as text.
func TestSQLBindsParametersRatherThanInterpolating(t *testing.T) {
	d := registerTestDriver(t)
	d.columns = []string{"id", "total"}
	d.rows = [][]driver.Value{{int64(1), int64(250)}, {int64(2), int64(90)}}

	resp, err := sqlOp(t, ordersBlock).Execute(context.Background(), opcore.Args{
		Body: map[string]any{"customer": "'; DROP TABLE orders; --", "limit": 10},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if d.statement != "SELECT id, total FROM orders WHERE customer = $1 LIMIT $2" {
		t.Fatalf("statement reached the driver altered: %q", d.statement)
	}
	if len(d.args) != 2 || d.args[0] != "'; DROP TABLE orders; --" {
		t.Fatalf("args = %#v, want the hostile value bound rather than interpolated", d.args)
	}
	out := resp.Decoded.(map[string]any)
	rows := out["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if first := rows[0].(map[string]any); first["id"] != int64(1) || first["total"] != int64(250) {
		t.Errorf("first row = %v", first)
	}
	if out["truncated"] != false {
		t.Errorf("truncated = %v, want false", out["truncated"])
	}
}

// The statement is authored, so it must not be reachable and an undeclared
// input must not bind.
func TestSQLKeepsTheStatementOutOfReach(t *testing.T) {
	desc := sqlDesc(t, ordersBlock)
	schema := desc.InputSchema()
	for _, reserved := range []string{"statement", "sql", "rows"} {
		if _, present := schema.Properties[reserved]; present {
			t.Errorf("schema exposes %q", reserved)
		}
	}
	if len(schema.Properties) != 2 {
		t.Fatalf("schema = %v, want exactly the two declared params", schema.Properties)
	}

	d := registerTestDriver(t)
	d.columns = []string{"id"}
	if _, err := sqlOp(t, ordersBlock).Execute(context.Background(), opcore.Args{
		Body: map[string]any{"customer": "c", "limit": 1, "injected": "x"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(d.args) != 2 {
		t.Fatalf("args = %#v, want only the two declared params", d.args)
	}
}

// Parameters surface as individually typed inputs with their declared bounds.
func TestSQLProjectsTypedParameters(t *testing.T) {
	schema := sqlDesc(t, ordersBlock).InputSchema()
	customer, ok := schema.Properties["customer"]
	if !ok || customer.Type != "string" || customer.Location != opcore.LocationBody {
		t.Fatalf("customer = %+v", customer)
	}
	limit := schema.Properties["limit"]
	if limit.Type != "integer" || limit.Minimum == nil || *limit.Minimum != 1 || *limit.Maximum != 100 {
		t.Fatalf("limit = %+v", limit)
	}
	if !contains(schema.Required, "customer") || contains(schema.Required, "limit") {
		t.Errorf("required = %v, want customer only", schema.Required)
	}
}

// A bounded read must not look complete. max-rows stops the scan and the
// response says so.
func TestSQLBoundsRowsAndSaysSo(t *testing.T) {
	d := registerTestDriver(t)
	d.columns = []string{"id"}
	for i := range 10 {
		d.rows = append(d.rows, []driver.Value{int64(i)})
	}
	resp, err := sqlOp(t, `        sql {
            statement "SELECT id FROM orders"
            max-rows "3"
        }`).Execute(context.Background(), opcore.Args{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := resp.Decoded.(map[string]any)
	if rows := out["rows"].([]any); len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true", out["truncated"])
	}
}

// A driver returns text as []byte, which would serialize as base64 JSON and be
// unreadable to a model.
func TestSQLRendersTextRatherThanBytes(t *testing.T) {
	d := registerTestDriver(t)
	d.columns = []string{"name"}
	d.rows = [][]driver.Value{{[]byte("ada")}}
	resp, err := sqlOp(t, `        sql {
            statement "SELECT name FROM people"
        }`).Execute(context.Background(), opcore.Args{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	row := resp.Decoded.(map[string]any)["rows"].([]any)[0].(map[string]any)
	if row["name"] != "ada" {
		t.Errorf("name = %#v, want the string", row["name"])
	}
}

// A writing statement reports rows affected rather than a row set.
func TestSQLWriteReportsRowsAffected(t *testing.T) {
	d := registerTestDriver(t)
	d.affected = 4
	src := `wrap ward mcp analytics {
    database opcoretest { value literal "dsn://test" }
    can update orders {
        sql {
            statement "UPDATE orders SET total = $1 WHERE customer = $2"
            param "total" type="integer" required=#true
            param "customer" type="string" required=#true
        }
    }
}`
	descs, cfg, err := opcore.ParseInline([]byte(src))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	cfg.Providers = valuesource.Merge(nil)
	op := opcore.Operation{Desc: descByLeaf(t, descs, "update"), RT: opcore.NewRuntime(cfg)}
	resp, err := op.Execute(context.Background(), opcore.Args{
		Body: map[string]any{"total": 1, "customer": "c"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := resp.Decoded.(map[string]any)["rows_affected"]; got != int64(4) {
		t.Errorf("rows_affected = %v, want 4", got)
	}
}

// An unregistered driver is a clear startup-shaped error naming it, not a
// silent failure on the first call.
func TestSQLNamesAnUnregisteredDriver(t *testing.T) {
	src := `wrap ward mcp analytics {
    database nosuchdriver { value literal "dsn://test" }
    can list orders {
        sql {
            statement "SELECT 1"
        }
    }
}`
	descs, cfg, err := opcore.ParseInline([]byte(src))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	cfg.Providers = valuesource.Merge(nil)
	op := opcore.Operation{Desc: descs[0], RT: opcore.NewRuntime(cfg)}
	_, err = op.Execute(context.Background(), opcore.Args{})
	if err == nil {
		t.Fatalf("Execute succeeded with no driver registered")
	}
	if !strings.Contains(err.Error(), "nosuchdriver") || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error = %q, want it to name the driver", err)
	}
}

// A missing required parameter is refused before the statement runs.
func TestSQLEnforcesRequiredParameters(t *testing.T) {
	registerTestDriver(t)
	_, err := sqlOp(t, ordersBlock).Execute(context.Background(), opcore.Args{
		Body: map[string]any{"limit": 1},
	})
	if err == nil {
		t.Fatalf("Execute accepted a call missing a required parameter")
	}
	if !strings.Contains(err.Error(), "customer") {
		t.Fatalf("error = %q, want it to name customer", err)
	}
}

func TestSQLFailsClosedAtBuild(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"no statement": {`        sql {
        }`, "needs a `statement"},
		"blank statement": {`        sql {
            statement "   "
        }`, "must be non-empty"},
		"unknown child": {`        sql {
            statement "SELECT 1"
            rows "3"
        }`, "unknown `sql` child"},
		"stacked statements": {`        sql {
            statement "SELECT 1; DROP TABLE orders"
        }`, "more than one statement"},
		"placeholder count": {`        sql {
            statement "SELECT id FROM orders WHERE a = $1 AND b = $2"
            param "a" type="string"
        }`, "2 placeholder(s) but 1 `param`"},
		"gap in placeholders": {`        sql {
            statement "SELECT id FROM orders WHERE a = $1 AND b = $3"
            param "a" type="string"
            param "b" type="string"
        }`, "they must be $1..$N"},
		"param without type": {`        sql {
            statement "SELECT id FROM orders WHERE a = $1"
            param "a"
        }`, "needs a `type="},
		"non-binding type": {`        sql {
            statement "SELECT id FROM orders WHERE a = $1"
            param "a" type="object"
        }`, "does not bind as a parameter"},
		"duplicate param": {`        sql {
            statement "SELECT id FROM orders WHERE a = $1 AND b = $2"
            param "a" type="string"
            param "a" type="string"
        }`, "duplicate `param`"},
		"reading verb writes": {`        sql {
            statement "DELETE FROM orders"
        }`, "reading verb but the statement mutates"},
		"cte hides a write": {`        sql {
            statement "WITH gone AS (DELETE FROM orders RETURNING id) SELECT id FROM gone"
        }`, "reading verb but the statement mutates"},
		"combined with path": {`        path "/orders"
        sql {
            statement "SELECT 1"
        }`, "cannot be combined with `path`"},
		"combined with graphql": {`        sql {
            statement "SELECT 1"
        }
        graphql {
            document "query { a }"
        }`, "cannot be combined with `graphql`"},
		"max-rows not a number": {`        sql {
            statement "SELECT 1"
            max-rows "lots"
        }`, "must be a whole number"},
		"max-rows over ceiling": {`        sql {
            statement "SELECT 1"
            max-rows "99999"
        }`, "must be between 1 and 10000"},
		"duplicate sql block": {`        sql {
            statement "SELECT 1"
        }
        sql {
            statement "SELECT 2"
        }`, "duplicate `sql`"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := opcore.ParseInline([]byte(sqlSpec(tc.block)))
			if err == nil {
				t.Fatalf("ParseInline accepted %q", tc.block)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A `;` or a `?` inside a literal is data, not syntax, so a legitimate
// statement carrying one must still parse.
func TestSQLScannerIgnoresLiteralsAndComments(t *testing.T) {
	desc := sqlDesc(t, `        sql {
            statement "-- a leading comment\nSELECT id FROM orders WHERE note = 'a ; and a ? inside' AND customer = $1"
            param "customer" type="string"
        }`)
	if desc.SQL == nil || len(desc.SQL.Params) != 1 {
		t.Fatalf("sql = %+v", desc.SQL)
	}
}

// A wrap serving only sql grants has no HTTP upstream to authenticate to.
func TestSQLWrapNeedsNoAuth(t *testing.T) {
	if _, _, err := opcore.ParseInline([]byte(sqlSpec(`        sql {
            statement "SELECT 1"
        }`))); err != nil {
		t.Fatalf("a sql-only wrap required auth: %v", err)
	}
}

// A sql grant with no wrap-level database is a build error rather than a
// runtime surprise.
func TestSQLNeedsADatabaseDeclaration(t *testing.T) {
	src := `wrap ward mcp analytics {
    can list orders {
        sql {
            statement "SELECT 1"
        }
    }
}`
	_, _, err := opcore.ParseInline([]byte(src))
	if err == nil {
		t.Fatalf("ParseInline accepted a sql grant with no database")
	}
	if !strings.Contains(err.Error(), "wrap-level `database") {
		t.Fatalf("error = %q", err)
	}
}

// The DSN resolves through the shared value registry, so SSM and env work the
// way every other credential in this engine does.
func TestSQLResolvesTheDSNThroughValueSources(t *testing.T) {
	registerTestDriver(t)
	descs, cfg, err := opcore.ParseInline([]byte(`wrap ward mcp analytics {
    database opcoretest { value env "OPCORE_TEST_DSN" }
    can list orders {
        sql {
            statement "SELECT 1"
        }
    }
}`))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	if cfg.Database.Driver != "opcoretest" {
		t.Fatalf("driver = %q", cfg.Database.Driver)
	}
	if got := cfg.Database.DSN.String(); got != "env OPCORE_TEST_DSN" {
		t.Fatalf("dsn chain = %q", got)
	}
	_ = descs
}
