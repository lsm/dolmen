package postgres

import (
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	parser "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/lsm/dolmen/internal/store"
)

var queryFunctions = wordSet("abs ceil ceiling floor round trunc sqrt power exp ln log mod sign greatest least lower upper length char_length character_length octet_length trim btrim ltrim rtrim substring substr replace concat concat_ws left right strpos position reverse repeat split_part regexp_replace regexp_match regexp_matches regexp_split_to_array regexp_split_to_table format coalesce nullif count sum avg min max bool_and bool_or every string_agg array_agg json_agg jsonb_agg json_object_agg jsonb_object_agg json_build_object jsonb_build_object json_build_array jsonb_build_array json_array_length jsonb_array_length json_extract_path json_extract_path_text jsonb_extract_path jsonb_extract_path_text json_typeof jsonb_typeof json_array_elements json_array_elements_text jsonb_array_elements jsonb_array_elements_text json_each json_each_text jsonb_each jsonb_each_text to_json to_jsonb date_part date_trunc to_char to_date to_timestamp make_date make_time make_timestamp age now transaction_timestamp statement_timestamp clock_timestamp row_number rank dense_rank percent_rank cume_dist ntile lag lead first_value last_value nth_value generate_series unnest cardinality array_length array_position array_to_string string_to_array encode decode md5")
var queryTypes = wordSet("bool boolean int2 int4 int8 smallint integer bigint float4 float8 real double numeric decimal text varchar bpchar char name bytea json jsonb date time timetz timestamp timestamptz interval")
var queryOperators = wordSet("= <> != < <= > >= + - * / % || ~ ~* !~ !~* ~~ ~~* !~~ !~~* ^ & | # << >> -> ->> #> #>> @> <@ ? ?| ?& @? @@")
var queryMessages = wordSet("Node SelectStmt RangeVar RangeSubselect RangeFunction JoinExpr Alias ResTarget ColumnRef A_Star A_Const Integer Float String Boolean BitString ParamRef A_Expr BoolExpr NullTest BooleanTest TypeCast TypeName FuncCall CaseExpr CaseWhen CoalesceExpr MinMaxExpr SubLink List IntList OidList SortBy WindowDef GroupingSet RowExpr A_ArrayExpr A_Indices A_Indirection NamedArgExpr SQLValueFunction CollateClause CommonTableExpr WithClause")

func wordSet(words string) map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.Fields(words) {
		out[word] = true
	}
	return out
}
func sqlRejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(format, args...))
}

func builtinName(nodes []*pg.Node, allowed map[string]bool) bool {
	if len(nodes) == 2 {
		if nodes[0].GetString_().GetSval() != "pg_catalog" {
			return false
		}
		nodes = nodes[1:]
	}
	return len(nodes) == 1 && allowed[nodes[0].GetString_().GetSval()]
}

type sqlCompiler struct {
	namespace  string
	tables     map[string]tableState
	names      *sqlNames
	parameters int
}

func compileSQL(input string, argc int, namespace string, tables map[string]tableState) (string, *sqlNames, error) {
	names := newSQLNames(tables)
	rewritten, count, err := rewriteSQL(input, names)
	if err != nil {
		return "", nil, err
	}
	if count != argc {
		return "", nil, sqlRejected("SQL has %d placeholders but received %d arguments", count, argc)
	}
	tree, err := parser.Parse(rewritten)
	if err != nil {
		return "", nil, sqlRejected("invalid PostgreSQL SQL: %v", err)
	}
	if len(tree.Stmts) != 1 || tree.Stmts[0].Stmt.GetSelectStmt() == nil {
		return "", nil, sqlRejected("query accepts a single SELECT or read-only WITH statement")
	}
	compiler := sqlCompiler{namespace: namespace, tables: tables, names: names, parameters: argc}
	if err := compiler.walk(tree.Stmts[0].Stmt.ProtoReflect(), map[string]bool{}); err != nil {
		return "", nil, err
	}
	output, err := parser.Deparse(tree)
	return output, names, err
}

