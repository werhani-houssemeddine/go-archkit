package pg

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSON stores any value in a json/jsonb column: use it in Mapping.Values and Scan.
//
//	"lines": pg.JSON[[]Line]{V: o.Lines}   // write
//	var lines pg.JSON[[]Line]; s.Scan(&lines); o.Lines = lines.V   // read
type JSON[T any] struct{ V T }

// Value implements driver.Valuer.
func (j JSON[T]) Value() (driver.Value, error) {
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (j *JSON[T]) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		var zero T
		j.V = zero
		return nil
	case []byte:
		return json.Unmarshal(v, &j.V)
	case string:
		return json.Unmarshal([]byte(v), &j.V)
	default:
		return fmt.Errorf("pg.JSON: cannot scan %T", src)
	}
}
