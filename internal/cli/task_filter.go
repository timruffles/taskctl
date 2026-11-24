package cli

import (
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"taskctl/internal/store"
)

func compileTaskFilter(expression string) (func(store.Task) (bool, error), error) {
	env, err := cel.NewEnv(
		cel.Variable("id", cel.StringType),
		cel.Variable("title", cel.StringType),
		cel.Variable("brief", cel.StringType),
		cel.Variable("status", cel.StringType),
		cel.Variable("tags", cel.ListType(cel.StringType)),
		cel.Variable("metadata", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("owner", cel.StringType),
		cel.Variable("prerequisites", cel.ListType(cel.StringType)),
		cel.Variable("origin_id", cel.StringType),
		cel.Variable("parent_id", cel.StringType),
		cel.Function("search", cel.Overload("search_string_string",
			[]*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
			cel.BinaryBinding(func(text, query ref.Val) ref.Val {
				return types.Bool(strings.Contains(
					strings.ToLower(string(text.(types.String))),
					strings.ToLower(string(query.(types.String))),
				))
			}))),
	)
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		return nil, usage("invalid --where expression: " + issues.Err().Error())
	}
	if ast.OutputType() != cel.BoolType {
		return nil, usage("--where expression must return a boolean")
	}
	program, err := env.Program(ast, cel.CostLimit(100_000))
	if err != nil {
		return nil, usage("invalid --where expression: " + err.Error())
	}
	return func(task store.Task) (bool, error) {
		result, _, err := program.Eval(map[string]any{
			"id": task.ID, "title": task.Title, "brief": task.Brief, "status": task.Status,
			"tags": task.Tags, "metadata": task.Metadata, "owner": task.Owner,
			"prerequisites": task.Prerequisites, "origin_id": task.OriginID, "parent_id": task.ParentID,
		})
		if err != nil {
			return false, usage(fmt.Sprintf("--where evaluation failed for %s: %v", task.ID, err))
		}
		return result == types.True, nil
	}, nil
}
