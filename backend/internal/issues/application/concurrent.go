package application

import (
	"context"
	"errors"
	"sync"
)

// maxConcurrentCalls bounds parallel GitHub calls on one user token; GitHub meets bursts with secondary rate limits.
const maxConcurrentCalls = 3

// MapConcurrent runs fn over items with at most maxConcurrentCalls in flight and returns the results in item order.
// The first failure cancels the rest. An error with Unauthorized() == true (a *RemoteError or a raw GitHub client
// error) wins over every other failure, because it revokes the credential; otherwise a real error wins over the
// context.Canceled of its cancelled siblings.
func MapConcurrent[T, R any](ctx context.Context, items []T, fn func(context.Context, T) (R, error)) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]R, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrentCalls)
	for i, item := range items {
		// Slots are taken in item order, so a failure cancels every item still waiting for one.
		// A free slot always wins, so up to maxConcurrentCalls items run even after a sibling failed (a 401 among
		// them still takes precedence); only an item that must wait for a slot is skipped once the batch is cancelled.
		select {
		case slots <- struct{}{}:
		default:
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				continue
			}
		}
		wg.Add(1)
		go func(i int, item T) {
			defer wg.Done()
			defer func() { <-slots }()
			res, err := fn(ctx, item)
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			results[i] = res
		}(i, item)
	}
	wg.Wait()
	var firstErr error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if unauthorized(err) {
			return nil, err
		}
		if firstErr == nil || (errors.Is(firstErr, context.Canceled) && !errors.Is(err, context.Canceled)) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// unauthorized reports whether err is a GitHub 401, as a *RemoteError or a raw client error.
func unauthorized(err error) bool {
	var u interface{ Unauthorized() bool }
	return errors.As(err, &u) && u.Unauthorized()
}
