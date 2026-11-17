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

type depthLimit struct{}

func (depthLimit) ExtensionName() string                   { return "DepthLimit" }
func (depthLimit) Validate(graphql.ExecutableSchema) error { return nil }
func (depthLimit) MutateOperationContext(_ context.Context, op *graphql.OperationContext) *gqlerror.Error {
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
	srv := handler.New(NewExecutableSchema(cfg))
	srv.AddTransport(transport.POST{})
	srv.Use(depthLimit{})
	srv.Use(extension.FixedComplexityLimit(maxQueryComplexity))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		srv.ServeHTTP(w, r)
	})
}
