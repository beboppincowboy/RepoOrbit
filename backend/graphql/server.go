package graph

import (
	"context"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type operationLimits struct{}

func (operationLimits) ExtensionName() string                   { return "OperationLimits" }
func (operationLimits) Validate(graphql.ExecutableSchema) error { return nil }
func (operationLimits) MutateOperationContext(_ context.Context, op *graphql.OperationContext) *gqlerror.Error {
	var depth func(ast.SelectionSet, int) bool
	depth = func(set ast.SelectionSet, level int) bool {
		if level > maxQueryDepth {
			return false
		}
		for _, s := range set {
			switch v := s.(type) {
			case *ast.Field:
				if !depth(v.SelectionSet, level+1) {
					return false
				}
			case *ast.InlineFragment:
				if !depth(v.SelectionSet, level+1) {
					return false
				}
			case *ast.FragmentSpread:
				if v.Definition != nil && !depth(v.Definition.SelectionSet, level+1) {
					return false
				}
			}
		}
		return true
	}
	if !depth(op.Operation.SelectionSet, 1) {
		return gqlerror.Errorf("query depth exceeds limit")
	}
	overview := false
	for _, field := range graphql.CollectFields(op, op.Operation.SelectionSet, []string{"Query"}) {
		if field.Name != "repositoryOverview" && field.Name != "crossRepositoryOverview" {
			continue
		}
		if overview {
			return gqlerror.Errorf("request at most one repository overview")
		}
		overview = true
	}
	return nil
}

// Server serves POST /graphql with depth, complexity and body-size (1 MiB) limits and no-store caching.
// Mount it behind auth.Handler.Protect.
func Server(resolver *Resolver) http.Handler {
	cfg := Config{Resolvers: resolver}
	weight := func(child, first int) int {
		if first < 1 || first > maxPageSize {
			return 100000
		}
		return 1 + first*child
	}
	cfg.Complexity.Query.SelectedRepositories = func(child, first int, _ *string) int { return weight(child, first) }
	cfg.Complexity.Query.Issues = func(child int, ids []string, first int, _ *string) int {
		if len(ids) > maxRepositoryIDs {
			return 100000
		}
		return weight(child, first)
	}
	cfg.Complexity.Query.RepositoryOverview = func(child int, _ string, _ int) int {
		return 100 + child
	}
	cfg.Complexity.RepositoryOverview.Contributors = func(child int) int { return 25 * child }
	cfg.Complexity.Query.CrossRepositoryOverview = func(child int, _ int) int { return 100 + child }
	cfg.Complexity.CrossRepositoryOverview.NeedsAttention = func(child int) int { return 25 * child }
	cfg.Complexity.CrossRepositoryOverview.AssignedToMe = func(child int) int { return 25 * child }
	cfg.Complexity.CrossRepositoryOverview.Daily = func(child int) int { return 90 * child }
	cfg.Complexity.RepositoryOverview.Timeline = func(child int) int { return 50 * child }
	cfg.Complexity.RepositoryOverview.Daily = func(child int) int { return 90 * child }
	srv := handler.New(NewExecutableSchema(cfg))
	srv.AddTransport(transport.POST{})
	srv.Use(operationLimits{})
	srv.Use(extension.FixedComplexityLimit(maxQueryComplexity))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		srv.ServeHTTP(w, r)
	})
}
