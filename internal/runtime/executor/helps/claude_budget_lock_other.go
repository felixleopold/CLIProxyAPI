//go:build !unix

package helps

import "fmt"

func lockClaudeBudget(string) (func(), bool, error) {
	return nil, false, fmt.Errorf("Claude API budgets require a Unix local filesystem with flock and directory fsync")
}