func (c *sqlCompiler) walk(message protoreflect.Message, ctes map[string]bool) error {
	if !queryMessages[string(message.Descriptor().Name())] {
		return sqlRejected("SQL construct %s is not supported in confined queries", message.Descriptor().Name())
	}
	skipWith := false
	switch node := message.Interface().(type) {
	case *pg.Node:
		if relation := node.GetRangeVar(); relation != nil {
			if relation.Catalogname != "" || relation.Schemaname != "" {
				return sqlRejected("query may reference only tables in its namespace")
			}
			if ctes[relation.Relname] {
				return nil
			}
			logical := c.names.original(relation.Relname)
			table, ok := c.tables[logical]
			if !ok {
				return fmt.Errorf("%w: table %s; use list_tables", store.ErrNotFound, logical)
			}
			c.names.tables = append(c.names.tables, table)
			cols := []string{ident("id"), ident("created_at")}
			for _, field := range table.schema.Fields {
				cols = append(cols, ident(table.columns[field.Name])+" AS "+ident(c.names.name(field.Name)))
			}
			generated, err := parser.Parse("SELECT " + strings.Join(cols, ",") + " FROM ONLY " + ident(c.namespace, table.physical))
			if err != nil {
				return err
			}
			alias := relation.Alias
			if alias == nil {
				alias = &pg.Alias{Aliasname: relation.Relname}
			}
			node.Node = &pg.Node_RangeSubselect{RangeSubselect: &pg.RangeSubselect{Subquery: generated.Stmts[0].Stmt, Alias: alias}}
			return nil
		}
	case *pg.SelectStmt:
		if node.IntoClause != nil || len(node.LockingClause) > 0 {
			return sqlRejected("SELECT INTO and row locking are not allowed")
		}
		local := map[string]bool{}
		for name, present := range ctes {
			local[name] = present
		}
		ctes = local
		if node.WithClause != nil {
			if node.WithClause.Recursive {
				for _, entry := range node.WithClause.Ctes {
					cte := entry.GetCommonTableExpr()
					if cte == nil {
						return sqlRejected("invalid CTE")
					}
					local[cte.Ctename] = true
				}
			}
			for _, entry := range node.WithClause.Ctes {
				cte := entry.GetCommonTableExpr()
				if cte == nil || cte.Ctequery.GetSelectStmt() == nil {
					return sqlRejected("WITH may contain only SELECT statements")
				}
				if err := c.walk(entry.ProtoReflect(), local); err != nil {
					return err
				}
				local[cte.Ctename] = true
			}
			skipWith = true
		}
	case *pg.FuncCall:
		if !builtinName(node.Funcname, queryFunctions) {
			return sqlRejected("function is not in the PostgreSQL query allowlist")
		}
		if len(node.Funcname) == 1 {
			node.Funcname = append([]*pg.Node{pg.MakeStrNode("pg_catalog")}, node.Funcname...)
		}
	case *pg.TypeName:
		if !builtinName(node.Names, queryTypes) || node.Setof {
			return sqlRejected("type is not in the PostgreSQL query allowlist")
		}
	case *pg.A_Expr:
		if !builtinName(node.Name, queryOperators) {
			return sqlRejected("operator is not in the PostgreSQL query allowlist")
		}
	case *pg.SortBy:
		if len(node.UseOp) > 0 && !builtinName(node.UseOp, queryOperators) {
			return sqlRejected("sort operator is not allowed")
		}
	case *pg.ColumnRef:
		if len(node.Fields) > 2 {
			return sqlRejected("column references may not qualify a schema or database")
		}
	case *pg.ParamRef:
		if node.Number < 1 || int(node.Number) > c.parameters {
			return sqlRejected("invalid SQL parameter")
		}
	case *pg.SQLValueFunction:
		if node.Op < pg.SQLValueFunctionOp_SVFOP_CURRENT_DATE || node.Op > pg.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N {
			return sqlRejected("session and catalog identity expressions are not allowed")
		}
	case *pg.CollateClause:
		if !builtinName(node.Collname, wordSet("C POSIX")) {
			return sqlRejected("only C and POSIX collations are allowed")
		}
	}
	var failure error
	message.Range(func(field protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if skipWith && string(field.Name()) == "with_clause" {
			return true
		}
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if err := c.walk(list.Get(i).Message(), ctes); err != nil {
					failure = err
					return false
				}
			}
		} else if err := c.walk(v.Message(), ctes); err != nil {
			failure = err
			return false
		}
		return true
	})
	return failure
}
