// Package coordinates defines the public table/key address contract shared by
// the HTTP boundary and replicated shard commands.
package coordinates

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	MaxTableBytes = 63
	MaxKeyBytes   = 1024
)

var ErrInvalid = errors.New("invalid table or key coordinates")

var reservedTables = map[string]struct{}{
	"_admin":       {},
	"_cdc":         {},
	"transactions": {},
}

func IsReservedTable(table string) bool {
	_, ok := reservedTables[table]
	return ok
}

// ValidTable is the exact reachable public table grammar. Reserved endpoint
// names are intentionally excluded even though HTTP recognizes them to return
// a stable reserved-table response.
func ValidTable(table string) bool {
	if table == "" || len(table) > MaxTableBytes || IsReservedTable(table) {
		return false
	}
	if table[0] < 'a' || table[0] > 'z' {
		return false
	}
	for _, value := range []byte(table[1:]) {
		if !((value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || value == '_' || value == '-') {
			return false
		}
	}
	return true
}

func ValidKey(key string) bool {
	return key != "" && len([]byte(key)) <= MaxKeyBytes && utf8.ValidString(key) && !strings.ContainsRune(key, '/')
}

func Validate(table, key string) error {
	if !ValidTable(table) || !ValidKey(key) {
		return ErrInvalid
	}
	return nil
}
