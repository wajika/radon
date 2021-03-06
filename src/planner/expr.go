/*
 * Radon
 *
 * Copyright 2018 The Radon Authors.
 * Code is licensed under the GPLv3.
 *
 */

package planner

import (
	"router"

	"github.com/pkg/errors"
	"github.com/xelabs/go-mysqlstack/sqlparser"
)

// getDMLRouting used to get the routing from the where clause.
func getDMLRouting(database, table, shardkey string, where *sqlparser.Where, router *router.Router) ([]router.Segment, error) {
	if shardkey != "" && where != nil {
		filters := splitAndExpression(nil, where.Expr)
		for _, filter := range filters {
			filter = skipParenthesis(filter)
			comparison, ok := filter.(*sqlparser.ComparisonExpr)
			if !ok {
				continue
			}

			// Only deal with Equal statement.
			switch comparison.Operator {
			case sqlparser.EqualStr:
				if nameMatch(comparison.Left, table, shardkey) {
					sqlval, ok := comparison.Right.(*sqlparser.SQLVal)
					if ok {
						return router.Lookup(database, table, sqlval, sqlval)
					}
				}
			}
		}
	}
	return router.Lookup(database, table, nil, nil)
}

func hasSubquery(node sqlparser.SQLNode) bool {
	has := false
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (kontinue bool, err error) {
		if _, ok := node.(*sqlparser.Subquery); ok {
			has = true
			return false, errors.New("dummy")
		}
		return true, nil
	}, node)
	return has
}

func nameMatch(node sqlparser.Expr, table, shardkey string) bool {
	colname, ok := node.(*sqlparser.ColName)
	return ok && (colname.Qualifier.Name.String() == "" || colname.Qualifier.Name.String() == table) && (colname.Name.String() == shardkey)
}

// isShardKeyChanging returns true if any of the update
// expressions modify a shardkey column.
func isShardKeyChanging(exprs sqlparser.UpdateExprs, shardkey string) bool {
	for _, assignment := range exprs {
		if shardkey == assignment.Name.Name.String() {
			return true
		}
	}
	return false
}

// splitAndExpression breaks up the Expr into AND-separated conditions
// and appends them to filters, which can be shuffled and recombined
// as needed.
func splitAndExpression(filters []sqlparser.Expr, node sqlparser.Expr) []sqlparser.Expr {
	if node == nil {
		return filters
	}
	switch node := node.(type) {
	case *sqlparser.AndExpr:
		filters = splitAndExpression(filters, node.Left)
		return splitAndExpression(filters, node.Right)
	case *sqlparser.ParenExpr:
		if node, ok := node.Expr.(*sqlparser.AndExpr); ok {
			return splitAndExpression(filters, node)
		}
	}
	return append(filters, node)
}

// skipParenthesis skips the parenthesis (if any) of an expression and
// returns the innermost unparenthesized expression.
func skipParenthesis(node sqlparser.Expr) sqlparser.Expr {
	if node, ok := node.(*sqlparser.ParenExpr); ok {
		return skipParenthesis(node.Expr)
	}
	return node
}

// checkComparison checks the WHERE or JOIN-ON clause contains non-sqlval comparison(t1.id=t2.id).
func checkComparison(expr sqlparser.Expr) error {
	filters := splitAndExpression(nil, expr)
	for _, filter := range filters {
		comparison, ok := filter.(*sqlparser.ComparisonExpr)
		if !ok {
			continue
		}
		if _, ok := comparison.Right.(*sqlparser.SQLVal); !ok {
			buf := sqlparser.NewTrackedBuffer(nil)
			comparison.Format(buf)
			return errors.Errorf("unsupported: [%s].must.be.value.compare", buf.String())
		}
	}
	return nil
}

// For example: select count(*), count(distinct x.a) as cstar, max(x.a) as mb, t.a as a1, x.b from t,x group by a1,b
// {field:count(*) referTables:{}   aggrFuc:count  aggrField:*   distinct:false}
// {field:cstar    referTables:{x}  aggrFuc:count  aggrField:*   distinct:true}
// {field:mb       referTables:{x}  aggrFuc:max    aggrField:x.a distinct:false}
// {field:a1       referTables:{t}  aggrFuc:}
// {field:b      referTables:{x}  aggrFuc:}
type selectTuple struct {
	//select expression
	expr sqlparser.SelectExpr
	//the field name of mysql returns
	field string
	//the referred tables
	referTables []string
	//aggregate function name
	aggrFuc string
	//field in the aggregate function
	aggrField string
	distinct  bool
}

