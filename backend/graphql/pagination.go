package graph

import (
	"fmt"
	"strconv"
)

func pageLimit(first int) (int, error) {
	if first < 1 || first > 100 {
		return 0, fmt.Errorf("first must be between 1 and 100")
	}
	return first, nil
}
func cursor(value *string) (int64, error) {
	if value == nil {
		return 0, nil
	}
	n, err := strconv.ParseInt(*value, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return n, nil
}
