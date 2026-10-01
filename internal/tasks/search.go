package tasks

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

// SQLite's LOWER folds only ASCII, so text search lowers with Go's Unicode-aware ToLower.
func init() {
	if err := sqlite.RegisterDeterministicScalarFunction("unicode_lower", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if s, ok := args[0].(string); ok {
				return strings.ToLower(s), nil
			}
			return args[0], nil
		}); err != nil {
		panic(err)
	}
}
