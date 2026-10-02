package opcore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// Database is the wrap-level `database <driver> { value ... }` declaration: a
// registered database/sql driver name and the chain its DSN resolves through.
type Database struct {
	Driver string
	DSN    guardfile.ValueChain
}

// IsZero reports whether no database was declared.
func (d Database) IsZero() bool { return d.Driver == "" }

// openDatabase resolves the DSN and opens the pool once. Once rather than per
// call because *sql.DB IS the pool; a rotated DSN needs a restart.
func (rt *Runtime) openDatabase(ctx context.Context) (*sql.DB, error) {
	rt.dbOnce.Do(func() {
		if rt.Database.IsZero() {
			rt.dbErr = sqlInternal(fmt.Errorf("no `database` declared for a `sql` grant"),
				"add `database <driver> { value ... }` to the wrap block")
			return
		}
		if !sqlDriverRegistered(rt.Database.Driver) {
			rt.dbErr = sqlInternal(
				fmt.Errorf("database/sql driver %q is not registered (registered: %s)", rt.Database.Driver, strings.Join(sql.Drivers(), ", ")),
				"import the driver for its side effect in the consumer binary, e.g. _ \"github.com/jackc/pgx/v5/stdlib\"")
			return
		}
		dsn, err := rt.resolveChain(ctx, rt.Database.DSN)
		if err != nil {
			rt.dbErr = err
			return
		}
		db, err := sql.Open(rt.Database.Driver, dsn)
		if err != nil {
			// The DSN can carry a password, so the driver's error is reported
			// without it: naming the driver is enough to act on.
			rt.dbErr = sqlInternal(fmt.Errorf("open %s database: %w", rt.Database.Driver, err),
				"check the DSN the declared value source resolves to")
			return
		}
		rt.db = db
	})
	return rt.db, rt.dbErr
}

func sqlDriverRegistered(name string) bool {
	for _, d := range sql.Drivers() {
		if d == name {
			return true
		}
	}
	return false
}

func sqlInternal(err error, advice string) error {
	return exitcode.New(exitcode.Internal, "internal", err, advice)
}

// executeSQL binds the declared parameters and runs the statement. Nothing is
// interpolated into the text, so an argument cannot become SQL.
func (o Operation) executeSQL(ctx context.Context, a Args) (Response, error) {
	spec := o.Desc.SQL
	args, err := bindSQLParams(spec, a.Body)
	if err != nil {
		return Response{}, err
	}
	db, err := o.RT.openDatabase(ctx)
	if err != nil {
		return Response{}, err
	}
	if spec.Reads {
		return querySQL(ctx, db, spec, args)
	}
	result, err := db.ExecContext(ctx, spec.Statement, args...)
	if err != nil {
		return Response{}, sqlUpstream(o.Desc.Leaf, err)
	}
	// Not every driver reports a count. That is a fact about the driver rather
	// than a failed call, so an unreported one stays null.
	out := map[string]any{"rows_affected": nil}
	if affected, aerr := result.RowsAffected(); aerr == nil {
		out["rows_affected"] = affected
	}
	return Response{Decoded: out, Status: "OK"}, nil
}

// bindSQLParams lowers declared parameters into positional driver arguments in
// declaration order. Only declared names are read, so an extra input is inert.
func bindSQLParams(spec *SQL, body map[string]any) ([]any, error) {
	out := make([]any, 0, len(spec.Params))
	for _, p := range spec.Params {
		value, present := body[p.Name]
		if !present {
			if p.Required {
				return nil, exitcode.New(exitcode.UserError, "user_error",
					fmt.Errorf("required parameter %q is missing", p.Name),
					"supply every required parameter this operation names")
			}
			out = append(out, nil)
			continue
		}
		if err := validateSQLParamValue(p, value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// validateSQLParamValue enforces the declared numeric bounds before binding.
func validateSQLParamValue(p Field, value any) error {
	numeric, ok := sqlNumericValue(value)
	if !ok {
		return nil
	}
	return validateNumericBounds(p, &numeric)
}

func sqlNumericValue(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// querySQL reads up to MaxRows rows and looks one past the bound, so a bounded
// read reports truncation as a fact rather than implying it saw everything.
func querySQL(ctx context.Context, db *sql.DB, spec *SQL, args []any) (Response, error) {
	rows, err := db.QueryContext(ctx, spec.Statement, args...)
	if err != nil {
		return Response{}, sqlUpstream("query", err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return Response{}, sqlUpstream("query", err)
	}
	out := make([]any, 0, spec.MaxRows)
	truncated := false
	for rows.Next() {
		if len(out) == spec.MaxRows {
			truncated = true
			break
		}
		row, serr := scanSQLRow(rows, columns)
		if serr != nil {
			return Response{}, sqlUpstream("scan", serr)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return Response{}, sqlUpstream("query", err)
	}
	return Response{
		Decoded: map[string]any{"rows": out, "truncated": truncated, "columns": columns},
		Status:  "OK",
	}, nil
}

// scanSQLRow reads one row into a column-keyed object, rendering the []byte a
// driver returns for text as a string so the result is JSON a model can read.
func scanSQLRow(rows *sql.Rows, columns []string) (map[string]any, error) {
	cells := make([]any, len(columns))
	into := make([]any, len(columns))
	for i := range cells {
		into[i] = &cells[i]
	}
	if err := rows.Scan(into...); err != nil {
		return nil, err
	}
	row := make(map[string]any, len(columns))
	for i, name := range columns {
		if raw, ok := cells[i].([]byte); ok {
			row[name] = string(raw)
			continue
		}
		row[name] = cells[i]
	}
	return row, nil
}

// sqlUpstream codes a database failure as an upstream failure, matching how an
// HTTP error status is reported rather than surfacing as an engine bug.
func sqlUpstream(what string, err error) error {
	return exitcode.New(exitcode.UpstreamFailed, "upstream_failed",
		fmt.Errorf("%s: %w", what, err),
		"check the statement against the database schema and the credential's grants")
}