// parserSelectExpr parses the AliasedExpr to select tuple.
func parserSelectExpr(expr *sqlparser.AliasedExpr) (*selectTuple, error) {
	funcName := ""
	aggrField := ""
	distinct := false
	referTables := make([]string, 0, 4)

	field := expr.As.String()
	if field == "" {
		if col, ok := expr.Expr.(*sqlparser.ColName); ok {
			field = col.Name.String()
		} else {
			buf := sqlparser.NewTrackedBuffer(nil)
			expr.Format(buf)
			field = buf.String()
		}
	}

	err := sqlparser.Walk(func(node sqlparser.SQLNode) (kontinue bool, err error) {
		switch node := node.(type) {
		case *sqlparser.ColName:
			tableName := node.Qualifier.Name.String()
			if tableName == "" {
				return true, nil
			}
			for _, tb := range referTables {
				if tb == tableName {
					return true, nil
				}
			}
			referTables = append(referTables, tableName)
		case *sqlparser.FuncExpr:
			distinct = node.Distinct
			if node.IsAggregate() {
				if node != expr.Expr {
					return false, errors.Errorf("unsupported: '%s'.contain.aggregate.in.select.exprs", field)
				}
				funcName = node.Name.String()
				if len(node.Exprs) != 1 {
					return false, errors.Errorf("unsupported: invalid.use.of.group.function[%s]", funcName)
				}
				buf := sqlparser.NewTrackedBuffer(nil)
				node.Exprs.Format(buf)
				aggrField = buf.String()
				if aggrField == "*" && node.Name.String() != "count" {
					return false, errors.Errorf("unsupported: syntax.error.at.'%s'", field)
				}
			}
		case *sqlparser.GroupConcatExpr:
			return false, errors.Errorf("unsupported: group_concat.in.select.exprs")
		case *sqlparser.Subquery:
			return false, errors.Errorf("unsupported: subqueries.in.select.exprs")
		}
		return true, nil
	}, expr.Expr)
	if err != nil {
		return nil, err
	}

	return &selectTuple{expr, field, referTables, funcName, aggrField, distinct}, nil
}

func parserSelectExprs(exprs sqlparser.SelectExprs) ([]selectTuple, error) {
	var tuples []selectTuple
	var err error
	for _, expr := range exprs {
		switch exp := expr.(type) {
		case *sqlparser.AliasedExpr:
			var tuple *selectTuple
			tuple, err = parserSelectExpr(exp)
			if err != nil {
				return nil, err
			}
			tuples = append(tuples, *tuple)
		case *sqlparser.StarExpr:
			tuple := selectTuple{expr: exp, field: "*"}
			if !exp.TableName.IsEmpty() {
				tuple.referTables = append(tuple.referTables, exp.TableName.Name.String())
			}
			tuples = append(tuples, tuple)
		case sqlparser.Nextval:
			return nil, errors.Errorf("unsupported: nextval.in.select.exprs")
		}
	}
	return tuples, nil
}

func checkInTuple(field, table string, tuples []selectTuple) bool {
	for _, tuple := range tuples {
		if tuple.field == "*" || tuple.field == field {
			if table == "" || len(tuple.referTables) == 0 {
				return true
			}
			if len(tuple.referTables) == 1 && tuple.referTables[0] == table {
				return true
			}
		}
	}
	return false
}

// decomposeAvg decomposes avg(a) to sum(a) and count(a).
func decomposeAvg(tuple *selectTuple) []*sqlparser.AliasedExpr {
	var ret []*sqlparser.AliasedExpr
	sum := &sqlparser.AliasedExpr{
		Expr: &sqlparser.FuncExpr{
			Name:  sqlparser.NewColIdent("sum"),
			Exprs: []sqlparser.SelectExpr{&sqlparser.AliasedExpr{Expr: sqlparser.NewValArg([]byte(tuple.aggrField))}},
		},
		As: sqlparser.NewColIdent(tuple.field),
	}
	count := &sqlparser.AliasedExpr{Expr: &sqlparser.FuncExpr{
		Name:  sqlparser.NewColIdent("count"),
		Exprs: []sqlparser.SelectExpr{&sqlparser.AliasedExpr{Expr: sqlparser.NewValArg([]byte(tuple.aggrField))}},
	}}
	ret = append(ret, sum, count)
	return ret
}
