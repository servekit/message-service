// Package models defines GORM model structs for message-service tables.
package models

import (
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/servekit/go-common/jsonx"
	"gorm.io/cli/gorm/field"
	"gorm.io/cli/gorm/genconfig"
)

// MapStringString is a JSONB-compatible map for template parameters.
type MapStringString map[string]string

// Scan implements sql.Scanner for JSONB.
func (m *MapStringString) Scan(value any) error {
	if value == nil {
		*m = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to unmarshal JSONB value: %v", value)
	}
	return jsonx.Unmarshal(bytes, m)
}

// Value implements driver.Valuer for JSONB.
func (m MapStringString) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return jsonx.Marshal(m)
}

// StringSlice is a JSONB-compatible string slice for list fields like Cc/Bcc.
type StringSlice []string

// Scan implements sql.Scanner for JSONB.
func (s *StringSlice) Scan(value any) error {
	if value == nil {
		*s = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to unmarshal JSONB value: %v", value)
	}
	return jsonx.Unmarshal(bytes, s)
}

// Value implements driver.Valuer for JSONB.
func (s StringSlice) Value() (driver.Value, error) {
	if s == nil {
		return nil, nil
	}
	return jsonx.Marshal(s)
}

// gorm gen field-type mappings for types that don't map cleanly to a built-in
// SQL/GORM type. Referenced by `gorm gen -i ./internal/store/models
// -o ./internal/store/generated` (Makefile target `generate`).
//
// Declared as `var _ = ...` (the form documented in genconfig.Config's doc
// comment) for two reasons:
//
//  1. gorm gen discovers this Config by scanning the package AST — it never
//     evaluates the map at runtime. The literal value is unused.
//  2. A named var forces the Go compiler to evaluate the map literal at init
//     time, which panics because MapStringString (alias for map[string]string)
//     is unhashable as a map key. The blank-identifier form lets the compiler
//     dead-code-eliminate the literal.
var _ = genconfig.Config{
	OutPath: "internal/store/generated",

	FieldTypeMap: map[any]any{
		sql.NullTime{}:    field.Time{},
		MapStringString{}: field.Field[map[string]string]{},
	},
}

// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&MessageEmailRecord{},
		&MessageSMSRecord{},
		&MessageEmailRecordAttachment{},
	}
}
