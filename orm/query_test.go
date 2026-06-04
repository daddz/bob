package orm_test

import (
	"context"
	"io"
	"testing"

	"github.com/stephenafamo/scan"

	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/clause"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/dialect"
	"github.com/stephenafamo/bob/dialect/psql/sm"
	"github.com/stephenafamo/bob/orm"
	testutils "github.com/stephenafamo/bob/test/utils"
)

type rawExpr struct{ sql string }

func (r rawExpr) WriteSQL(_ context.Context, w io.StringWriter, _ bob.Dialect, _ int) ([]any, error) {
	w.WriteString(r.sql)
	return nil, nil
}

type (
	intSliceTransformer = bob.SliceTransformer[int, []int]
	todoModQuery        = orm.ModQuery[*dialect.SelectQuery, rawExpr, int, []int, intSliceTransformer]
)

// modQuery builds a ModQuery whose generated Mod is mod, mirroring what the
// queries plugin emits. The raw expression is only used by the un-augmented
// path; .With() always rebuilds from mod via Build.
func modQuery(expr string, scanner scan.Mapper[int], mod func(*dialect.SelectQuery)) todoModQuery {
	return todoModQuery{
		Query: orm.Query[rawExpr, int, []int, intSliceTransformer]{
			ExecQuery: orm.ExecQuery[rawExpr]{
				BaseQuery: bob.BaseQuery[rawExpr]{
					Expression: rawExpr{sql: expr},
					Dialect:    dialect.Dialect,
					QueryType:  bob.QueryTypeSelect,
				},
			},
			Scanner: scanner,
		},
		Mod:   bob.ModFunc[*dialect.SelectQuery](mod),
		Build: psql.Select,
	}
}

// newModQuery builds a ModQuery whose generated Mod alone reproduces
// "SELECT id FROM todo".
func newModQuery(scanner scan.Mapper[int]) todoModQuery {
	return modQuery("SELECT id FROM todo", scanner, func(q *dialect.SelectQuery) {
		q.AppendSelect(psql.Quote("id"))
		q.SetTable(psql.Quote("todo"))
	})
}

func TestModQueryWith(t *testing.T) {
	mq := newModQuery(nil)

	examples := testutils.Testcases{
		"base query from generated mod": {
			Doc:         "With() and no extra mods reproduces the base query from the generated Mod alone",
			ExpectedSQL: `SELECT id FROM todo`,
			Query:       mq.With(),
		},
		"augmented with extra mods": {
			Doc:          "Extra mods are appended on top of the generated Mod",
			ExpectedSQL:  `SELECT id FROM todo WHERE (project_id = $1) LIMIT 10`,
			ExpectedArgs: []any{1},
			Query: mq.With(
				sm.Where(psql.Quote("project_id").EQ(psql.Arg(1))),
				sm.Limit(10),
			),
		},
	}

	testutils.RunTests(t, examples, nil)
}

func TestModQueryWithPreservesScanner(t *testing.T) {
	scannerCalled := false
	scanner := func(context.Context, []string) (func(*scan.Row) (any, error), func(any) (int, error)) {
		scannerCalled = true
		return nil, nil
	}

	augmented := newModQuery(scanner).With()
	if augmented.Scanner == nil {
		t.Fatal("With() dropped the scanner")
	}

	augmented.Scanner(context.Background(), nil)
	if !scannerCalled {
		t.Fatal("With() did not preserve the original scanner")
	}
}

// TestModQueryWithClauseComposition covers augmenting a query that already
// carries ORDER BY / LIMIT / OFFSET in its main clause slots (as the queries
// plugin emits for a non-combined query). ORDER BY appends, LIMIT/OFFSET
// override, and a query that lacks a clause gains it.
func TestModQueryWithClauseComposition(t *testing.T) {
	// SELECT id FROM todo ORDER BY id LIMIT 5
	mq := modQuery("SELECT id FROM todo", nil, func(q *dialect.SelectQuery) {
		q.AppendSelect(psql.Quote("id"))
		q.SetTable(psql.Quote("todo"))
		q.OrderBy.AppendOrder(psql.Quote("id"))
		q.Limit.SetLimit(5)
	})

	examples := testutils.Testcases{
		"override existing limit": {
			Doc:         "sm.Limit replaces the query's own LIMIT instead of adding a second one",
			ExpectedSQL: `SELECT id FROM todo ORDER BY id LIMIT 2`,
			Query:       mq.With(sm.Limit(2)),
		},
		"append to existing order by": {
			Doc:         "sm.OrderBy appends a column to the query's existing ORDER BY",
			ExpectedSQL: `SELECT id FROM todo ORDER BY id, title LIMIT 5`,
			Query:       mq.With(sm.OrderBy(psql.Quote("title"))),
		},
		"add offset to query without one": {
			Doc:         "sm.Offset is added when the query has no OFFSET of its own",
			ExpectedSQL: `SELECT id FROM todo ORDER BY id LIMIT 5 OFFSET 3`,
			Query:       mq.With(sm.Offset(3)),
		},
		"compose where, order and limit": {
			Doc:          "Several mods of different kinds apply together",
			ExpectedSQL:  `SELECT id FROM todo WHERE user_id = $1 ORDER BY id, title LIMIT 2`,
			ExpectedArgs: []any{7},
			Query: mq.With(
				sm.Where(psql.Quote("user_id").EQ(psql.Arg(7))),
				sm.OrderBy(psql.Quote("title")),
				sm.Limit(2),
			),
		},
	}

	testutils.RunTests(t, examples, nil)
}

// TestModQueryWithArgRenumbering covers augmenting a parameterized query: a mod
// that inserts an argument ahead of the query's existing ones must renumber the
// placeholders and keep the args in matching order.
func TestModQueryWithArgRenumbering(t *testing.T) {
	// SELECT id FROM todo WHERE user_id = $1 ORDER BY id LIMIT $2 OFFSET $3
	mq := modQuery("SELECT id FROM todo", nil, func(q *dialect.SelectQuery) {
		q.AppendSelect(psql.Quote("id"))
		q.SetTable(psql.Quote("todo"))
		q.AppendWhere(psql.Quote("user_id").EQ(psql.Arg(1)))
		q.OrderBy.AppendOrder(psql.Quote("id"))
		q.Limit.SetLimit(psql.Arg(10))
		q.Offset.SetOffset(psql.Arg(0))
	})

	examples := testutils.Testcases{
		"base keeps original placeholders": {
			Doc:          "With() and no mods reproduces the parameterized query unchanged",
			ExpectedSQL:  `SELECT id FROM todo WHERE user_id = $1 ORDER BY id LIMIT $2 OFFSET $3`,
			ExpectedArgs: []any{1, 10, 0},
			Query:        mq.With(),
		},
		"inserted where renumbers later args": {
			Doc:          "An added WHERE arg takes $2 and shifts LIMIT/OFFSET to $3/$4, args stay in order",
			ExpectedSQL:  `SELECT id FROM todo WHERE user_id = $1 AND is_completed = $2 ORDER BY id LIMIT $3 OFFSET $4`,
			ExpectedArgs: []any{1, true, 10, 0},
			Query:        mq.With(sm.Where(psql.Quote("is_completed").EQ(psql.Arg(true)))),
		},
	}

	testutils.RunTests(t, examples, nil)
}

// TestModQueryWithCombinedQueryCaveat documents the limitation for queries
// that use a set operation (UNION/INTERSECT/EXCEPT). The plugin routes the
// outer ORDER BY/LIMIT/OFFSET to the Combined* slots so it applies to the whole
// result; sm.Limit/sm.OrderBy/sm.Offset target the main slots, which apply to
// the first branch only. Augmenting those clauses on a combined query is
// therefore almost never what you want.
func TestModQueryWithCombinedQueryCaveat(t *testing.T) {
	// SELECT id FROM todo UNION (SELECT id FROM archived_todo) LIMIT 5
	mq := modQuery("SELECT id FROM todo", nil, func(q *dialect.SelectQuery) {
		q.AppendSelect(psql.Quote("id"))
		q.SetTable(psql.Quote("todo"))
		q.AppendCombine(clause.Combine{
			Strategy: clause.Union,
			Query: bob.BaseQuery[bob.Expression]{
				Expression: rawExpr{sql: "SELECT id FROM archived_todo"},
				QueryType:  bob.QueryTypeSelect,
				Dialect:    dialect.Dialect,
			},
		})
		q.CombinedLimit.SetLimit(5)
	})

	examples := testutils.Testcases{
		"base limits the whole combined result": {
			Doc:         "Without augmentation the outer LIMIT applies to the combined result",
			ExpectedSQL: `SELECT id FROM todo UNION (SELECT id FROM archived_todo) LIMIT 5`,
			Query:       mq.With(),
		},
		"augmented limit applies to the first branch only": {
			Doc:         "CAVEAT: sm.Limit wraps and limits the first branch, the combined LIMIT 5 still applies to the whole",
			ExpectedSQL: `(SELECT id FROM todo LIMIT 2) UNION (SELECT id FROM archived_todo) LIMIT 5`,
			Query:       mq.With(sm.Limit(2)),
		},
	}

	testutils.RunTests(t, examples, nil)
}
